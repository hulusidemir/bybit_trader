package signals

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"bybit_trader/pkg/analysis"
	"bybit_trader/pkg/models"
)

// TPConfig holds configurable TP/DCA percentages
type TPConfig struct {
	TP1Pct                float64            // e.g. 1.0
	TP2Pct                float64            // e.g. 2.5
	TP3Pct                float64            // e.g. 5.0
	ATRTP2Mult            float64            // e.g. 3.5
	ATRTP3Mult            float64            // e.g. 6.0
	DCAThresholdPct       float64            // default DCA %, e.g. 20.0
	CoinDCAOverrides      map[string]float64 // per-coin DCA overrides
	ShortFundingRateLimit float64            // skip SHORT if funding < this (e.g. -0.0001)
}

// DCAForCoin returns the DCA % for a given symbol (override or default)
func (t TPConfig) DCAForCoin(symbol string) float64 {
	if pct, ok := t.CoinDCAOverrides[symbol]; ok && pct > 0 {
		return pct
	}
	return t.DCAThresholdPct
}

// DefaultTPConfig returns sensible defaults
func DefaultTPConfig() TPConfig {
	return TPConfig{
		TP1Pct:                1.0,
		TP2Pct:                2.5,
		TP3Pct:                5.0,
		ATRTP2Mult:            3.5,
		ATRTP3Mult:            6.0,
		DCAThresholdPct:       20.0,
		ShortFundingRateLimit: -0.008,
	}
}

// GenerateSignals creates trade signals from MTF analysis results
// STRICT: Only passes through the highest quality setups
func GenerateSignals(mtfResults []analysis.MTFResult, tpCfg TPConfig) []*models.Signal {
	var signals []*models.Signal

	for _, r := range mtfResults {
		sig := buildSignal(r, tpCfg)
		if sig == nil {
			continue
		}

		// ══════════════════════════════════════════════════
		// QUALITY GATE 1: Minimum Grade A (score >= 70)
		// ══════════════════════════════════════════════════
		if sig.Grade != models.GradeAPlus && sig.Grade != models.GradeA {
			continue
		}

		// (R:R zorunluluğu kullanıcının isteği üzerine tamamen kaldırıldı)

		// ══════════════════════════════════════════════════
		// QUALITY GATE 2: Funding rate filter for SHORT
		// Very negative funding = market already heavily short,
		// high short squeeze risk + expensive funding cost
		// ══════════════════════════════════════════════════
		if sig.Direction == models.DirectionShort && tpCfg.ShortFundingRateLimit < 0 && sig.FundingRate < tpCfg.ShortFundingRateLimit {
			continue
		}

		// ══════════════════════════════════════════════════
		// QUALITY GATE 3: Require orderbook support
		// ══════════════════════════════════════════════════
		if !r.HasOBSupport {
			continue
		}

		// ══════════════════════════════════════════════════
		// QUALITY GATE 4: Minimum 2 TF alignment
		// ══════════════════════════════════════════════════
		if r.AlignedTFs < 2 {
			continue
		}

		// ══════════════════════════════════════════════════
		// QUALITY GATE 5: Price trend must confirm direction
		// Fiyat trendi sinyal yönüyle aynı olmalı.
		// LONG için fiyat yukarı gidiyor olmalı, SHORT için aşağı.
		// ══════════════════════════════════════════════════
		if !checkPriceTrendConfirm(r, sig.Direction) {
			continue
		}

		// ══════════════════════════════════════════════════
		// QUALITY GATE 6: Price range position filter
		// Tepede LONG açma, dipte SHORT açma.
		// LONG: fiyat aralığın üst %75'inde olmamalı
		// SHORT: fiyat aralığın alt %25'inde olmamalı
		// ══════════════════════════════════════════════════
		if !checkPriceRangeFilter(r, sig.Direction) {
			continue
		}

		signals = append(signals, sig)
	}

	return signals
}

// checkPriceTrendConfirm ensures both 5m AND 15m show price moving in signal direction.
// Scalper needs short-term momentum confirmation from both low timeframes.
func checkPriceTrendConfirm(r analysis.MTFResult, dir models.SignalDirection) bool {
	m5, has5 := r.Metrics["5"]
	m15, has15 := r.Metrics["15"]

	// Both 5m and 15m must exist and confirm
	if !has5 || !has15 {
		return false
	}

	if dir == models.DirectionLong {
		return m5.PriceTrend >= models.TrendUp && m15.PriceTrend >= models.TrendUp
	}
	return m5.PriceTrend <= models.TrendDown && m15.PriceTrend <= models.TrendDown
}

// checkPriceRangeFilter prevents buying at top and shorting at bottom.
// Uses 15m timeframe for range context (~7.5 hours with 30 candles).
func checkPriceRangeFilter(r analysis.MTFResult, dir models.SignalDirection) bool {
	// Primary: 15m range (30 candles = 7.5h context)
	// Fallback: 60m if 15m unavailable
	var m *models.TimeframeMetrics
	if m15, ok := r.Metrics["15"]; ok && m15.PriceRangePos != 50 {
		m = m15
	} else if m60, ok := r.Metrics["60"]; ok && m60.PriceRangePos != 50 {
		m = m60
	}
	if m == nil {
		return true // no range data = allow
	}

	if dir == models.DirectionLong && m.PriceRangePos > 80 {
		return false // fiyat zaten tepede — LONG açma
	}
	if dir == models.DirectionShort && m.PriceRangePos < 20 {
		return false // fiyat zaten dipte — SHORT açma
	}
	return true
}

func buildSignal(r analysis.MTFResult, tpCfg TPConfig) *models.Signal {
	// Get the primary TF metrics
	m, ok := r.Metrics[r.PrimaryTF]
	if !ok {
		return nil
	}

	price := m.LastPrice
	if price == 0 {
		return nil
	}

	// ── Grade thresholds ────────────────────────────
	grade := models.GradeB
	if r.ConfluenceScore >= 85 {
		grade = models.GradeAPlus
	} else if r.ConfluenceScore >= 75 {
		grade = models.GradeA
	}

	// Calculate entry zone, TP based on direction (no SL — DCA strategy)
	var entryLow, entryHigh, tp1, tp2, tp3 float64

	if r.Direction == models.DirectionLong {
		entryLow, entryHigh, tp1, tp2, tp3 = calcLongLevels(price, m, tpCfg)
	} else {
		entryLow, entryHigh, tp1, tp2, tp3 = calcShortLevels(price, m, tpCfg)
	}

	// DCA level (per-coin override or default)
	entryMid := (entryLow + entryHigh) / 2
	dcaPct := tpCfg.DCAForCoin(r.Symbol)
	var dcaLevel float64
	if r.Direction == models.DirectionLong {
		dcaLevel = entryMid * (1 - dcaPct/100)
	} else {
		dcaLevel = entryMid * (1 + dcaPct/100)
	}

	// Determine L/S ratio from any available timeframe
	lsRatio := 0.0
	for _, mx := range r.Metrics {
		if mx.LSRatio > 0 {
			lsRatio = mx.LSRatio
			break
		}
	}

	return &models.Signal{
		ID:              generateID(),
		Symbol:          r.Symbol,
		Direction:       r.Direction,
		Pattern:         r.Pattern,
		Grade:           grade,
		Confidence:      r.ConfluenceScore,
		EntryLow:        entryLow,
		EntryHigh:       entryHigh,
		StopLoss:        0, // No SL — DCA strategy
		TP1:             tp1,
		TP2:             tp2,
		TP3:             tp3,
		RiskRewardTP1:   0,
		RiskRewardTP2:   0,
		RiskRewardTP3:   0,
		DCALevel:        dcaLevel,
		Explanation:     r.Description,
		Metrics:         r.Metrics,
		LSRatio:         lsRatio,
		Volume24h:       m.Volume24h,
		FundingRate:     m.FundingRate,
		NextFundingTime: m.NextFundingTime,
		FundingInterval: m.FundingInterval,
		Timestamp:       time.Now(),
	}
}

// ════════════════════════════════════════════════════════════
// CORE FIX: TP/SL are now percentage-based with hard minimums
// No more 0.05% TPs — institutional-grade scalp levels
// ════════════════════════════════════════════════════════════

func calcLongLevels(price float64, m *models.TimeframeMetrics, tpCfg TPConfig) (entryLow, entryHigh, tp1, tp2, tp3 float64) {
	// Entry zone: tight range around current price
	if m.BidWallPrice > 0 && m.BidWallPrice < price && m.BidWallPrice > price*0.99 {
		entryLow = m.BidWallPrice
		entryHigh = price
	} else {
		entryLow = price * 0.998 // 0.2% below
		entryHigh = price * 1.001
	}

	entryMid := (entryLow + entryHigh) / 2

	// ── TP1: configurable % from entry ──────────────────
	tp1 = entryMid * (1 + tpCfg.TP1Pct/100)

	// ── TP2: ATR-based with minimum floor ─────────────
	if m.ATR > 0 {
		tp2 = entryMid + m.ATR*tpCfg.ATRTP2Mult
	} else {
		tp2 = entryMid * (1 + tpCfg.TP2Pct/100)
	}
	if tp2 < entryMid*(1+tpCfg.TP2Pct/100) {
		tp2 = entryMid * (1 + tpCfg.TP2Pct/100)
	}

	// ── TP3: ATR-based with minimum floor ─────────────
	if m.ATR > 0 {
		tp3 = entryMid + m.ATR*tpCfg.ATRTP3Mult
	} else {
		tp3 = entryMid * (1 + tpCfg.TP3Pct/100)
	}
	if tp3 < entryMid*(1+tpCfg.TP3Pct/100) {
		tp3 = entryMid * (1 + tpCfg.TP3Pct/100)
	}

	return
}

func calcShortLevels(price float64, m *models.TimeframeMetrics, tpCfg TPConfig) (entryLow, entryHigh, tp1, tp2, tp3 float64) {
	// Entry zone
	if m.AskWallPrice > 0 && m.AskWallPrice > price && m.AskWallPrice < price*1.01 {
		entryLow = price
		entryHigh = m.AskWallPrice
	} else {
		entryLow = price * 0.999
		entryHigh = price * 1.002 // 0.2% above
	}

	entryMid := (entryLow + entryHigh) / 2

	// ── TP1: configurable % from entry ──────────────────
	tp1 = entryMid * (1 - tpCfg.TP1Pct/100)

	// ── TP2: ATR-based with minimum floor ─────────────
	if m.ATR > 0 {
		tp2 = entryMid - m.ATR*tpCfg.ATRTP2Mult
	} else {
		tp2 = entryMid * (1 - tpCfg.TP2Pct/100)
	}
	if tp2 > entryMid*(1-tpCfg.TP2Pct/100) {
		tp2 = entryMid * (1 - tpCfg.TP2Pct/100)
	}

	// ── TP3: ATR-based with minimum floor ─────────────
	if m.ATR > 0 {
		tp3 = entryMid - m.ATR*tpCfg.ATRTP3Mult
	} else {
		tp3 = entryMid * (1 - tpCfg.TP3Pct/100)
	}
	if tp3 > entryMid*(1-tpCfg.TP3Pct/100) {
		tp3 = entryMid * (1 - tpCfg.TP3Pct/100)
	}

	return
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
