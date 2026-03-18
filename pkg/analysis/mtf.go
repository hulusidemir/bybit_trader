package analysis

import (
	"bybit_trader/pkg/models"
)

// MTFResult holds the multi-timeframe confluence analysis.
// OrderFlow patterns are detected at the state level; TF metrics
// provide secondary confirmation and display data.
type MTFResult struct {
	Symbol          string
	Direction       models.SignalDirection
	Pattern         models.PatternName
	Description     string
	ConfluenceScore int                              // 0-100
	AlignedTFs      int                              // how many timeframes show supporting data
	TotalTFs        int                              // always 4
	TFDetails       map[string]bool                  // tf -> supports direction
	PrimaryTF       string                           // always "5" for OrderFlow (fastest)
	Metrics         map[string]*models.TimeframeMetrics
	HasOBSupport    bool // orderbook confirms direction
	HasCVDConfirm   bool // both perp+spot CVD confirm

	// OrderFlow-specific confirmations
	HasOIConfirm       bool // OI building or flushing detected
	SpoofingDetected   bool // spoofing detected on either side
	AbsorptionDetected bool // absorption detected on either side
}

// AnalyzeMTF performs OrderFlow-based confluence analysis.
// Patterns come from OrderFlowState; CoinAnalysis provides
// per-TF metrics for secondary confirmation and display.
func AnalyzeMTF(analysis *models.CoinAnalysis, ofs *OrderFlowState) []MTFResult {
	patterns := ClassifyPatterns(ofs)
	if len(patterns) == 0 {
		return nil
	}

	var results []MTFResult

	// Deduplicate by direction — take the first (highest priority) pattern per direction
	seen := make(map[models.SignalDirection]bool)

	for _, p := range patterns {
		if seen[p.Direction] {
			continue
		}
		seen[p.Direction] = true

		// Count supporting TFs based on OB/CVD alignment
		aligned := 0
		tfDetails := make(map[string]bool)
		for tf, m := range analysis.Metrics {
			if supportsDirection(m, p.Direction) {
				aligned++
				tfDetails[tf] = true
			}
		}

		hasOBSupport := checkOBSupport(analysis.Metrics, p.Direction)
		hasCVDConfirm := checkCVDConfirmation(analysis.Metrics, p.Direction)

		score := calcConfluenceScore(ofs, aligned, analysis.Metrics, p.Direction, hasOBSupport, hasCVDConfirm)

		if score < 60 {
			continue
		}

		// Primary TF is always "5" for OrderFlow (fastest reaction)
		primaryTF := "5"
		if _, ok := analysis.Metrics[primaryTF]; !ok {
			// Fallback to first available
			for tf := range analysis.Metrics {
				primaryTF = tf
				break
			}
		}

		results = append(results, MTFResult{
			Symbol:             analysis.Symbol,
			Direction:          p.Direction,
			Pattern:            p.Pattern,
			Description:        p.Description,
			ConfluenceScore:    score,
			AlignedTFs:         aligned,
			TotalTFs:           4,
			TFDetails:          tfDetails,
			PrimaryTF:          primaryTF,
			Metrics:            analysis.Metrics,
			HasOBSupport:       hasOBSupport,
			HasCVDConfirm:      hasCVDConfirm,
			HasOIConfirm:       ofs.OIBuilding || ofs.OIFlushing,
			SpoofingDetected:   ofs.SpoofBidDetected || ofs.SpoofAskDetected,
			AbsorptionDetected: ofs.BuyAbsorption || ofs.SellAbsorption,
		})
	}

	return results
}

// supportsDirection checks if a TF's OB AND CVD both support the signal direction.
// Uses AND logic — both indicators must confirm (not just one).
func supportsDirection(m *models.TimeframeMetrics, dir models.SignalDirection) bool {
	if dir == models.DirectionLong {
		return m.OBBias >= models.OBBidWall && m.PerpCVDTrend >= models.TrendUp
	}
	return m.OBBias <= models.OBAskWall && m.PerpCVDTrend <= models.TrendDown
}

func checkOBSupport(metrics map[string]*models.TimeframeMetrics, dir models.SignalDirection) bool {
	for _, m := range metrics {
		if dir == models.DirectionLong && m.OBBias >= models.OBBidWall {
			return true
		}
		if dir == models.DirectionShort && m.OBBias <= models.OBAskWall {
			return true
		}
	}
	return false
}

func checkCVDConfirmation(metrics map[string]*models.TimeframeMetrics, dir models.SignalDirection) bool {
	perpConfirm := false
	hasPerpData := false

	for _, m := range metrics {
		if m.PerpCVD != 0 {
			hasPerpData = true
		}

		if dir == models.DirectionLong {
			// Require actual bullish perp CVD trend (not neutral)
			if m.PerpCVDTrend >= models.TrendUp {
				perpConfirm = true
			}
		} else {
			// Require actual bearish perp CVD trend (not neutral)
			if m.PerpCVDTrend <= models.TrendDown {
				perpConfirm = true
			}
		}
	}

	// No perp CVD data at all → cannot confirm direction
	if !hasPerpData {
		return false
	}

	return perpConfirm
}

func calcConfluenceScore(
	ofs *OrderFlowState,
	aligned int,
	metrics map[string]*models.TimeframeMetrics,
	dir models.SignalDirection,
	hasOBSupport bool,
	hasCVDConfirm bool,
) int {
	score := 0

	// ── OrderFlow detection strength (max 40) ──────
	score += int(float64(ofs.ManipulationScore) * 0.40)

	// ── Orderbook support (max 15) ─────────────────
	if hasOBSupport {
		score += 15
	}

	// ── CVD dual confirmation (max 15) ─────────────
	if hasCVDConfirm {
		score += 15
	}

	// ── Multi-detection bonus (max 20) ─────────────
	// Replaces fake TF alignment (all TFs have identical data).
	// Rewards when multiple OrderFlow modules fire simultaneously.
	detections := 0
	if ofs.SpoofBidDetected || ofs.SpoofAskDetected {
		detections++
	}
	if ofs.BuyAbsorption || ofs.SellAbsorption {
		detections++
	}
	if ofs.OIFlushing {
		detections++
	}
	if ofs.CVDDivBullish || ofs.CVDDivBearish {
		detections++
	}
	if detections >= 3 {
		score += 20
	} else if detections >= 2 {
		score += 10
	}

	// ── OI-CVD momentum (max 10) ───────────────────
	for _, m := range metrics {
		if dir == models.DirectionLong {
			if m.OITrend >= models.TrendUp && m.PerpCVDTrend >= models.TrendUp {
				score += 10
				break
			}
		} else {
			if m.OITrend >= models.TrendUp && m.PerpCVDTrend <= models.TrendDown {
				score += 10
				break
			}
		}
	}

	if score > 100 {
		score = 100
	}

	return score
}
