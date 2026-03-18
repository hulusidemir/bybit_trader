package analysis

import (
	"log"
	"math"
	"sync"

	"bybit_trader/pkg/exchange"
	"bybit_trader/pkg/exchange/bybit"
	"bybit_trader/pkg/models"
)

type Engine struct {
	bybit     *bybit.Client
	providers []exchange.DataProvider
}

func NewEngine(b *bybit.Client, providers []exchange.DataProvider) *Engine {
	return &Engine{bybit: b, providers: providers}
}

// timeframes used for analysis
var timeframes = []string{"5", "15", "60", "240"}

// bybit kline intervals (same values as tfKey for bybit)
var klineIntervals = map[string]string{
	"5": "5", "15": "15", "60": "60", "240": "240",
}

// ── Aggregation types ───────────────────────────────────────

type tfProviderData struct {
	oi     []models.OpenInterestPoint
	perpTV []models.TakerVolume
	spotTV []models.TakerVolume
	ls     []models.LongShortRatio
}

type aggregatedTFData struct {
	oiSeries     [][]models.OpenInterestPoint
	totalPerpCVD float64
	perpCVDCount int
	totalSpotCVD float64
	spotCVDCount int
	lsRatios     []float64
}

// AnalyzeCoin performs full multi-timeframe analysis using aggregated data from all providers
func (e *Engine) AnalyzeCoin(coin *models.Coin) *models.CoinAnalysis {
	analysis := &models.CoinAnalysis{
		Symbol:      coin.Symbol,
		Metrics:     make(map[string]*models.TimeframeMetrics),
		LastPrice:   coin.LastPrice,
		Volume24h:   coin.Turnover24h,
		FundingRate: coin.FundingRate,
	}

	// Fetch orderbooks from all providers (once, in parallel)
	orderbooks := e.fetchAllOrderbooks(coin.Symbol, 50)

	for _, tf := range timeframes {
		metrics := &models.TimeframeMetrics{
			Timeframe:       tf,
			LastPrice:       coin.LastPrice,
			Volume24h:       coin.Turnover24h,
			FundingRate:     coin.FundingRate,
			NextFundingTime: coin.NextFundingTime,
			FundingInterval: coin.FundingInterval,
		}

		// Fetch OI, CVD, LS from all providers in parallel
		agg := e.fetchAllTFData(coin.Symbol, tf)

		// ── Aggregated Open Interest ─────────────────────
		if len(agg.oiSeries) > 0 {
			metrics.OIChange = exchange.AggregateOIChange(agg.oiSeries)
			metrics.OITrend = classifyTrend(metrics.OIChange, 2.0, 5.0)
		}

		// ── Aggregated Perp CVD ──────────────────────────
		if agg.perpCVDCount > 0 {
			metrics.PerpCVD = agg.totalPerpCVD
			metrics.PerpCVDTrend = classifyCVDTrend(metrics.PerpCVD, coin.Turnover24h)
		}

		// ── Aggregated Spot CVD ──────────────────────────
		if agg.spotCVDCount > 0 {
			metrics.SpotCVD = agg.totalSpotCVD
			metrics.SpotCVDTrend = classifyCVDTrend(metrics.SpotCVD, coin.Turnover24h)
		}

		// ── Aggregated Orderbook ─────────────────────────
		e.applyAggregatedOrderbook(metrics, orderbooks)

		// ── ATR from Bybit klines (primary exchange) ─────
		klines, err := e.bybit.FetchKline(coin.Symbol, klineIntervals[tf], 15)
		if err != nil {
			log.Printf("[%s][%s] kline fetch error: %v", coin.Symbol, tf, err)
		} else {
			metrics.ATR = calcATR(klines, 14)
		}

		// ── Aggregated Long/Short Ratio ──────────────────
		if len(agg.lsRatios) > 0 {
			sum := 0.0
			for _, r := range agg.lsRatios {
				sum += r
			}
			metrics.LSRatio = sum / float64(len(agg.lsRatios))
		}

		analysis.Metrics[tf] = metrics
	}

	return analysis
}

// fetchAllOrderbooks fetches orderbooks from all providers in parallel.
func (e *Engine) fetchAllOrderbooks(symbol string, depth int) []*models.OrderbookSnapshot {
	results := make([]*models.OrderbookSnapshot, len(e.providers))
	var wg sync.WaitGroup

	for i, p := range e.providers {
		if !p.SupportsSymbol(symbol) {
			continue
		}
		wg.Add(1)
		go func(idx int, prov exchange.DataProvider) {
			defer wg.Done()
			ob, err := prov.FetchOrderbook(symbol, depth)
			if err == nil && ob != nil {
				results[idx] = ob
			}
		}(i, p)
	}
	wg.Wait()

	var obs []*models.OrderbookSnapshot
	for _, ob := range results {
		if ob != nil && (len(ob.Bids) > 0 || len(ob.Asks) > 0) {
			obs = append(obs, ob)
		}
	}
	return obs
}

// fetchAllTFData fetches OI, CVD, LS data from all providers for a given timeframe in parallel.
func (e *Engine) fetchAllTFData(symbol, tfKey string) aggregatedTFData {
	datas := make([]tfProviderData, len(e.providers))
	var wg sync.WaitGroup

	for i, p := range e.providers {
		if !p.SupportsSymbol(symbol) {
			continue
		}
		wg.Add(1)
		go func(idx int, prov exchange.DataProvider) {
			defer wg.Done()
			var d tfProviderData
			d.oi, _ = prov.FetchOI(symbol, tfKey, 50)
			d.perpTV, _ = prov.FetchPerpTakerVolume(symbol, tfKey, 30)
			d.spotTV, _ = prov.FetchSpotTakerVolume(symbol, tfKey, 30)
			d.ls, _ = prov.FetchLSRatio(symbol, tfKey, 10)
			datas[idx] = d
		}(i, p)
	}
	wg.Wait()

	var agg aggregatedTFData
	for _, d := range datas {
		if len(d.oi) > 0 {
			agg.oiSeries = append(agg.oiSeries, d.oi)
		}
		if len(d.perpTV) > 0 {
			agg.totalPerpCVD += calcCVD(d.perpTV)
			agg.perpCVDCount++
		}
		if len(d.spotTV) > 0 {
			agg.totalSpotCVD += calcCVD(d.spotTV)
			agg.spotCVDCount++
		}
		if len(d.ls) > 0 {
			agg.lsRatios = append(agg.lsRatios, d.ls[len(d.ls)-1].Ratio)
		}
	}

	return agg
}

// applyAggregatedOrderbook merges orderbooks from all exchanges into metrics.
func (e *Engine) applyAggregatedOrderbook(metrics *models.TimeframeMetrics, orderbooks []*models.OrderbookSnapshot) {
	if len(orderbooks) == 0 {
		return
	}

	totalBidVol := 0.0
	totalAskVol := 0.0
	maxBidWallVal := 0.0
	maxAskWallVal := 0.0

	for _, ob := range orderbooks {
		for _, b := range ob.Bids {
			totalBidVol += b.Amount * b.Price
		}
		for _, a := range ob.Asks {
			totalAskVol += a.Amount * a.Price
		}

		bwP, bwS := findWall(ob.Bids)
		if bwP*bwS > maxBidWallVal {
			maxBidWallVal = bwP * bwS
			metrics.BidWallPrice = bwP
			metrics.BidWallSize = bwS
		}

		awP, awS := findWall(ob.Asks)
		if awP*awS > maxAskWallVal {
			maxAskWallVal = awP * awS
			metrics.AskWallPrice = awP
			metrics.AskWallSize = awS
		}
	}

	if totalAskVol > 0 {
		metrics.OBImbalance = totalBidVol / totalAskVol
		metrics.OBBias = classifyOBBias(metrics.OBImbalance)
	}
}

// ── Helper Functions ────────────────────────────────────────

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
