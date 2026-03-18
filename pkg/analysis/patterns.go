package analysis

import (
	"bybit_trader/pkg/models"
)

// PatternDef defines a single OrderFlow pattern's matching criteria.
// Each pattern detects a specific manipulation or smart-money footprint.
type PatternDef struct {
	Name        models.PatternName
	Direction   models.SignalDirection
	Description string
	Match       func(ofs *OrderFlowState) bool
}

// AllPatterns — 8 OrderFlow patterns based on manipulation detection.
// Direction-agnostic detection modules feed into directional patterns.
//
// Detection → Pattern mapping:
//   Spoofing    → SpoofTrapLong / SpoofTrapShort
//   Absorption  → TopAbsorption (SHORT) / BottomAbsorption (LONG)
//   OI Flush    → LongSqueeze (SHORT) / ShortSqueeze (LONG)
//   CVD Diverg  → DeltaDivLong / DeltaDivShort
var AllPatterns = []PatternDef{

	// ═══════════════════════════════════════════════════════════
	// SPOOFING TRAP — Fake wall pulled → trade in manipulator's direction
	// Fake ask wall removed = they want price UP → LONG
	// Fake bid wall removed = they want price DOWN → SHORT
	// ═══════════════════════════════════════════════════════════
	{
		Name:      models.PatternSpoofTrapLong,
		Direction: models.DirectionLong,
		Description: "Spoofing Tuzağı — Sahte ask duvarı yok oldu, " +
			"fiyat yükselmeden direniş kaldırıldı. Manipülatör yukarı yönlü.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.SpoofAskDetected
		},
	},
	{
		Name:      models.PatternSpoofTrapShort,
		Direction: models.DirectionShort,
		Description: "Spoofing Tuzağı — Sahte bid duvarı yok oldu, " +
			"fiyat düşmeden destek kaldırıldı. Manipülatör aşağı yönlü.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.SpoofBidDetected
		},
	},

	// ═══════════════════════════════════════════════════════════
	// ABSORPTION — Hidden player absorbing aggressive flow
	// Buy absorption at top: someone selling into buyers → SHORT
	// Sell absorption at bottom: someone buying into sellers → LONG
	// ═══════════════════════════════════════════════════════════
	{
		Name:      models.PatternTopAbsorption,
		Direction: models.DirectionShort,
		Description: "Tepede Absorpsiyon — Güçlü alış baskısına rağmen fiyat " +
			"yükselemiyor. Büyük satıcı tüm alışları absorbe ediyor.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.BuyAbsorption && ofs.AbsorptionScore >= 0.3
		},
	},
	{
		Name:      models.PatternBottomAbsorption,
		Direction: models.DirectionLong,
		Description: "Dipte Absorpsiyon — Güçlü satış baskısına rağmen fiyat " +
			"düşemiyor. Büyük alıcı tüm satışları absorbe ediyor.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.SellAbsorption && ofs.AbsorptionScore >= 0.3
		},
	},

	// ═══════════════════════════════════════════════════════════
	// OI FLUSH — Liquidation cascade after position build-up
	// Longs flushed (price drop + OI drop) → ride the cascade SHORT
	// Shorts flushed (price rise + OI drop) → ride the cascade LONG
	// ═══════════════════════════════════════════════════════════
	{
		Name:      models.PatternLongSqueeze,
		Direction: models.DirectionShort,
		Description: "Long Squeeze — OI düşüşü + fiyat çöküşü, " +
			"long pozisyonlar tasfiye ediliyor. Likidasyon kaskadı.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.OIFlushing && ofs.OIFlushSide == "long"
		},
	},
	{
		Name:      models.PatternShortSqueeze,
		Direction: models.DirectionLong,
		Description: "Short Squeeze — OI düşüşü + fiyat yükselişi, " +
			"short pozisyonlar tasfiye ediliyor. Likidasyon kaskadı.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.OIFlushing && ofs.OIFlushSide == "short"
		},
	},

	// ═══════════════════════════════════════════════════════════
	// DELTA DIVERGENCE — Spot vs Futures CVD divergence
	// Spot (smart money) buying + Futures flat/selling → LONG
	// Spot (smart money) selling + Futures flat/buying → SHORT
	// ═══════════════════════════════════════════════════════════
	{
		Name:      models.PatternDeltaDivLong,
		Direction: models.DirectionLong,
		Description: "Delta Sapma — Spot piyasada güçlü alım, vadeli " +
			"piyasa geride. Akıllı para birikiyor.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.CVDDivBullish
		},
	},
	{
		Name:      models.PatternDeltaDivShort,
		Direction: models.DirectionShort,
		Description: "Delta Sapma — Spot piyasada güçlü satış, vadeli " +
			"piyasa geride. Akıllı para dağıtıyor.",
		Match: func(ofs *OrderFlowState) bool {
			return ofs.CVDDivBearish
		},
	},
}

// ClassifyPatterns matches OrderFlowState against all pattern definitions
func ClassifyPatterns(ofs *OrderFlowState) []PatternMatch {
	var matches []PatternMatch
	for _, p := range AllPatterns {
		if p.Match(ofs) {
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
