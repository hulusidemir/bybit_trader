package analysis

import (
	"bybit_trader/pkg/models"
)

// PatternDef defines a single pattern's matching criteria
type PatternDef struct {
	Name        models.PatternName
	Direction   models.SignalDirection
	Description string
	Match       func(m *models.TimeframeMetrics) bool
}

// AllPatterns — 6 momentum-focused patterns designed for scalping.
// Each pattern requires:
//   - EMA structure confirmation (EMA9 vs EMA21)
//   - Price trend in the signal direction
//   - At least one confirmation from OI/CVD/OB
//   - Price NOT at range extreme (handled by signal generator)
var AllPatterns = []PatternDef{

	// ═══════════════════════════════════════════════════════════
	// MOMENTUM CONTINUATION — Ride the existing trend
	// All indicators aligned, volume above average
	// ═══════════════════════════════════════════════════════════
	{
		Name:      models.PatternMomentumLong,
		Direction: models.DirectionLong,
		Description: "Momentum Devam — EMA bullish, fiyat yukarı trend, " +
			"OB alıcı baskın, hacim ortalamanın üstünde.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.EMABull &&
				m.PriceTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall &&
				m.VolProfile >= 0.8
		},
	},
	{
		Name:      models.PatternMomentumShort,
		Direction: models.DirectionShort,
		Description: "Momentum Devam — EMA bearish, fiyat aşağı trend, " +
			"OB satıcı baskın, hacim ortalamanın üstünde.",
		Match: func(m *models.TimeframeMetrics) bool {
			return !m.EMABull &&
				m.PriceTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall &&
				m.VolProfile >= 0.8
		},
	},

	// ═══════════════════════════════════════════════════════════
	// BREAKOUT — Strong directional move with OI surge
	// OI increasing = new positions being opened in the direction
	// ═══════════════════════════════════════════════════════════
	{
		Name:      models.PatternBreakoutLong,
		Direction: models.DirectionLong,
		Description: "Breakout Yukarı — OI artıyor, fiyat güçlü yukarı, " +
			"EMA bullish, perp CVD positif.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.EMABull &&
				m.PriceTrend >= models.TrendStrongUp &&
				m.OITrend >= models.TrendUp &&
				(m.PerpCVDTrend >= models.TrendUp || m.PerpCVD == 0)
		},
	},
	{
		Name:      models.PatternBreakoutShort,
		Direction: models.DirectionShort,
		Description: "Breakout Aşağı — OI artıyor, fiyat güçlü aşağı, " +
			"EMA bearish, perp CVD negatif.",
		Match: func(m *models.TimeframeMetrics) bool {
			return !m.EMABull &&
				m.PriceTrend <= models.TrendStrongDown &&
				m.OITrend >= models.TrendUp &&
				(m.PerpCVDTrend <= models.TrendDown || m.PerpCVD == 0)
		},
	},

	// ═══════════════════════════════════════════════════════════
	// CONFLUENCE — Multiple exchange data points confirm
	// EMA + Trend + CVD (spot AND perp) + OB all aligned
	// ═══════════════════════════════════════════════════════════
	{
		Name:      models.PatternConfluenceLong,
		Direction: models.DirectionLong,
		Description: "Çoklu Teyit Yukarı — EMA bullish, perp + spot CVD pozitif, " +
			"OB alıcı baskın, fiyat yükseliyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.EMABull &&
				m.PriceTrend >= models.TrendUp &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend >= models.TrendNeutral &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      models.PatternConfluenceShort,
		Direction: models.DirectionShort,
		Description: "Çoklu Teyit Aşağı — EMA bearish, perp + spot CVD negatif, " +
			"OB satıcı baskın, fiyat düşüyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return !m.EMABull &&
				m.PriceTrend <= models.TrendDown &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend <= models.TrendNeutral &&
				m.OBBias <= models.OBAskWall
		},
	},
}

// ClassifyPatterns finds all matching patterns for the given metrics
func ClassifyPatterns(m *models.TimeframeMetrics) []PatternMatch {
	var matches []PatternMatch
	for _, p := range AllPatterns {
		if p.Match(m) {
			matches = append(matches, PatternMatch{
				Pattern:     p.Name,
				Direction:   p.Direction,
				Description: p.Description,
			})
		}
	}
	return matches
}

type PatternMatch struct {
	Pattern     models.PatternName
	Direction   models.SignalDirection
	Description string
}
