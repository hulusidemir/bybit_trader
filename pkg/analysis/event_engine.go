package analysis

import (
	"math"
	"sync"
	"time"

	"bybit_trader/pkg/models"
)

// ════════════════════════════════════════════════════════════
// EventEngine — Event-Driven Analysis Engine
//
// Replaces the REST-polling Engine with an in-memory state machine.
// Receives MarketEvents, updates per-symbol state, and evaluates
// signal conditions in real-time. All operations are non-blocking.
//
// Architecture:
//   MarketEvent → ProcessEvent() → update SymbolState → evaluate()
//                                                       → []MTFResult
// ════════════════════════════════════════════════════════════

type EventEngine struct {
	mu     sync.RWMutex
	states map[string]*models.SymbolState

	orderFlow    *OrderFlowEngine
	evalCooldown time.Duration // minimum time between evaluations per symbol
	lastEval     map[string]time.Time
}

// NewEventEngine creates a production-ready event engine.
func NewEventEngine() *EventEngine {
	return &EventEngine{
		states:       make(map[string]*models.SymbolState),
		orderFlow:    NewOrderFlowEngine(),
		evalCooldown: 5 * time.Second,
		lastEval:     make(map[string]time.Time),
	}
}

// ProcessEvent ingests a single MarketEvent, updates in-memory state,
// and evaluates signal conditions. Returns MTF results for the caller
// to convert into signals (avoiding analysis→signals import cycle).
// This is the hot path — must be allocation-minimal and non-blocking.
func (e *EventEngine) ProcessEvent(evt models.MarketEvent) []MTFResult {
	e.mu.Lock()
	state := e.getOrCreate(evt.Symbol)

	switch evt.EventType {
	case models.EventTick:
		e.applyTick(state, evt)
	case models.EventOIUpdate:
		e.applyOI(state, evt)
	case models.EventOBUpdate:
		e.applyOrderbook(state, evt)
	case models.EventCVDUpdate:
		e.applyCVD(state, evt)
	case models.EventKline:
		e.applyKline(state, evt)
	}

	state.LastUpdate = evt.Timestamp
	state.EventCount++
	e.mu.Unlock()

	// Evaluate signal conditions (read-lock only)
	return e.evaluate(evt.Symbol)
}

// RemoveSymbols purges state for symbols no longer in the hot list.
func (e *EventEngine) RemoveSymbols(symbols []string) {
	remove := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		remove[s] = true
	}
	e.mu.Lock()
	for sym := range e.states {
		if remove[sym] {
			delete(e.states, sym)
			delete(e.lastEval, sym)
		}
	}
	e.mu.Unlock()
}

// ── Event Application (hot path) ────────────────────────────

func (e *EventEngine) getOrCreate(symbol string) *models.SymbolState {
	s, ok := e.states[symbol]
	if !ok {
		s = models.NewSymbolState(symbol)
		e.states[symbol] = s
	}
	return s
}

func (e *EventEngine) applyTick(s *models.SymbolState, evt models.MarketEvent) {
	p, ok := evt.Payload.(models.TickPayload)
	if !ok {
		return
	}
	s.LastPrice = p.LastPrice
	s.Volume24h = p.Volume24h
	s.Turnover24h = p.Turnover24h
	s.FundingRate = p.FundingRate
	if p.OpenInterest > 0 {
		s.OI[evt.Exchange] = p.OpenInterest
		e.recordOISnapshot(s, evt.Timestamp)
	}
	// Track price ticks for absorption detection
	s.PriceTicks = append(s.PriceTicks, models.PriceTick{
		Timestamp: evt.Timestamp,
		Price:     p.LastPrice,
	})
	if len(s.PriceTicks) > models.MaxPriceTicks {
		s.PriceTicks = s.PriceTicks[len(s.PriceTicks)-models.MaxPriceTicks:]
	}
}

func (e *EventEngine) applyOI(s *models.SymbolState, evt models.MarketEvent) {
	p, ok := evt.Payload.(models.OIPayload)
	if !ok {
		return
	}
	s.OI[evt.Exchange] = p.OpenInterest
	e.recordOISnapshot(s, evt.Timestamp)
}

func (e *EventEngine) applyOrderbook(s *models.SymbolState, evt models.MarketEvent) {
	p, ok := evt.Payload.(models.OBPayload)
	if !ok {
		return
	}
	snap := &models.OrderbookSnapshot{
		Symbol:    evt.Symbol,
		Timestamp: evt.Timestamp,
		Bids:      p.Bids,
		Asks:      p.Asks,
	}
	if p.IsFutures {
		s.FuturesOBs[evt.Exchange] = snap
	} else {
		s.SpotOBs[evt.Exchange] = snap
	}
	// Track walls for spoofing detection
	if bwP, bwS := findWall(p.Bids); bwS > 0 {
		s.WallHistory = append(s.WallHistory, models.WallSnapshot{
			Timestamp: evt.Timestamp,
			Side:      "bid",
			Price:     bwP,
			Size:      bwS,
		})
	}
	if awP, awS := findWall(p.Asks); awS > 0 {
		s.WallHistory = append(s.WallHistory, models.WallSnapshot{
			Timestamp: evt.Timestamp,
			Side:      "ask",
			Price:     awP,
			Size:      awS,
		})
	}
	if len(s.WallHistory) > models.MaxWallHistory {
		s.WallHistory = s.WallHistory[len(s.WallHistory)-models.MaxWallHistory:]
	}
}

func (e *EventEngine) applyCVD(s *models.SymbolState, evt models.MarketEvent) {
	p, ok := evt.Payload.(models.CVDPayload)
	if !ok {
		return
	}
	if p.IsSpot {
		s.SpotCVD[evt.Exchange] += p.NetDelta
	} else {
		s.PerpCVD[evt.Exchange] += p.NetDelta
	}
	// Record CVD snapshot for divergence detection
	var totalPerp, totalSpot float64
	for _, v := range s.PerpCVD {
		totalPerp += v
	}
	for _, v := range s.SpotCVD {
		totalSpot += v
	}
	s.CVDHistory = append(s.CVDHistory, models.CVDSnapshot{
		Timestamp: evt.Timestamp,
		PerpCVD:   totalPerp,
		SpotCVD:   totalSpot,
	})
	if len(s.CVDHistory) > models.MaxCVDHistory {
		s.CVDHistory = s.CVDHistory[len(s.CVDHistory)-models.MaxCVDHistory:]
	}
}

func (e *EventEngine) applyKline(s *models.SymbolState, evt models.MarketEvent) {
	p, ok := evt.Payload.(models.KlinePayload)
	if !ok {
		return
	}
	candle := models.OHLCV{
		Timestamp: evt.Timestamp,
		Open:      p.Open,
		High:      p.High,
		Low:       p.Low,
		Close:     p.Close,
		Volume:    p.Volume,
		Turnover:  p.Turnover,
	}

	tf := p.Timeframe
	klines := s.Klines[tf]

	if p.Closed {
		// Finalized candle: append to rolling window
		klines = append(klines, candle)
		if len(klines) > models.MaxKlineWindow {
			klines = klines[len(klines)-models.MaxKlineWindow:]
		}
	} else if len(klines) > 0 {
		// Update the forming candle (last element)
		klines[len(klines)-1] = candle
	} else {
		klines = append(klines, candle)
	}
	s.Klines[tf] = klines
}

// ── Signal Evaluation ───────────────────────────────────────

// evaluate checks if a symbol has enough accumulated data for analysis,
// then runs OrderFlow detection and MTF pattern matching.
func (e *EventEngine) evaluate(symbol string) []MTFResult {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Cooldown check: prevent over-evaluation
	if last, ok := e.lastEval[symbol]; ok {
		if time.Since(last) < e.evalCooldown {
			return nil
		}
	}

	state, ok := e.states[symbol]
	if !ok || state.LastPrice == 0 {
		return nil
	}

	// Data readiness: need enough events for meaningful OrderFlow analysis
	if state.EventCount < 50 {
		return nil
	}

	// Run OrderFlow detection on raw state
	ofs := e.orderFlow.Detect(state)
	if ofs.ManipulationScore < 20 {
		return nil
	}

	// Synthesize metrics for display & secondary confirmation
	coinAnalysis := e.synthesize(state)
	if coinAnalysis == nil {
		return nil
	}

	// Run OrderFlow-based MTF analysis
	mtfResults := AnalyzeMTF(coinAnalysis, ofs)
	if len(mtfResults) == 0 {
		return nil
	}

	// Update cooldown
	e.lastEval[symbol] = time.Now()

	return mtfResults
}

// synthesize converts accumulated SymbolState into a CoinAnalysis
// compatible with the existing MTF/pattern matching pipeline.
func (e *EventEngine) synthesize(s *models.SymbolState) *models.CoinAnalysis {
	ca := &models.CoinAnalysis{
		Symbol:      s.Symbol,
		Timestamp:   time.Now(),
		Metrics:     make(map[string]*models.TimeframeMetrics),
		LastPrice:   s.LastPrice,
		Volume24h:   s.Turnover24h,
		FundingRate: s.FundingRate,
	}

	// Collect all orderbook snapshots into sets
	var futuresOBs []*models.OrderbookSnapshot
	for _, ob := range s.FuturesOBs {
		if ob != nil {
			futuresOBs = append(futuresOBs, ob)
		}
	}
	var spotOBs []*models.OrderbookSnapshot
	for _, ob := range s.SpotOBs {
		if ob != nil {
			spotOBs = append(spotOBs, ob)
		}
	}

	for _, tf := range timeframes {
		metrics := &models.TimeframeMetrics{
			Timeframe:       tf,
			LastPrice:       s.LastPrice,
			Volume24h:       s.Turnover24h,
			FundingRate:     s.FundingRate,
			NextFundingTime: s.NextFundingTime,
			FundingInterval: s.FundingInterval,
		}

		// ═══ PHASE 1: AGGREGATED OI (sum across exchanges) ═══
		var totalOI float64
		for _, oi := range s.OI {
			totalOI += oi
		}
		if totalOI > 0 {
			metrics.OIChange = 0 // OI change requires history; set from OI events with ChangePct
			metrics.OITrend = models.TrendNeutral
		}

		// ═══ PHASE 3: CVD (sum across exchanges) ═══
		var totalPerpCVD float64
		for _, cvd := range s.PerpCVD {
			totalPerpCVD += cvd
		}
		if len(s.PerpCVD) > 0 {
			metrics.PerpCVD = totalPerpCVD
			metrics.PerpCVDTrend = classifyCVDTrend(totalPerpCVD, s.Turnover24h)
		}

		var totalSpotCVD float64
		for _, cvd := range s.SpotCVD {
			totalSpotCVD += cvd
		}
		if len(s.SpotCVD) > 0 {
			metrics.SpotCVD = totalSpotCVD
			metrics.SpotCVDTrend = classifyCVDTrend(totalSpotCVD, s.Turnover24h)
		}

		// ═══ PHASE 4: ORDERBOOK (separate futures + spot bias) ═══
		applyFuturesOBBias(metrics, futuresOBs)
		applySpotOBBias(metrics, spotOBs)
		applyCombinedOB(metrics, futuresOBs, spotOBs)

		ca.Metrics[tf] = metrics
	}

	return ca
}

// ── Orderbook helpers (stateless, no Engine receiver needed) ─

func applyFuturesOBBias(m *models.TimeframeMetrics, obs []*models.OrderbookSnapshot) {
	if len(obs) == 0 {
		return
	}
	bidVol, askVol := aggregateOBVolume(obs)
	if askVol > 0 {
		m.FuturesOBBias = classifyOBBias(bidVol / askVol)
	}
}

func applySpotOBBias(m *models.TimeframeMetrics, obs []*models.OrderbookSnapshot) {
	if len(obs) == 0 {
		return
	}
	bidVol, askVol := aggregateOBVolume(obs)
	if askVol > 0 {
		m.SpotOBBias = classifyOBBias(bidVol / askVol)
	}
}

func applyCombinedOB(m *models.TimeframeMetrics, futuresOBs, spotOBs []*models.OrderbookSnapshot) {
	all := append(futuresOBs, spotOBs...)
	if len(all) == 0 {
		return
	}

	var totalBidVol, totalAskVol float64
	var maxBidWallVal, maxAskWallVal float64

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
			m.BidWallPrice = bwP
			m.BidWallSize = bwS
		}
		awP, awS := findWall(ob.Asks)
		if awP*awS > maxAskWallVal {
			maxAskWallVal = awP * awS
			m.AskWallPrice = awP
			m.AskWallSize = awS
		}
	}

	if totalAskVol > 0 {
		m.OBImbalance = totalBidVol / totalAskVol
		m.OBBias = classifyOBBias(m.OBImbalance)
	}
}

// roundFloat prevents floating point drift in hot-path accumulation.
func roundFloat(val float64, precision int) float64 {
	p := math.Pow(10, float64(precision))
	return math.Round(val*p) / p
}

// recordOISnapshot aggregates OI across exchanges and appends to history.
func (e *EventEngine) recordOISnapshot(s *models.SymbolState, ts int64) {
	var totalOI float64
	for _, oi := range s.OI {
		totalOI += oi
	}
	s.OIHistory = append(s.OIHistory, models.OISnapshot{
		Timestamp: ts,
		TotalOI:   totalOI,
	})
	if len(s.OIHistory) > models.MaxOIHistory {
		s.OIHistory = s.OIHistory[len(s.OIHistory)-models.MaxOIHistory:]
	}
}
