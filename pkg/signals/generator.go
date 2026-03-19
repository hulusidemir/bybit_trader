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

		// ══════════════════════════════════════════════════
		// QUALITY GATE 2: Funding rate filter for SHORT
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
		// QUALITY GATE 4: CVD confirmation (spot + futures sync)
		// ══════════════════════════════════════════════════
		if !r.HasCVDConfirm {
			continue
		}

		signals = append(signals, sig)
	}

	return signals
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
	} else if r.ConfluenceScore >= 70 {
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
	// Aggressive limit: enter at current price (BestAsk)
	entryLow = price
	entryHigh = price

	// Percentage-based TP levels
	tp1 = price * (1 + tpCfg.TP1Pct/100)
	tp2 = price * (1 + tpCfg.TP2Pct/100)
	tp3 = price * (1 + tpCfg.TP3Pct/100)

	return
}

func calcShortLevels(price float64, m *models.TimeframeMetrics, tpCfg TPConfig) (entryLow, entryHigh, tp1, tp2, tp3 float64) {
	// Aggressive limit: enter at current price (BestBid)
	entryLow = price
	entryHigh = price

	// Percentage-based TP levels
	tp1 = price * (1 - tpCfg.TP1Pct/100)
	tp2 = price * (1 - tpCfg.TP2Pct/100)
	tp3 = price * (1 - tpCfg.TP3Pct/100)

	return
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
