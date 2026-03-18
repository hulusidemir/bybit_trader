package analysis

import (
	"fmt"
	"math"

	"bybit_trader/pkg/models"
)

// ════════════════════════════════════════════════════════════
// OrderFlowEngine — Pure Order Flow & Manipulation Detection
//
// Zero lagging indicators. Operates on real-time WebSocket data.
// Direction-agnostic detection modules that identify manipulation
// patterns regardless of market direction.
//
// 4 Detection Modules:
//   1. Spoofing Filter   — detects fake OB walls (appear → disappear)
//   2. Absorption        — detects price stalling despite heavy delta
//   3. OI Flush          — detects liquidation cascades from OI anomalies
//   4. CVD Divergence    — detects spot vs futures delta divergence
// ════════════════════════════════════════════════════════════

// OrderFlowState holds computed order flow signals for a symbol.
// Produced by OrderFlowEngine.Detect() on each evaluation cycle.
type OrderFlowState struct {
	// Spoofing module
	SpoofBidDetected bool // fake bid wall appeared and disappeared
	SpoofAskDetected bool // fake ask wall appeared and disappeared

	// Absorption module
	BuyAbsorption   bool    // heavy buying but price stalled/dropped
	SellAbsorption  bool    // heavy selling but price stalled/rose
	AbsorptionScore float64 // 0-1 strength of absorption signal

	// OI Flush module
	OIBuilding   bool    // OI was significantly increasing
	OIFlushing   bool    // OI sudden drop (liquidation cascade)
	OIFlushSide  string  // "long" or "short" — which side got flushed
	OIChangeRate float64 // rate of OI change (positive = building)

	// CVD Divergence module
	CVDDivergence float64 // spot CVD vs futures CVD divergence
	CVDDivBullish bool    // spot buying more than futures (smart money accumulating)
	CVDDivBearish bool    // spot selling more than futures (smart money distributing)

	// Combined scoring
	ManipulationScore int // 0-100 overall manipulation detection confidence
}

// OrderFlowEngine performs real-time order flow analysis.
// Thresholds are computed dynamically per symbol based on Turnover24h
// so that BTCUSDT and PEPEUSDT are not evaluated with the same ruler.
type OrderFlowEngine struct {
	// Spoofing — dynamic: max(floor, Turnover24h * spoofWallRatio)
	spoofWallFloor float64 // absolute minimum wall USD (safety floor)
	spoofWallRatio float64 // fraction of 24h turnover for dynamic minimum
	spoofShrinkRatio float64 // wall must shrink to this fraction to count as spoof

	// Absorption — already normalized against turnover internally
	absorptionMinDelta float64 // minimum CVD ratio for absorption check
	absorptionMaxPrice float64 // base max price change %; scaled by volatility proxy

	// OI Flush — percentage-based, scales naturally
	oiFlushMinDrop  float64 // minimum OI drop % to detect flush
	oiFlushMinBuild float64 // minimum OI rise % before flush counts

	// CVD Divergence — normalized against turnover internally
	cvdDivMinSpread float64 // minimum CVD divergence between spot/futures
}

// NewOrderFlowEngine creates an engine with production-calibrated defaults.
// Spoofing wall threshold is now dynamic: max(50K, Turnover24h × 0.1%).
func NewOrderFlowEngine() *OrderFlowEngine {
	return &OrderFlowEngine{
		spoofWallFloor:   50000,  // $50K absolute floor (filters low-cap noise)
		spoofWallRatio:   0.001,  // 0.1% of 24h turnover
		spoofShrinkRatio: 0.3,    // wall must shrink to <30% of original size

		absorptionMinDelta: 0.003, // 0.3% of 24h volume as CVD delta
		absorptionMaxPrice: 0.08,  // base: price must move <0.08% for absorption

		oiFlushMinDrop:  2.0, // 2% OI drop = flush
		oiFlushMinBuild: 1.0, // 1% OI rise before flush counts

		cvdDivMinSpread: 0.002, // 0.2% of 24h volume divergence
	}
}

// dynamicSpoofWallMin returns the minimum wall size for spoof detection.
// Scales with 24h turnover so BTC ($50B/day) gets ~$25M minimum
// while a $10M/day alt-coin gets ~$5K (floored at $10K).
func (ofe *OrderFlowEngine) dynamicSpoofWallMin(turnover24h float64) float64 {
	dynamic := turnover24h * ofe.spoofWallRatio
	if dynamic < ofe.spoofWallFloor {
		return ofe.spoofWallFloor
	}
	return dynamic
}

// dynamicAbsorptionMaxPrice scales the max price threshold.
// Higher-volume coins get tighter price tolerance (harder to trigger).
// Low-volume coins get wider tolerance: base * max(1, log10(turnover/1M)).
func (ofe *OrderFlowEngine) dynamicAbsorptionMaxPrice(turnover24h float64) float64 {
	if turnover24h <= 0 {
		return ofe.absorptionMaxPrice
	}
	// Scale: $1M turnover → 1×, $100M → 2×, $10B → 4×
	scale := math.Log10(turnover24h / 1e6)
	if scale < 1 {
		scale = 1
	}
	return ofe.absorptionMaxPrice * scale
}

// Detect runs all 4 detection modules on the symbol state and returns
// the combined OrderFlowState. This is the hot-path evaluation function.
func (ofe *OrderFlowEngine) Detect(s *models.SymbolState) *OrderFlowState {
	ofs := &OrderFlowState{}

	ofe.detectSpoofing(s, ofs)
	ofe.detectAbsorption(s, ofs)
	ofe.detectOIFlush(s, ofs)
	ofe.detectCVDDivergence(s, ofs)

	// ── Contradiction guard ──────────────────────────
	// If opposing signals fire simultaneously, neither is reliable.
	if ofs.SpoofBidDetected && ofs.SpoofAskDetected {
		ofs.SpoofBidDetected = false
		ofs.SpoofAskDetected = false
	}
	if ofs.BuyAbsorption && ofs.SellAbsorption {
		ofs.BuyAbsorption = false
		ofs.SellAbsorption = false
		ofs.AbsorptionScore = 0
	}
	if ofs.CVDDivBullish && ofs.CVDDivBearish {
		ofs.CVDDivBullish = false
		ofs.CVDDivBearish = false
	}

	calcManipulationScore(ofs)

	return ofs
}

// ── Module 1: Spoofing Detection ────────────────────────────
// Detects fake orderbook walls that appear and then are pulled
// (disappear without being filled by price trading through them).

func (ofe *OrderFlowEngine) detectSpoofing(s *models.SymbolState, ofs *OrderFlowState) {
	if len(s.WallHistory) < 8 {
		return
	}

	wallMinSize := ofe.dynamicSpoofWallMin(s.Turnover24h)
	now := s.WallHistory[len(s.WallHistory)-1].Timestamp

	// Find the most recent and previous walls for each side.
	// CRITICAL: Only compare walls at the SAME price level (within 0.5%).
	// Comparing different price levels is not spoofing — it's normal OB rotation.
	var lastBid, prevBid, lastAsk, prevAsk *models.WallSnapshot

	for i := len(s.WallHistory) - 1; i >= 0; i-- {
		w := &s.WallHistory[i]
		// Skip stale snapshots (>60s old) — spoof happens fast
		if now-w.Timestamp > 60000 {
			continue
		}
		if w.Side == "bid" {
			if lastBid == nil {
				lastBid = w
			} else if prevBid == nil {
				// Must be at the SAME price level (within 0.5%)
				if lastBid.Price > 0 && math.Abs(w.Price-lastBid.Price)/lastBid.Price < 0.005 {
					prevBid = w
				}
			}
		} else {
			if lastAsk == nil {
				lastAsk = w
			} else if prevAsk == nil {
				if lastAsk.Price > 0 && math.Abs(w.Price-lastAsk.Price)/lastAsk.Price < 0.005 {
					prevAsk = w
				}
			}
		}
		if prevBid != nil && prevAsk != nil {
			break
		}
	}

	// Bid side: was there a significant bid wall at the SAME price that suddenly shrank?
	if prevBid != nil && lastBid != nil {
		prevNotional := prevBid.Price * prevBid.Size
		lastNotional := lastBid.Price * lastBid.Size
		if prevNotional > wallMinSize && lastNotional < prevNotional*ofe.spoofShrinkRatio {
			// Price didn't drop to the bid wall → it was pulled, not filled
			if s.LastPrice > prevBid.Price*1.001 {
				ofs.SpoofBidDetected = true
			}
		}
	}

	// Ask side: was there a significant ask wall at the SAME price that suddenly shrank?
	if prevAsk != nil && lastAsk != nil {
		prevNotional := prevAsk.Price * prevAsk.Size
		lastNotional := lastAsk.Price * lastAsk.Size
		if prevNotional > wallMinSize && lastNotional < prevNotional*ofe.spoofShrinkRatio {
			// Price didn't rise to the ask wall → it was pulled, not filled
			if s.LastPrice < prevAsk.Price*0.999 {
				ofs.SpoofAskDetected = true
			}
		}
	}
}

// ── Module 2: Absorption Detection ──────────────────────────
// Detects when heavy order flow (CVD delta) fails to move price,
// indicating a large hidden player absorbing all the flow.

func (ofe *OrderFlowEngine) detectAbsorption(s *models.SymbolState, ofs *OrderFlowState) {
	if len(s.PriceTicks) < 20 || len(s.CVDHistory) < 20 {
		return
	}
	if s.Turnover24h == 0 {
		return
	}

	// Compare recent CVD snapshot vs 20 snapshots ago (more statistical significance)
	recent := s.CVDHistory[len(s.CVDHistory)-1]
	older := s.CVDHistory[len(s.CVDHistory)-20]

	perpDelta := recent.PerpCVD - older.PerpCVD
	spotDelta := recent.SpotCVD - older.SpotCVD
	totalDelta := perpDelta + spotDelta

	// Price change over same period
	recentPrice := s.PriceTicks[len(s.PriceTicks)-1].Price
	olderIdx := len(s.PriceTicks) - 20
	if olderIdx < 0 {
		olderIdx = 0
	}
	olderPrice := s.PriceTicks[olderIdx].Price
	if olderPrice == 0 {
		return
	}
	priceChangePct := (recentPrice - olderPrice) / olderPrice * 100

	// Normalize CVD delta against 24h volume
	cvdRatio := math.Abs(totalDelta) / s.Turnover24h

	// Dynamic absorption max price: scales with coin's turnover
	maxPriceChange := ofe.dynamicAbsorptionMaxPrice(s.Turnover24h)

	// Buy absorption: strong buying (positive CVD delta) but price barely moves or drops
	if totalDelta > 0 && cvdRatio > ofe.absorptionMinDelta && priceChangePct < maxPriceChange {
		ofs.BuyAbsorption = true
		ofs.AbsorptionScore = math.Min(cvdRatio/ofe.absorptionMinDelta, 5.0) / 5.0
	}

	// Sell absorption: strong selling (negative CVD delta) but price barely moves or rises
	if totalDelta < 0 && cvdRatio > ofe.absorptionMinDelta && priceChangePct > -maxPriceChange {
		ofs.SellAbsorption = true
		ofs.AbsorptionScore = math.Min(cvdRatio/ofe.absorptionMinDelta, 5.0) / 5.0
	}
}

// ── Module 3: OI Flush Detection ────────────────────────────
// Detects the build-up → flush pattern: OI rising (positions accumulating)
// then suddenly dropping (mass liquidation / stop cascade).

func (ofe *OrderFlowEngine) detectOIFlush(s *models.SymbolState, ofs *OrderFlowState) {
	n := len(s.OIHistory)
	if n < 10 {
		return
	}

	recent := s.OIHistory[n-1]
	mid := s.OIHistory[n/2]
	old := s.OIHistory[0]

	if old.TotalOI == 0 || mid.TotalOI == 0 {
		return
	}

	// Phase 1: OI was building (old → mid)
	buildPct := (mid.TotalOI - old.TotalOI) / old.TotalOI * 100

	// Phase 2: OI dropped (mid → recent)
	dropPct := (recent.TotalOI - mid.TotalOI) / mid.TotalOI * 100

	ofs.OIChangeRate = (recent.TotalOI - old.TotalOI) / old.TotalOI * 100

	if buildPct > ofe.oiFlushMinBuild {
		ofs.OIBuilding = true
	}

	if buildPct > ofe.oiFlushMinBuild && dropPct < -ofe.oiFlushMinDrop {
		ofs.OIFlushing = true

		// Determine which side got flushed based on price direction
		if len(s.PriceTicks) >= 2 {
			recentPrice := s.PriceTicks[len(s.PriceTicks)-1].Price
			midIdx := len(s.PriceTicks) / 2
			if midIdx < len(s.PriceTicks) {
				midPrice := s.PriceTicks[midIdx].Price
				if midPrice > 0 {
					if recentPrice < midPrice {
						ofs.OIFlushSide = "long" // price dropped → longs got flushed
					} else {
						ofs.OIFlushSide = "short" // price rose → shorts got flushed
					}
				}
			}
		}
	}
}

// ── Module 4: CVD Divergence Detection ──────────────────────
// Detects divergence between spot CVD and futures CVD.
// Spot = "smart money" (no leverage), Futures = "retail" (leveraged).
// Divergence signals manipulation or informed positioning.

func (ofe *OrderFlowEngine) detectCVDDivergence(s *models.SymbolState, ofs *OrderFlowState) {
	if len(s.CVDHistory) < 10 {
		return
	}
	if s.Turnover24h == 0 {
		return
	}

	// Guard: if no spot exchange is feeding data, skip divergence entirely.
	// Without real spot CVD, the math becomes 0 - perp = false bearish signal.
	if len(s.SpotCVD) == 0 {
		return
	}

	recent := s.CVDHistory[len(s.CVDHistory)-1]
	older := s.CVDHistory[len(s.CVDHistory)-10]

	perpDelta := recent.PerpCVD - older.PerpCVD
	spotDelta := recent.SpotCVD - older.SpotCVD

	// If spot CVD hasn't changed across the window, spot data is likely stale/absent
	if spotDelta == 0 && recent.SpotCVD == 0 {
		return
	}

	// Normalize against 24h volume
	perpNorm := perpDelta / s.Turnover24h
	spotNorm := spotDelta / s.Turnover24h

	divergence := spotNorm - perpNorm
	ofs.CVDDivergence = divergence

	if divergence > ofe.cvdDivMinSpread {
		ofs.CVDDivBullish = true // spot buying more than futures → accumulation
	} else if divergence < -ofe.cvdDivMinSpread {
		ofs.CVDDivBearish = true // spot selling more than futures → distribution
	}
}

// ── Scoring ─────────────────────────────────────────────────

func calcManipulationScore(ofs *OrderFlowState) {
	score := 0

	if ofs.SpoofBidDetected || ofs.SpoofAskDetected {
		score += 30
	}
	if ofs.BuyAbsorption || ofs.SellAbsorption {
		score += int(ofs.AbsorptionScore * 25)
	}
	if ofs.OIFlushing {
		score += 25
	}
	if ofs.CVDDivBullish || ofs.CVDDivBearish {
		score += 20
	}

	if score > 100 {
		score = 100
	}
	ofs.ManipulationScore = score
}

// String returns a human-readable summary of the OrderFlowState.
func (ofs *OrderFlowState) String() string {
	parts := ""
	if ofs.SpoofBidDetected {
		parts += "SpoofBid "
	}
	if ofs.SpoofAskDetected {
		parts += "SpoofAsk "
	}
	if ofs.BuyAbsorption {
		parts += fmt.Sprintf("BuyAbsorb(%.0f%%) ", ofs.AbsorptionScore*100)
	}
	if ofs.SellAbsorption {
		parts += fmt.Sprintf("SellAbsorb(%.0f%%) ", ofs.AbsorptionScore*100)
	}
	if ofs.OIFlushing {
		parts += fmt.Sprintf("OIFlush(%s) ", ofs.OIFlushSide)
	}
	if ofs.CVDDivBullish {
		parts += "CVDDivBull "
	}
	if ofs.CVDDivBearish {
		parts += "CVDDivBear "
	}
	if parts == "" {
		parts = "none"
	}
	return fmt.Sprintf("[Score:%d] %s", ofs.ManipulationScore, parts)
}
