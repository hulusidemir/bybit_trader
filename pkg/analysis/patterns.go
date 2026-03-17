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
		Direction: models.DirectionShort,
		Description: "Spot birik im + perp hedge + bid wall: piyasa tek yönlü bullish. " +
			"Aşırı iyimserlik tükenme noktasına işaret eder — büyük oyuncular çıkış likiditesi oluşturuyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      models.PatternAggressiveDistro,
		Direction: models.DirectionLong,
		Description: "Perp alım + spot satış + ask wall: dağıtım gibi görünüyor. " +
			"Ancak bu yapı genellikle geçici — satış baskısı tükendiğinde sert yukarı dönüş.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},
	{
		Name:      models.PatternWhaleSqueezeSetup,
		Direction: models.DirectionShort,
		Description: "OI çok yüksek, perp CVD çok negatif, ask tarafı ince: herkes squeeze bekliyor. " +
			"Kalabalık aynı yönde beklediğinde genellikle tersi gerçekleşir.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendStrongUp &&
				m.PerpCVDTrend <= models.TrendStrongDown &&
				m.SpotCVDTrend == models.TrendNeutral &&
				m.OBBias <= models.OBAskWall // thin ask = easy to push up
		},
	},
	{
		Name:      models.PatternCapitulationReversal,
		Direction: models.DirectionShort,
		Description: "OI düşüyor, spot alım var, bid wall güçlü: 'dip toplama' modu. " +
			"Erken alıcılar genellikle tuzağa düşer — kapitülasyon henüz bitmemiş olabilir.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendStrongDown &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      models.PatternSmartMoneyShort,
		Direction: models.DirectionLong,
		Description: "OI artıyor, spot'ta agresif satış, ask wall baskın. " +
			"Short kalabalığı aşırı büyüdüğünde sıkıştırma riski artar.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend == models.TrendNeutral &&
				m.SpotCVDTrend <= models.TrendStrongDown &&
				m.OBBias <= models.OBAskWall
		},
	},

	{
		Name:      models.PatternSilentDistribution,
		Direction: models.DirectionLong,
		Description: "OI sabit, perp alım var, spot satış devam ediyor. " +
			"Sessiz dağıtım gibi görünüyor ama perp alım baskısı fiyatı yukarı itecek güç biriktiriyor.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend == models.TrendNeutral &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},
	{
		Name:      models.PatternDivergentStrength,
		Direction: models.DirectionShort,
		Description: "Tüm metrikler bullish: OI↑, Perp↑, Spot↑, Bid wall. " +
			"Herkes aynı yönde — tek yönlü konsensüs tükenme noktasına işaret eder.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      models.PatternBearishConvergence,
		Direction: models.DirectionLong,
		Description: "OI artıyor, perp ve spot CVD negatif, ask wall baskın. " +
			"Aşırı bearish konsensüs genellikle dip oluşumu sinyalidir — kalabalığın tersine pozisyon.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend >= models.TrendUp &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},

	{
		Name:      models.PatternLiqCascadeShort,
		Direction: models.DirectionLong,
		Description: "OI hızla düşüyor, perp ve spot CVD çok negatif, ask wall baskın. " +
			"Tasfiye kaskadı — agresif satış tükendiğinde sert toparlanma gelir.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendStrongDown &&
				m.PerpCVDTrend <= models.TrendStrongDown &&
				m.SpotCVDTrend <= models.TrendDown &&
				m.OBBias <= models.OBAskWall
		},
	},
	{
		Name:      models.PatternLiqCascadeLong,
		Direction: models.DirectionShort,
		Description: "OI düşüyor, perp CVD çok pozitif, spot alım var. " +
			"Short squeeze devam ediyor — ancak squeeze tükendiğinde sert geri çekilme beklentisi.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendStrongDown &&
				m.PerpCVDTrend >= models.TrendStrongUp &&
				m.SpotCVDTrend >= models.TrendUp &&
				m.OBBias <= models.OBAskWall // thin ask = easy to break
		},
	},
	{
		Name:      models.PatternAbsorption,
		Direction: models.DirectionShort,
		Description: "OI sabit, perp CVD pozitif, devasa bid wall. " +
			"Satış emiliyor gibi görünüyor ama bid wall kırıldığında likidite kaskadı tetiklenebilir.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend == models.TrendNeutral &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend == models.TrendNeutral &&
				m.OBBias >= models.OBBidHeavy
		},
	},
	{
		Name:      models.PatternHiddenSelling,
		Direction: models.DirectionLong,
		Description: "OI sabit, perp CVD negatif, devasa ask wall. " +
			"Alım emiliyor gibi görünüyor ama ask wall kırıldığında short squeeze tetiklenebilir.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend == models.TrendNeutral &&
				m.PerpCVDTrend <= models.TrendDown &&
				m.SpotCVDTrend == models.TrendNeutral &&
				m.OBBias <= models.OBAskHeavy
		},
	},
	{
		Name:      models.PatternExhaustionTop,
		Direction: models.DirectionLong,
		Description: "OI düşerken perp CVD pozitif: 'son nefes pompası' gibi görünüyor. " +
			"Ancak pozisyon kapanırken bile alım baskısı var — momentum devam edebilir.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.OITrend <= models.TrendDown &&
				m.PerpCVDTrend >= models.TrendUp &&
				m.SpotCVDTrend <= models.TrendDown
		},
	},
	{
		Name:      models.PatternExhaustionBottom,
		Direction: models.DirectionShort,
		Description: "OI düşerken spot CVD pozitif, bid wall güçlü: 'dip toplama' modu. " +
			"Erken alıcılar genellikle tuzağa düşer — düşüş devam edebilir.",
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
		Direction: models.DirectionShort,
		Description: "Sadece Bybit: Güçlü OI artışı + devasa Bid wall. " +
			"Piyasa tek yönlü bullish — aşırı iyimserlik tükenme sinyali.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.SpotCVD == 0 && m.PerpCVD == 0 && // Only trigger if Binance data is missing
				m.OITrend >= models.TrendStrongUp &&
				m.OBBias >= models.OBBidHeavy
		},
	},
	{
		Name:      "Pure Bybit Momentum Short",
		Direction: models.DirectionLong,
		Description: "Sadece Bybit: Güçlü OI artışı + devasa Ask wall. " +
			"Piyasa tek yönlü bearish — aşırı kötümserlik dönüş sinyali.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.SpotCVD == 0 && m.PerpCVD == 0 && // Only trigger if Binance data is missing
				m.OITrend >= models.TrendStrongUp &&
				m.OBBias <= models.OBAskHeavy
		},
	},
	{
		Name:      "Pure Bybit Funding Exhaustion",
		Direction: models.DirectionShort,
		Description: "Sadece Bybit: Aşırı negatif funding + bid wall güçlü. " +
			"Herkes squeeze bekliyor — kalabalığın beklentisi genellikle yanlış çıkar.",
		Match: func(m *models.TimeframeMetrics) bool {
			return m.SpotCVD == 0 && m.PerpCVD == 0 && // Only trigger if Binance data is missing
				m.FundingRate < -0.001 &&
				m.OITrend >= models.TrendUp &&
				m.OBBias >= models.OBBidWall
		},
	},
	{
		Name:      "Pure Bybit Funding Exhaustion Short",
		Direction: models.DirectionLong,
		Description: "Sadece Bybit: Aşırı pozitif funding + ask wall baskın. " +
			"Herkes dump bekliyor — aşırı tek yönlü beklenti dönüş noktasına işaret eder.",
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
