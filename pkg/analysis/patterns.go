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

// AllPatterns are the 22+ named pattern definitions
var AllPatterns = []PatternDef{
	{
		Name:      models.PatternStealthAccumulation,
		Direction: models.DirectionLong,
		Description: "Spot birikim + perp hedge + bid wall: piyasa tek yönlü bullish. " +
			"Trend takip — güçlü alım baskısı momentum devam sinyali.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      models.PatternAggressiveDistro,
		Direction: models.DirectionShort,
		Description: "Perp alım + spot satış + ask wall: dağıtım devam ediyor. " +
			"Trend takip — spot satış baskısı aşağı yönlü momentum sinyali.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},
	{
		Name:      models.PatternWhaleSqueezeSetup,
		Direction: models.DirectionLong,
		Description: "OI çok yüksek, perp CVD pozitif, bid wall güçlü. " +
			"Trend takip — alıcılar agresif, yukarı momentum devam ediyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendStrongUp &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall // bid wall = alıcı desteği var
		},
	},
	{
		Name:      models.PatternCapitulationReversal,
		Direction: models.DirectionLong,
		Description: "OI düşüyor, spot alım var, bid wall güçlü: dip toplama modu. " +
			"Trend takip — spot alım baskısı ve bid wall yukarı destek sinyali.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendStrongDown &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      models.PatternSmartMoneyShort,
		Direction: models.DirectionShort,
		Description: "OI artıyor, spot'ta agresif satış, ask wall baskın. " +
			"Trend takip — güçlü satış baskısı aşağı yönlü momentum devam sinyali.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend == models.TrendNeutral &&
				m.SpotCVDTrend <= models.TrendStrongDown &&
				m.OBBias <= models.OBAskWall
		},
	},

	{
		Name:      models.PatternSilentDistribution,
		Direction: models.DirectionShort,
		Description: "OI sabit, perp alım var, spot satış devam ediyor. " +
			"Trend takip — spot satış baskısı devam ediyor, aşağı yönlü momentum.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend == models.TrendNeutral &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},
	{
		Name:      models.PatternDivergentStrength,
		Direction: models.DirectionLong,
		Description: "Tüm metrikler bullish: OI↑, Perp↑, Spot↑, Bid wall. " +
			"Trend takip — tüm göstergeler yukarı yönlü, güçlü momentum.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      models.PatternBearishConvergence,
		Direction: models.DirectionShort,
		Description: "OI artıyor, perp ve spot CVD negatif, ask wall baskın. " +
			"Trend takip — tüm göstergeler aşağı yönlü, satış momentum devam ediyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},

	{
		Name:      models.PatternLiqCascadeShort,
		Direction: models.DirectionShort,
		Description: "OI hızla düşüyor, perp ve spot CVD çok negatif, ask wall baskın. " +
			"Trend takip — tasfiye kaskadı devam ediyor, aşağı momentum güçlü.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendStrongDown &&
				m.PerpCVDTrend <= models.TrendStrongDown &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},
	{
		Name:      models.PatternLiqCascadeLong,
		Direction: models.DirectionLong,
		Description: "OI düşüyor, perp CVD çok pozitif, spot alım var, bid wall güçlü. " +
			"Trend takip — short tasfiyesi devam ediyor, yukarı momentum güçlü.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendStrongDown &&
				m.PerpCVDTrend >= models.TrendStrongUp &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall // bid wall = alım desteği var
		},
	},
	{
		Name:      models.PatternAbsorption,
		Direction: models.DirectionLong,
		Description: "OI sabit, perp CVD pozitif, devasa bid wall. " +
			"Trend takip — alım emilimi devam ediyor, yukarı yönlü destek güçlü.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend == models.TrendNeutral &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend == models.TrendNeutral &&
				m.OBBias >= models.OBBidHeavy
		},
	},
	{
		Name:      models.PatternHiddenSelling,
		Direction: models.DirectionShort,
		Description: "OI sabit, perp CVD negatif, devasa ask wall. " +
			"Trend takip — satış baskısı devam ediyor, aşağı yönlü momentum.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend == models.TrendNeutral &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend == models.TrendNeutral &&
				m.OBBias <= models.OBAskHeavy
		},
	},
	{
		Name:      models.PatternExhaustionTop,
		Direction: models.DirectionShort,
		Description: "OI düşerken perp CVD pozitif, spot satış, ask wall baskın. " +
			"Trend takip — son alıcılar tükeniyor, aşağı dönüş beklenir.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendDown &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall // ask wall = satıcı direnci
		},
	},
	{
		Name:      models.PatternExhaustionBottom,
		Direction: models.DirectionLong,
		Description: "OI düşerken spot CVD pozitif, bid wall güçlü: dip toplama modu. " +
			"Trend takip — spot alım ve bid wall desteği yukarı dönüş sinyali.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendDown &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},

	// ── Pure Bybit Patterns (No Binance CVD Required) ──────────
	// These patterns allow coins listed only on Bybit to trigger signals
	// by compensating for the lack of CVD with stronger OI and OB requirements.
	{
		Name:      "Pure Bybit Momentum Long",
		Direction: models.DirectionLong,
		Description: "Sadece Bybit: Güçlü OI artışı + devasa Bid wall. " +
			"Trend takip — güçlü alım baskısı, yukarı momentum devam ediyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.SpotCVD == 0 && m.PerpCVD == 0 && // Only trigger if Binance data is missing
				m.OITrend >= models.TrendStrongUp &&
				m.OBBias >= models.OBBidHeavy
		},
	},
	{
		Name:      "Pure Bybit Momentum Short",
		Direction: models.DirectionShort,
		Description: "Sadece Bybit: Güçlü OI artışı + devasa Ask wall. " +
			"Trend takip — güçlü satış baskısı, aşağı momentum devam ediyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.SpotCVD == 0 && m.PerpCVD == 0 && // Only trigger if Binance data is missing
				m.OITrend >= models.TrendStrongUp &&
				m.OBBias <= models.OBAskHeavy
		},
	},
	{
		Name:      "Pure Bybit Funding Exhaustion",
		Direction: models.DirectionLong,
		Description: "Sadece Bybit: Aşırı negatif funding + bid wall güçlü. " +
			"Trend takip — negatif funding short baskısını gösteriyor, squeeze potansiyeli.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.SpotCVD == 0 && m.PerpCVD == 0 && // Only trigger if Binance data is missing
				m.FundingRate < -0.001 &&
				m.OITrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      "Pure Bybit Funding Exhaustion Short",
		Direction: models.DirectionShort,
		Description: "Sadece Bybit: Aşırı pozitif funding + ask wall baskın. " +
			"Trend takip — pozitif funding long baskısını gösteriyor, düşüş potansiyeli.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.SpotCVD == 0 && m.PerpCVD == 0 && // Only trigger if Binance data is missing
				m.FundingRate > 0.001 &&
				m.OITrend >= models.TrendUp &&
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
