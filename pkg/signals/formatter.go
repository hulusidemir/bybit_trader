package signals

import (
	"fmt"
	"math"
	"strings"
	"time"

	"bybit_trader/pkg/models"
)

// FormatTelegramMessage creates a beautiful Telegram message for a signal
func FormatTelegramMessage(sig *models.Signal) string {
	var b strings.Builder

	// Direction emoji and header
	dirEmoji := "🟢"
	dirText := "LONG"
	if sig.Direction == models.DirectionShort {
		dirEmoji = "🔴"
		dirText = "SHORT"
	}

	b.WriteString(fmt.Sprintf("🔥 %s %s SİNYAL — %s\n\n", dirEmoji, dirText, sig.Symbol))

	// Pattern
	b.WriteString(fmt.Sprintf("📊 *Pattern:* %s\n", sig.Pattern))

	// Timeframe confluence
	b.WriteString("⏰ *Timeframe:* ")
	for _, tf := range []string{"5", "15", "60", "240"} {
		label := tfLabel(tf)
		if _, ok := sig.Metrics[tf]; ok {
			// Check if the pattern matches on this TF
			patterns := classifyTFForSignal(sig.Metrics[tf], sig.Direction)
			if patterns {
				b.WriteString(fmt.Sprintf("%s ✅ | ", label))
			} else {
				b.WriteString(fmt.Sprintf("%s ⚠️ | ", label))
			}
		} else {
			b.WriteString(fmt.Sprintf("%s ❌ | ", label))
		}
	}
	b.WriteString("\n\n")

	// Metrics summary
	for _, tf := range []string{"5", "15", "60", "240"} {
		m, ok := sig.Metrics[tf]
		if !ok {
			continue
		}
		b.WriteString(fmt.Sprintf("━━ %s ━━\n", tfLabel(tf)))
		b.WriteString(fmt.Sprintf("📈 OI: %s (%+.1f%%)\n", trendIcon(m.OITrend), m.OIChange))
		b.WriteString(fmt.Sprintf("🔵 Spot CVD: %s (%s)\n", trendIcon(m.SpotCVDTrend), formatVolume(m.SpotCVD)))
		b.WriteString(fmt.Sprintf("🟣 Perp CVD: %s (%s)\n", trendIcon(m.PerpCVDTrend), formatVolume(m.PerpCVD)))
		b.WriteString(fmt.Sprintf("📕 OB: Bid/Ask %.2f", m.OBImbalance))
		if m.BidWallPrice > 0 {
			b.WriteString(fmt.Sprintf(" | Bid wall $%s", formatPrice(m.BidWallPrice)))
		}
		if m.AskWallPrice > 0 {
			b.WriteString(fmt.Sprintf(" | Ask wall $%s", formatPrice(m.AskWallPrice)))
		}
		b.WriteString("\n\n")
		break // Only show primary timeframe detail
	}

	// Explanation
	b.WriteString(fmt.Sprintf("💡 *Analiz:* %s\n\n", sig.Explanation))

	// Entry, TP, DCA
	b.WriteString(fmt.Sprintf("🎯 *Entry Zone:* $%s — $%s\n", formatPrice(sig.EntryLow), formatPrice(sig.EntryHigh)))
	b.WriteString(fmt.Sprintf("✅ *TP1:* $%s (+1%%)\n", formatPrice(sig.TP1)))
	b.WriteString(fmt.Sprintf("✅ *TP2:* $%s (+2.5%%)\n", formatPrice(sig.TP2)))
	b.WriteString(fmt.Sprintf("✅ *TP3:* $%s (+5%%)\n", formatPrice(sig.TP3)))
	b.WriteString(fmt.Sprintf("📊 *DCA Seviyesi:* $%s (-20%%)\n\n", formatPrice(sig.DCALevel)))

	// Grade and extras
	b.WriteString(fmt.Sprintf("⚡ *Sinyal Gücü:* %s (%d/100)\n", sig.Grade, sig.Confidence))
	if sig.LSRatio > 0 {
		lsDesc := "Dengeli"
		if sig.LSRatio < 0.8 {
			lsDesc = "Short ağırlıklı — squeeze potansiyeli"
		} else if sig.LSRatio > 1.2 {
			lsDesc = "Long ağırlıklı — dump riski"
		}
		b.WriteString(fmt.Sprintf("📊 *L/S Ratio:* %.2f (%s)\n", sig.LSRatio, lsDesc))
	}
	if sig.FundingRate != 0 {
		b.WriteString(fmt.Sprintf("💸 *Funding Rate:* %.4f%%\n", sig.FundingRate*100))

		// Funding countdown
		if sig.NextFundingTime > 0 {
			nextFund := time.UnixMilli(sig.NextFundingTime)
			untilFund := time.Until(nextFund)
			if untilFund > 0 {
				hours := int(untilFund.Hours())
				mins := int(untilFund.Minutes()) % 60
				b.WriteString(fmt.Sprintf("⏳ *Sonraki Funding:* %dsa %ddk sonra", hours, mins))
				if sig.FundingInterval > 0 {
					b.WriteString(fmt.Sprintf(" (her %dsa)\n", sig.FundingInterval))
				} else {
					b.WriteString("\n")
				}
			}
		}
	}
	b.WriteString(fmt.Sprintf("💰 *24h Hacim:* %s\n", formatVolume(sig.Volume24h)))

	return b.String()
}

// FormatTradeClose creates a close notification
func FormatTradeClose(trade *models.Trade) string {
	var b strings.Builder

	statusEmoji := "🛑"
	statusText := "STOPPED"
	switch trade.Status {
	case models.TradeTP1:
		statusEmoji = "✅"
		statusText = "TP1 HIT"
	case models.TradeTP2:
		statusEmoji = "🎯"
		statusText = "TP2 HIT"
	case models.TradeTP3:
		statusEmoji = "🏆"
		statusText = "TP3 HIT"
	}

	pnlEmoji := "📉"
	if trade.PnLPercent > 0 {
		pnlEmoji = "📈"
	}

	b.WriteString(fmt.Sprintf("%s *İŞLEM KAPANDI* — %s\n\n", statusEmoji, trade.Symbol))
	b.WriteString(fmt.Sprintf("📊 Pattern: %s\n", trade.Pattern))
	b.WriteString(fmt.Sprintf("🔄 Yön: %s\n", trade.Direction))
	b.WriteString(fmt.Sprintf("🎯 Giriş: $%s\n", formatPrice(trade.EntryPrice)))
	b.WriteString(fmt.Sprintf("🏁 Çıkış: $%s (%s)\n", formatPrice(trade.ExitPrice), statusText))
	b.WriteString(fmt.Sprintf("%s *PnL: %+.2f%%*\n", pnlEmoji, trade.PnLPercent))

	return b.String()
}

// FormatStopMoved creates a stop-loss moved notification
func FormatStopMoved(trade *models.Trade, level string, stopLoss float64) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("🛡️ *STOP GÜNCELLENDİ* — %s\n\n", trade.Symbol))
	b.WriteString(fmt.Sprintf("🔄 Yön: %s\n", trade.Direction))
	b.WriteString(fmt.Sprintf("📊 Pattern: %s\n", trade.Pattern))
	b.WriteString(fmt.Sprintf("📍 Yeni Stop: $%s\n", formatPrice(stopLoss)))
	b.WriteString(fmt.Sprintf("🎯 Tetikleyen Seviye: %s\n", level))
	b.WriteString(fmt.Sprintf("💵 Anlık Fiyat: $%s\n", formatPrice(trade.CurrentPrice)))

	return b.String()
}

func tfLabel(tf string) string {
	switch tf {
	case "5":
		return "5m"
	case "15":
		return "15m"
	case "60":
		return "1h"
	case "240":
		return "4h"
	}
	return tf
}

func trendIcon(t models.Trend) string {
	switch t {
	case models.TrendStrongUp:
		return "⬆️⬆️"
	case models.TrendUp:
		return "⬆️"
	case models.TrendNeutral:
		return "➡️"
	case models.TrendDown:
		return "⬇️"
	case models.TrendStrongDown:
		return "⬇️⬇️"
	}
	return "❓"
}

func formatPrice(p float64) string {
	if p >= 1000 {
		return fmt.Sprintf("%.2f", p)
	}
	if p >= 1 {
		return fmt.Sprintf("%.4f", p)
	}
	if p >= 0.01 {
		return fmt.Sprintf("%.6f", p)
	}
	return fmt.Sprintf("%.8f", p)
}

func formatVolume(v float64) string {
	abs := math.Abs(v)
	sign := ""
	if v < 0 {
		sign = "-"
	} else {
		sign = "+"
	}
	if abs >= 1_000_000_000 {
		return fmt.Sprintf("%s$%.1fB", sign, abs/1_000_000_000)
	}
	if abs >= 1_000_000 {
		return fmt.Sprintf("%s$%.1fM", sign, abs/1_000_000)
	}
	if abs >= 1_000 {
		return fmt.Sprintf("%s$%.1fK", sign, abs/1_000)
	}
	return fmt.Sprintf("%s$%.0f", sign, abs)
}

func classifyTFForSignal(m *models.TimeframeMetrics, dir models.SignalDirection) bool {
	if dir == models.DirectionShort {
		// Contrarian SHORT: bullish market data confirms setup (spot up or bid wall)
		return m.SpotCVDTrend >= models.TrendUp || (m.OITrend >= models.TrendUp && m.OBBias >= models.OBBidWall)
	}
	// Contrarian LONG: bearish market data confirms setup (spot down or ask wall)
	return m.SpotCVDTrend <= models.TrendDown || (m.OITrend >= models.TrendUp && m.OBBias <= models.OBAskWall)
}

// ── New Notification Formats (Live Trading) ────────────────

// FormatOrderFilled creates a notification when a limit entry order is filled
func FormatOrderFilled(trade *models.Trade, avgPrice, filledQty float64) string {
	var b strings.Builder

	dirEmoji := "🟢"
	if trade.Direction == models.DirectionShort {
		dirEmoji = "🔴"
	}

	b.WriteString(fmt.Sprintf("%s *EMİR DOLDU* — %s\n\n", dirEmoji, trade.Symbol))
	b.WriteString(fmt.Sprintf("📊 Pattern: %s\n", trade.Pattern))
	b.WriteString(fmt.Sprintf("🔄 Yön: %s\n", trade.Direction))
	b.WriteString(fmt.Sprintf("💰 Ortalama Fiyat: $%s\n", formatPrice(avgPrice)))
	b.WriteString(fmt.Sprintf("📦 Miktar: %.6f\n", filledQty))
	b.WriteString(fmt.Sprintf("🎯 TP1: $%s | TP2: $%s | TP3: $%s\n", formatPrice(trade.TP1), formatPrice(trade.TP2), formatPrice(trade.TP3)))

	return b.String()
}

// FormatTPHit creates a notification when a TP level is hit (partial close)
func FormatTPHit(trade *models.Trade, level string, currentPrice float64, closedQty, fraction float64) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("🎯 *%s KAPANDI* — %s\n\n", level, trade.Symbol))
	b.WriteString(fmt.Sprintf("📊 Pattern: %s\n", trade.Pattern))
	b.WriteString(fmt.Sprintf("🔄 Yön: %s\n", trade.Direction))
	b.WriteString(fmt.Sprintf("💵 Fiyat: $%s\n", formatPrice(currentPrice)))
	b.WriteString(fmt.Sprintf("📦 Kapatılan: %.6f (%%%.0f)\n", closedQty, fraction*100))
	b.WriteString(fmt.Sprintf("📦 Kalan: %.6f\n", trade.RemainingQty))

	return b.String()
}

// FormatDCA creates a notification when a DCA entry is executed
func FormatDCA(trade *models.Trade, dcaCount int, dcaPrice, dcaQty, newAvgEntry, totalMargin, tp1, tp2, tp3 float64) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("📊 *DCA #%d* — %s\n\n", dcaCount, trade.Symbol))
	b.WriteString(fmt.Sprintf("🔄 Yön: %s\n", trade.Direction))
	b.WriteString(fmt.Sprintf("💰 DCA Fiyat: $%s\n", formatPrice(dcaPrice)))
	b.WriteString(fmt.Sprintf("📦 Eklenen Miktar: %.6f\n", dcaQty))
	b.WriteString(fmt.Sprintf("📈 Yeni Ortalama: $%s\n", formatPrice(newAvgEntry)))
	b.WriteString(fmt.Sprintf("💵 Toplam Margin: $%.0f\n", totalMargin))
	b.WriteString(fmt.Sprintf("🎯 Yeni TP1: $%s | TP2: $%s | TP3: $%s\n", formatPrice(tp1), formatPrice(tp2), formatPrice(tp3)))

	return b.String()
}

// FormatTPOrderPlaced creates a notification when a TP limit order is placed
func FormatTPOrderPlaced(trade *models.Trade, phase string, tpPrice float64, orderQty float64) string {
	var b strings.Builder

	levelName := "TP1"
	switch phase {
	case "WAITING_TP1":
		levelName = "TP1"
	case "WAITING_TP2":
		levelName = "TP2"
	case "WAITING_TP3":
		levelName = "TP3"
	}

	b.WriteString(fmt.Sprintf("📋 *%s LİMİT EMRİ GİRİLDİ* — %s\n\n", levelName, trade.Symbol))
	b.WriteString(fmt.Sprintf("🔄 Yön: %s\n", trade.Direction))
	b.WriteString(fmt.Sprintf("💵 Hedef Fiyat: $%s\n", formatPrice(tpPrice)))
	b.WriteString(fmt.Sprintf("📦 Miktar: %.6f\n", orderQty))
	b.WriteString(fmt.Sprintf("📊 Kalan Pozisyon: %.6f\n", trade.RemainingQty))

	return b.String()
}
