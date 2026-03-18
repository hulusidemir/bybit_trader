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
	ConfluenceScore int                          // 0-100
	AlignedTFs      int                          // how many timeframes agree
	TotalTFs        int                          // always 4
	TFDetails       map[string]bool              // tf -> aligned
	PrimaryTF       string                       // the timeframe that triggered the signal
	Metrics         map[string]*models.TimeframeMetrics
	HasOBSupport    bool                         // orderbook confirms direction
	HasCVDConfirm   bool                         // both perp+spot CVD confirm
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

	// Score and rank
	for pattern, tfs := range patternTFs {
		info := patternInfo[pattern]
		aligned := len(tfs)

		// ══════════════════════════════════════════════════
		// STRICT: Minimum 2 timeframe alignment required
		// Single-TF patterns are noise, not signals
		// ══════════════════════════════════════════════════
		if aligned < 2 {
			continue
		}

		// Check orderbook and CVD confirmation
		hasOBSupport := checkOBSupport(analysis.Metrics, info.Direction)
		hasCVDConfirm := checkCVDConfirmation(analysis.Metrics, info.Direction)

		// Confluence score calculation
		score := calcConfluenceScore(aligned, tfs, analysis.Metrics, info.Direction, hasOBSupport, hasCVDConfirm)

		// ══════════════════════════════════════════════════
		// STRICT: Minimum score 75 — higher quality filter
		// Only 75+ passes as A, 85+ as A+
		// ══════════════════════════════════════════════════
		if score < 75 {
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
			Direction:       info.Direction,
			Pattern:         pattern,
			Description:     info.Description,
			ConfluenceScore: score,
			AlignedTFs:      aligned,
			TotalTFs:        4,
			TFDetails:       tfs,
			PrimaryTF:       primaryTF,
			Metrics:         analysis.Metrics,
			HasOBSupport:    hasOBSupport,
			HasCVDConfirm:   hasCVDConfirm,
		})
	}

	return results
}

func checkOBSupport(metrics map[string]*models.TimeframeMetrics, dir models.SignalDirection) bool {
	for _, m := range metrics {
		// Momentum LONG: bid wall = alıcılar güçlü, destek var
		if dir == models.DirectionLong && m.OBBias >= models.OBBidWall {
			return true
		}
		// Momentum SHORT: ask wall = satıcılar güçlü, direnç var
		if dir == models.DirectionShort && m.OBBias <= models.OBAskWall {
			return true
		}
	}
	return false
}

func checkCVDConfirmation(metrics map[string]*models.TimeframeMetrics, dir models.SignalDirection) bool {
	perpConfirm := false
	spotConfirm := false
	noCVDData := true // track if any CVD data exists

	for _, m := range metrics {
		if m.PerpCVD != 0 || m.SpotCVD != 0 {
			noCVDData = false
		}

		if dir == models.DirectionLong {
			// Momentum LONG: perp alım baskısı olmalı
			if m.PerpCVDTrend >= models.TrendUp {
				perpConfirm = true
			}
			// Spot nötr veya alım olmalı (satış olmamalı)
			if m.SpotCVDTrend >= models.TrendNeutral {
				spotConfirm = true
			}
		} else {
			// Momentum SHORT: perp satış baskısı olmalı
			if m.PerpCVDTrend <= models.TrendDown {
				perpConfirm = true
			}
			// Spot nötr veya satış olmalı (alım olmamalı)
			if m.SpotCVDTrend <= models.TrendNeutral {
				spotConfirm = true
			}
		}
	}

	// Pure Bybit coinlerinde CVD verisi yoksa bu kontrolü bypass et
	if noCVDData {
		return true
	}

	return perpConfirm && spotConfirm
}

func calcConfluenceScore(
	aligned int,
	tfs map[string]bool,
	metrics map[string]*models.TimeframeMetrics,
	dir models.SignalDirection,
	hasOBSupport bool,
	hasCVDConfirm bool,
) int {
	score := 0

	// ── Base: timeframe alignment (max 40) ─────────
	// Strict: single TF gets nothing (filtered above)
	switch aligned {
	case 4:
		score += 40 // All 4 TFs agree — very strong
	case 3:
		score += 28 // 3/4 — solid
	case 2:
		score += 18 // 2/4 — minimum viable
	}

	// ── HTF alignment bonus (max 20) ───────────────
	// Higher timeframes carry more weight — institutional alignment
	if tfs["240"] {
		score += 14 // 4H alignment is critical
	}
	if tfs["60"] {
		score += 6 // 1H adds confluence
	}

	// ── OI-CVD Momentum strength (max 15) ──────────
	// Trend-following: OI + CVD aynı yönde = güçlü momentum
	divScore := 0
	for _, m := range metrics {
		if dir == models.DirectionLong {
			// LONG: OI artıyor + perp alım baskısı = momentum güçlü
			if m.OITrend >= models.TrendUp && m.PerpCVDTrend >= models.TrendUp {
				divScore += 5
			}
			// Spot alım desteği = ek teyit
			if m.SpotCVDTrend >= models.TrendUp {
				divScore += 3
			}
		} else {
			// SHORT: OI artıyor + perp satış baskısı = momentum güçlü
			if m.OITrend >= models.TrendUp && m.PerpCVDTrend <= models.TrendDown {
				divScore += 5
			}
			// Spot satış desteği = ek teyit
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
	// Both perp AND spot CVD supporting the direction — very strong
	if hasCVDConfirm {
		score += 10
	}

	// ── Funding rate alignment (max 5) ─────────────
	// Counter-funding positions have edge
	for _, m := range metrics {
		if dir == models.DirectionLong && m.FundingRate < -0.0002 {
			score += 5 // Negative funding = shorts paying longs
			break
		}
		if dir == models.DirectionShort && m.FundingRate > 0.0003 {
			score += 5 // Very positive = longs overleveraged
			break
		}
	}

	if score > 100 {
		score = 100
	}

	return score
}
