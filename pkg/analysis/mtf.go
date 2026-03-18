package analysis

import (
	"bybit_trader/pkg/models"
)

// MTFResult holds the multi-timeframe confluence analysis
type MTFResult struct {
	Symbol          string
	Direction       models.SignalDirection
	Pattern         models.PatternName
	Description     string
	ConfluenceScore int                              // 0-100
	AlignedTFs      int                              // how many timeframes agree
	TotalTFs        int                              // always 4
	TFDetails       map[string]bool                  // tf -> aligned
	PrimaryTF       string                           // the timeframe that triggered the signal
	Metrics         map[string]*models.TimeframeMetrics
	HasOBSupport    bool                             // orderbook confirms direction
	HasCVDConfirm   bool                             // both perp+spot CVD confirm
	HasEMAConfirm   bool                             // EMA structure confirms
}

// AnalyzeMTF performs multi-timeframe confluence analysis
func AnalyzeMTF(analysis *models.CoinAnalysis) []MTFResult {
	var results []MTFResult

	// For each timeframe, check which patterns match
	tfPatterns := make(map[string][]PatternMatch)
	for tf, metrics := range analysis.Metrics {
		matches := ClassifyPatterns(metrics)
		if len(matches) > 0 {
			tfPatterns[tf] = matches
		}
	}

	if len(tfPatterns) == 0 {
		return nil
	}

	// Find patterns that appear across multiple timeframes
	patternTFs := make(map[models.PatternName]map[string]bool)
	patternInfo := make(map[models.PatternName]PatternMatch)

	for tf, matches := range tfPatterns {
		for _, m := range matches {
			if _, ok := patternTFs[m.Pattern]; !ok {
				patternTFs[m.Pattern] = make(map[string]bool)
				patternInfo[m.Pattern] = m
			}
			patternTFs[m.Pattern][tf] = true
		}
	}

	// Also check for direction-level alignment (different patterns, same direction)
	dirTFs := make(map[models.SignalDirection]map[string]bool)
	dirBestPattern := make(map[models.SignalDirection]PatternMatch)
	for tf, matches := range tfPatterns {
		for _, m := range matches {
			if _, ok := dirTFs[m.Direction]; !ok {
				dirTFs[m.Direction] = make(map[string]bool)
				dirBestPattern[m.Direction] = m
			}
			dirTFs[m.Direction][tf] = true
		}
	}

	// Score and rank — use direction-level confluence (more flexible than exact pattern match)
	for dir, tfs := range dirTFs {
		info := dirBestPattern[dir]
		aligned := len(tfs)

		// Minimum 2 timeframe alignment required
		if aligned < 2 {
			continue
		}

		// Check confirmations
		hasOBSupport := checkOBSupport(analysis.Metrics, dir)
		hasCVDConfirm := checkCVDConfirmation(analysis.Metrics, dir)
		hasEMAConfirm := checkEMAConfirmation(analysis.Metrics, dir)

		// Confluence score
		score := calcConfluenceScore(aligned, tfs, analysis.Metrics, dir, hasOBSupport, hasCVDConfirm, hasEMAConfirm)

		// Minimum score 70
		if score < 70 {
			continue
		}

		// Find primary (lowest) timeframe for entry precision
		primaryTF := "240"
		for _, tf := range []string{"5", "15", "60", "240"} {
			if tfs[tf] {
				primaryTF = tf
				break
			}
		}

		results = append(results, MTFResult{
			Symbol:          analysis.Symbol,
			Direction:       dir,
			Pattern:         info.Pattern,
			Description:     info.Description,
			ConfluenceScore: score,
			AlignedTFs:      aligned,
			TotalTFs:        4,
			TFDetails:       tfs,
			PrimaryTF:       primaryTF,
			Metrics:         analysis.Metrics,
			HasOBSupport:    hasOBSupport,
			HasCVDConfirm:   hasCVDConfirm,
			HasEMAConfirm:   hasEMAConfirm,
		})
	}

	return results
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
	spotConfirm := false
	noCVDData := true

	for _, m := range metrics {
		if m.PerpCVD != 0 || m.SpotCVD != 0 {
			noCVDData = false
		}

		if dir == models.DirectionLong {
			if m.PerpCVDTrend >= models.TrendUp {
				perpConfirm = true
			}
			if m.SpotCVDTrend >= models.TrendNeutral {
				spotConfirm = true
			}
		} else {
			if m.PerpCVDTrend <= models.TrendDown {
				perpConfirm = true
			}
			if m.SpotCVDTrend <= models.TrendNeutral {
				spotConfirm = true
			}
		}
	}

	if noCVDData {
		return true
	}

	return perpConfirm && spotConfirm
}

// checkEMAConfirmation verifies EMA structure across timeframes
func checkEMAConfirmation(metrics map[string]*models.TimeframeMetrics, dir models.SignalDirection) bool {
	confirmedTFs := 0
	totalTFs := 0

	for _, m := range metrics {
		if m.EMAFast == 0 && m.EMASlow == 0 {
			continue
		}
		totalTFs++
		if dir == models.DirectionLong && m.EMABull {
			confirmedTFs++
		}
		if dir == models.DirectionShort && !m.EMABull {
			confirmedTFs++
		}
	}

	if totalTFs == 0 {
		return false
	}
	// At least half of timeframes must have correct EMA structure
	return confirmedTFs*2 >= totalTFs
}

func calcConfluenceScore(
	aligned int,
	tfs map[string]bool,
	metrics map[string]*models.TimeframeMetrics,
	dir models.SignalDirection,
	hasOBSupport bool,
	hasCVDConfirm bool,
	hasEMAConfirm bool,
) int {
	score := 0

	// ── Base: timeframe alignment (max 30) ─────────
	switch aligned {
	case 4:
		score += 30
	case 3:
		score += 22
	case 2:
		score += 14
	}

	// ── HTF alignment bonus (max 15) ───────────────
	if tfs["240"] {
		score += 10
	}
	if tfs["60"] {
		score += 5
	}

	// ── EMA structure (max 15) ─────────────────────
	// EMA alignment is the most critical indicator
	if hasEMAConfirm {
		score += 15
	}

	// ── OI-CVD Momentum (max 15) ───────────────────
	divScore := 0
	for _, m := range metrics {
		if dir == models.DirectionLong {
			if m.OITrend >= models.TrendUp && m.PerpCVDTrend >= models.TrendUp {
				divScore += 5
			}
			if m.SpotCVDTrend >= models.TrendUp {
				divScore += 3
			}
		} else {
			if m.OITrend >= models.TrendUp && m.PerpCVDTrend <= models.TrendDown {
				divScore += 5
			}
			if m.SpotCVDTrend <= models.TrendDown {
				divScore += 3
			}
		}
	}
	if divScore > 15 {
		divScore = 15
	}
	score += divScore

	// ── Orderbook confirmation (max 10) ────────────
	if hasOBSupport {
		score += 10
	}

	// ── CVD dual confirmation (max 10) ─────────────
	if hasCVDConfirm {
		score += 10
	}

	// ── Volume profile bonus (max 5) ───────────────
	for _, m := range metrics {
		if m.VolProfile >= 1.2 {
			score += 5
			break
		}
	}

	if score > 100 {
		score = 100
	}

	return score
}
