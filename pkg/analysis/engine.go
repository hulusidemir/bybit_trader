package analysis

import (
	"log"
	"math"
	"strings"

	"bybit_trader/pkg/exchange/binance"
	"bybit_trader/pkg/exchange/bybit"
	"bybit_trader/pkg/models"
)

// isInvalidSymbolErr checks if the error is a Binance invalid symbol error (-1121)
func isInvalidSymbolErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "-1121") || strings.Contains(err.Error(), "Invalid symbol"))
}

type Engine struct {
	bybit   *bybit.Client
	binance *binance.Client
}

func NewEngine(b *bybit.Client, bn *binance.Client) *Engine {
	return &Engine{bybit: b, binance: bn}
}

// timeframe mapping: analysis key -> bybit OI interval, bybit kline interval, binance period
var timeframeMap = map[string]struct {
	OIInterval     string // bybit OI
	KlineInterval  string // bybit kline
	BinancePeriod  string // binance taker vol / LS ratio
	SpotKlineInt   string // binance spot kline
	LSPeriod       string // bybit account ratio
}{
	"5":   {OIInterval: "5min", KlineInterval: "5", BinancePeriod: "5m", SpotKlineInt: "5m", LSPeriod: "5min"},
	"15":  {OIInterval: "15min", KlineInterval: "15", BinancePeriod: "15m", SpotKlineInt: "15m", LSPeriod: "15min"},
	"60":  {OIInterval: "1h", KlineInterval: "60", BinancePeriod: "1h", SpotKlineInt: "1h", LSPeriod: "1h"},
	"240": {OIInterval: "4h", KlineInterval: "240", BinancePeriod: "4h", SpotKlineInt: "4h", LSPeriod: "4h"},
}

// AnalyzeCoin performs full multi-timeframe analysis for a single coin
func (e *Engine) AnalyzeCoin(coin *models.Coin) *models.CoinAnalysis {
	analysis := &models.CoinAnalysis{
		Symbol:      coin.Symbol,
		Metrics:     make(map[string]*models.TimeframeMetrics),
		LastPrice:   coin.LastPrice,
		Volume24h:   coin.Turnover24h,
		FundingRate: coin.FundingRate,
	}

	// Fetch orderbook once (doesn't depend on timeframe)
	ob, err := e.bybit.FetchOrderbook(coin.Symbol, 50)
	if err != nil {
		log.Printf("[%s] orderbook fetch error: %v", coin.Symbol, err)
		ob = &models.OrderbookSnapshot{}
	}

	for tf, mapping := range timeframeMap {
		metrics := &models.TimeframeMetrics{
			Timeframe:       tf,
			LastPrice:       coin.LastPrice,
			Volume24h:       coin.Turnover24h,
			FundingRate:     coin.FundingRate,
			NextFundingTime: coin.NextFundingTime,
			FundingInterval: coin.FundingInterval,
		}

		// ── Open Interest ────────────────────────────────
		oiData, err := e.bybit.FetchOpenInterest(coin.Symbol, mapping.OIInterval, 50)
		if err != nil {
			log.Printf("[%s][%s] OI fetch error: %v", coin.Symbol, tf, err)
		} else {
			metrics.OIChange = calcPercentChange(oiData)
			metrics.OITrend = classifyTrend(metrics.OIChange, 2.0, 5.0)
		}

		// ── Perp CVD (from Binance futures kline taker volume) ──
		futuresSymbol := binance.BybitToFuturesSymbol(coin.Symbol)
		if binance.IsFuturesSymbolValid(futuresSymbol) {
			takerVol, err := e.binance.FetchTakerBuySellVolume(futuresSymbol, mapping.BinancePeriod, 30)
			if err != nil {
				if isInvalidSymbolErr(err) {
					binance.MarkFuturesInvalid(futuresSymbol)
					log.Printf("[%s] Binance futures verisi yok -> Sadece Bybit verileriyle taranmaya devam ediliyor", coin.Symbol)
				} else {
					log.Printf("[%s][%s] perp taker vol fetch error: %v", coin.Symbol, tf, err)
				}
			} else {
				metrics.PerpCVD = calcCVD(takerVol)
				metrics.PerpCVDTrend = classifyCVDTrend(metrics.PerpCVD, coin.Turnover24h)
			}
		}

		// ── Spot CVD (from Binance spot kline taker volume) ────
		spotSymbol := binance.BybitToSpotSymbol(coin.Symbol)
		if binance.IsSpotSymbolValid(spotSymbol) {
			spotTakerVol, err := e.binance.FetchSpotTakerVolume(spotSymbol, mapping.SpotKlineInt, 30)
			if err != nil {
				if isInvalidSymbolErr(err) {
					binance.MarkSpotInvalid(spotSymbol)
					log.Printf("[%s] Binance spot verisi yok -> Sadece Bybit verileriyle taranmaya devam ediliyor", coin.Symbol)
				} else {
					log.Printf("[%s][%s] spot taker vol fetch error: %v", coin.Symbol, tf, err)
				}
			} else {
				metrics.SpotCVD = calcCVD(spotTakerVol)
				metrics.SpotCVDTrend = classifyCVDTrend(metrics.SpotCVD, coin.Turnover24h)
			}
		}

		// ── Orderbook Analysis ───────────────────────────
		if len(ob.Bids) > 0 && len(ob.Asks) > 0 {
			metrics.OBImbalance = calcOBImbalance(ob)
			metrics.OBBias = classifyOBBias(metrics.OBImbalance)
			bidWallP, bidWallS := findWall(ob.Bids)
			askWallP, askWallS := findWall(ob.Asks)
			metrics.BidWallPrice = bidWallP
			metrics.BidWallSize = bidWallS
			metrics.AskWallPrice = askWallP
			metrics.AskWallSize = askWallS
		}

		// ── ATR (14-period from Bybit klines) ────────────
		klines, err := e.bybit.FetchKline(coin.Symbol, mapping.KlineInterval, 15)
		if err != nil {
			log.Printf("[%s][%s] kline fetch error: %v", coin.Symbol, tf, err)
		} else {
			metrics.ATR = calcATR(klines, 14)
		}

		// ── Long/Short Ratio ─────────────────────────────
		lsData, err := e.bybit.FetchLongShortRatio(coin.Symbol, mapping.LSPeriod, 10)
		if err != nil {
			log.Printf("[%s][%s] L/S ratio fetch error: %v", coin.Symbol, tf, err)
		} else if len(lsData) > 0 {
			metrics.LSRatio = lsData[len(lsData)-1].Ratio
		}

		analysis.Metrics[tf] = metrics
	}

	return analysis
}

// ── Helper Functions ────────────────────────────────────────

func calcPercentChange(data []models.OpenInterestPoint) float64 {
	if len(data) < 2 {
		return 0
	}
	first := data[0].OpenInterest
	last := data[len(data)-1].OpenInterest
	if first == 0 {
		return 0
	}
	return ((last - first) / first) * 100
}

func classifyTrend(change, moderate, strong float64) models.Trend {
	abs := math.Abs(change)
	if abs < moderate {
		return models.TrendNeutral
	}
	if change > 0 {
		if abs >= strong {
			return models.TrendStrongUp
		}
		return models.TrendUp
	}
	if abs >= strong {
		return models.TrendStrongDown
	}
	return models.TrendDown
}

func calcCVD(data []models.TakerVolume) float64 {
	cvd := 0.0
	for _, d := range data {
		cvd += d.BuyVolume - d.SellVolume
	}
	return cvd
}

func calcSpotCVD(candles []models.OHLCV) float64 {
	// Approximate: if close > open, volume is "buy"; else "sell"
	// More accurate: use (close - low) / (high - low) * volume as buy proxy
	cvd := 0.0
	for _, c := range candles {
		rng := c.High - c.Low
		if rng == 0 {
			continue
		}
		buyRatio := (c.Close - c.Low) / rng
		buyVol := c.Volume * buyRatio
		sellVol := c.Volume * (1 - buyRatio)
		cvd += buyVol - sellVol
	}
	return cvd
}

func classifyCVDTrend(cvd, volume24h float64) models.Trend {
	if volume24h == 0 {
		return models.TrendNeutral
	}
	// Normalize CVD against 24h volume
	ratio := cvd / volume24h
	if math.Abs(ratio) < 0.001 {
		return models.TrendNeutral
	}
	if ratio > 0 {
		if ratio > 0.005 {
			return models.TrendStrongUp
		}
		return models.TrendUp
	}
	if ratio < -0.005 {
		return models.TrendStrongDown
	}
	return models.TrendDown
}

func calcOBImbalance(ob *models.OrderbookSnapshot) float64 {
	bidVol := 0.0
	askVol := 0.0
	for _, b := range ob.Bids {
		bidVol += b.Amount * b.Price
	}
	for _, a := range ob.Asks {
		askVol += a.Amount * a.Price
	}
	if askVol == 0 {
		return 999
	}
	return bidVol / askVol
}

func classifyOBBias(ratio float64) models.OrderbookBias {
	if ratio > 1.5 {
		return models.OBBidHeavy
	}
	if ratio > 1.15 {
		return models.OBBidWall
	}
	if ratio < 0.67 {
		return models.OBAskHeavy
	}
	if ratio < 0.87 {
		return models.OBAskWall
	}
	return models.OBBalanced
}

// calcATR computes Average True Range over the given period
func calcATR(candles []models.OHLCV, period int) float64 {
	if len(candles) < 2 {
		return 0
	}

	var trSum float64
	count := 0
	for i := 1; i < len(candles) && count < period; i++ {
		prevClose := candles[i-1].Close
		h := candles[i].High
		l := candles[i].Low

		tr1 := h - l
		tr2 := math.Abs(h - prevClose)
		tr3 := math.Abs(l - prevClose)

		tr := tr1
		if tr2 > tr {
			tr = tr2
		}
		if tr3 > tr {
			tr = tr3
		}
		trSum += tr
		count++
	}

	if count == 0 {
		return 0
	}
	return trSum / float64(count)
}

// findWall finds the largest order in the book (wall)
func findWall(levels []models.OrderbookLevel) (price, size float64) {
	maxVal := 0.0
	for _, l := range levels {
		val := l.Amount * l.Price
		if val > maxVal {
			maxVal = val
			price = l.Price
			size = l.Amount
		}
	}
	return
}
