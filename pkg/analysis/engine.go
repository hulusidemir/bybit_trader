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

// bybit kline intervals
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

type orderbookSet struct {
	futures []*models.OrderbookSnapshot
	spot    []*models.OrderbookSnapshot
}

// AnalyzeCoin performs full multi-timeframe analysis using aggregated data from all providers.
// Price-action-first approach: kline analysis determines structure, then confirmation data is layered.
func (e *Engine) AnalyzeCoin(coin *models.Coin) *models.CoinAnalysis {
	analysis := &models.CoinAnalysis{
		Symbol:      coin.Symbol,
		Metrics:     make(map[string]*models.TimeframeMetrics),
		LastPrice:   coin.LastPrice,
		Volume24h:   coin.Turnover24h,
		FundingRate: coin.FundingRate,
	}

	// Fetch orderbooks from all providers (once, in parallel) — split futures vs spot
	obs := e.fetchAllOrderbooks(coin.Symbol, 50)

	for _, tf := range timeframes {
		metrics := &models.TimeframeMetrics{
			Timeframe:       tf,
			LastPrice:       coin.LastPrice,
			Volume24h:       coin.Turnover24h,
			FundingRate:     coin.FundingRate,
			NextFundingTime: coin.NextFundingTime,
			FundingInterval: coin.FundingInterval,
		}

		// ═══ PHASE 1: PRICE ACTION (from Bybit klines) ═══
		klines, err := e.bybit.FetchKline(coin.Symbol, klineIntervals[tf], 30)
		if err != nil {
			log.Printf("[%s][%s] kline fetch error: %v", coin.Symbol, tf, err)
		} else if len(klines) >= 6 {
			metrics.ATR = calcATR(klines, 14)
			metrics.PriceTrend = calcPriceTrend(klines)
			metrics.PriceRangePos = calcPriceRangePos(klines, coin.LastPrice)

			// EMA calculation for momentum structure
			metrics.EMAFast = calcEMA(klines, 9)
			metrics.EMASlow = calcEMA(klines, 21)
			metrics.EMABull = metrics.EMAFast > metrics.EMASlow

			// Volume profile: current vs average
			metrics.VolProfile = calcVolumeProfile(klines)
		}

		// ═══ PHASE 2: MARKET DATA (aggregated from all exchanges) ═══
		agg := e.fetchAllTFData(coin.Symbol, tf)

		// Aggregated Open Interest
		if len(agg.oiSeries) > 0 {
			metrics.OIChange = exchange.AggregateOIChange(agg.oiSeries)
			metrics.OITrend = classifyTrend(metrics.OIChange, 2.0, 5.0)
		}

		// Aggregated Perp CVD
		if agg.perpCVDCount > 0 {
			metrics.PerpCVD = agg.totalPerpCVD
			metrics.PerpCVDTrend = classifyCVDTrend(metrics.PerpCVD, coin.Turnover24h)
		}

		// Aggregated Spot CVD
		if agg.spotCVDCount > 0 {
			metrics.SpotCVD = agg.totalSpotCVD
			metrics.SpotCVDTrend = classifyCVDTrend(metrics.SpotCVD, coin.Turnover24h)
		}

		// ═══ PHASE 3: ORDERBOOK (separate futures and spot) ═══
		e.applyFuturesOrderbook(metrics, obs.futures)
		e.applySpotOrderbook(metrics, obs.spot)
		// Combined OB for walls and overall bias
		e.applyCombinedOrderbook(metrics, obs)

		// Aggregated Long/Short Ratio
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

// fetchAllOrderbooks fetches futures and spot orderbooks separately from all providers.
func (e *Engine) fetchAllOrderbooks(symbol string, depth int) orderbookSet {
	type result struct {
		futures *models.OrderbookSnapshot
		spot    *models.OrderbookSnapshot
	}
	results := make([]result, len(e.providers))
	var wg sync.WaitGroup

	for i, p := range e.providers {
		if !p.SupportsSymbol(symbol) {
			continue
		}
		wg.Add(1)
		go func(idx int, prov exchange.DataProvider) {
			defer wg.Done()
			var r result
			if ob, err := prov.FetchFuturesOrderbook(symbol, depth); err == nil && ob != nil {
				r.futures = ob
			}
			if ob, err := prov.FetchSpotOrderbook(symbol, depth); err == nil && ob != nil {
				r.spot = ob
			}
			results[idx] = r
		}(i, p)
	}
	wg.Wait()

	var set orderbookSet
	for _, r := range results {
		if r.futures != nil && (len(r.futures.Bids) > 0 || len(r.futures.Asks) > 0) {
			set.futures = append(set.futures, r.futures)
		}
		if r.spot != nil && (len(r.spot.Bids) > 0 || len(r.spot.Asks) > 0) {
			set.spot = append(set.spot, r.spot)
		}
	}
	return set
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

// ── Orderbook Processing ────────────────────────────────────

func (e *Engine) applyFuturesOrderbook(metrics *models.TimeframeMetrics, orderbooks []*models.OrderbookSnapshot) {
	if len(orderbooks) == 0 {
		return
	}
	totalBidVol, totalAskVol := aggregateOBVolume(orderbooks)
	if totalAskVol > 0 {
		ratio := totalBidVol / totalAskVol
		metrics.FuturesOBBias = classifyOBBias(ratio)
	}
}

func (e *Engine) applySpotOrderbook(metrics *models.TimeframeMetrics, orderbooks []*models.OrderbookSnapshot) {
	if len(orderbooks) == 0 {
		return
	}
	totalBidVol, totalAskVol := aggregateOBVolume(orderbooks)
	if totalAskVol > 0 {
		ratio := totalBidVol / totalAskVol
		metrics.SpotOBBias = classifyOBBias(ratio)
	}
}

func (e *Engine) applyCombinedOrderbook(metrics *models.TimeframeMetrics, obs orderbookSet) {
	all := append(obs.futures, obs.spot...)
	if len(all) == 0 {
		return
	}

	totalBidVol := 0.0
	totalAskVol := 0.0
	maxBidWallVal := 0.0
	maxAskWallVal := 0.0

	for _, ob := range all {
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

func aggregateOBVolume(orderbooks []*models.OrderbookSnapshot) (bidVol, askVol float64) {
	for _, ob := range orderbooks {
		for _, b := range ob.Bids {
			bidVol += b.Amount * b.Price
		}
		for _, a := range ob.Asks {
			askVol += a.Amount * a.Price
		}
	}
	return
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

// calcPriceTrend determines price direction from recent candles.
// Uses weighted comparison and candle structure analysis.
func calcPriceTrend(candles []models.OHLCV) models.Trend {
	n := len(candles)
	if n < 10 {
		return models.TrendNeutral
	}

	// Compare last 5 candles avg vs previous 5
	recentSum := 0.0
	for i := n - 5; i < n; i++ {
		recentSum += candles[i].Close
	}
	recentAvg := recentSum / 5.0

	prevSum := 0.0
	for i := n - 10; i < n-5; i++ {
		prevSum += candles[i].Close
	}
	prevAvg := prevSum / 5.0

	if prevAvg == 0 {
		return models.TrendNeutral
	}

	changePct := (recentAvg - prevAvg) / prevAvg * 100

	// Also check candle structure: higher lows / lower highs
	hlScore := calcHLScore(candles[n-10:])

	// Combined directional strength
	strength := changePct + hlScore*0.3

	if strength > 0.6 {
		return models.TrendStrongUp
	}
	if strength > 0.2 {
		return models.TrendUp
	}
	if strength < -0.6 {
		return models.TrendStrongDown
	}
	if strength < -0.2 {
		return models.TrendDown
	}
	return models.TrendNeutral
}

// calcHLScore measures higher-lows vs lower-highs structure.
// Positive = bullish structure, Negative = bearish structure.
func calcHLScore(candles []models.OHLCV) float64 {
	if len(candles) < 3 {
		return 0
	}
	higherLows := 0
	lowerHighs := 0
	for i := 1; i < len(candles); i++ {
		if candles[i].Low > candles[i-1].Low {
			higherLows++
		}
		if candles[i].High < candles[i-1].High {
			lowerHighs++
		}
	}
	total := len(candles) - 1
	if total == 0 {
		return 0
	}
	return float64(higherLows-lowerHighs) / float64(total) * 100
}

// calcPriceRangePos calculates where current price sits within recent high-low range.
// Returns 0-100: 0 = at the bottom, 100 = at the top.
func calcPriceRangePos(candles []models.OHLCV, currentPrice float64) float64 {
	if len(candles) < 2 || currentPrice == 0 {
		return 50 // unknown = assume middle
	}

	highest := 0.0
	lowest := math.MaxFloat64
	for _, c := range candles {
		if c.High > highest {
			highest = c.High
		}
		if c.Low < lowest {
			lowest = c.Low
		}
	}

	rangeSize := highest - lowest
	if rangeSize == 0 {
		return 50
	}

	pos := (currentPrice - lowest) / rangeSize * 100
	if pos < 0 {
		pos = 0
	}
	if pos > 100 {
		pos = 100
	}
	return pos
}

// calcEMA computes the Exponential Moving Average for the given period.
// Returns the final EMA value.
func calcEMA(candles []models.OHLCV, period int) float64 {
	if len(candles) == 0 {
		return 0
	}
	if len(candles) < period {
		sum := 0.0
		for _, c := range candles {
			sum += c.Close
		}
		return sum / float64(len(candles))
	}

	// Seed EMA with SMA of first `period` candles
	sma := 0.0
	for i := 0; i < period; i++ {
		sma += candles[i].Close
	}
	ema := sma / float64(period)

	// Apply EMA formula for remaining candles
	k := 2.0 / float64(period+1)
	for i := period; i < len(candles); i++ {
		ema = candles[i].Close*k + ema*(1-k)
	}

	return ema
}

// calcVolumeProfile returns current volume / average volume.
// >1 means above average, <1 means below average.
func calcVolumeProfile(candles []models.OHLCV) float64 {
	if len(candles) < 5 {
		return 1.0
	}

	// Average volume of all candles except last 3
	avgPeriod := len(candles) - 3
	if avgPeriod < 3 {
		avgPeriod = 3
	}
	var avgSum float64
	for i := 0; i < avgPeriod; i++ {
		avgSum += candles[i].Volume
	}
	avgVol := avgSum / float64(avgPeriod)

	if avgVol == 0 {
		return 1.0
	}

	// Recent 3 candles average
	var recentSum float64
	for i := len(candles) - 3; i < len(candles); i++ {
		recentSum += candles[i].Volume
	}
	recentVol := recentSum / 3.0

	return recentVol / avgVol
}
