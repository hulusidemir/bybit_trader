package tracker

import (
	"log"
	"time"

	"bybit_trader/pkg/exchange/bybit"
	"bybit_trader/pkg/executor"
	"bybit_trader/pkg/models"
	"bybit_trader/pkg/signals"
	"bybit_trader/pkg/telegram"
)

type Monitor struct {
	store    *Store
	bybit    *bybit.Client
	tgBot    *telegram.Bot
	exec     *executor.Executor
	interval time.Duration
	stopCh   chan struct{}
	orderTimeout time.Duration // cancel unfilled limit orders after this
}

func NewMonitor(store *Store, bybitClient *bybit.Client, tgBot *telegram.Bot, exec *executor.Executor, orderTimeoutSec int) *Monitor {
	return &Monitor{
		store:        store,
		bybit:        bybitClient,
		tgBot:        tgBot,
		exec:         exec,
		interval:     5 * time.Second,
		stopCh:       make(chan struct{}),
		orderTimeout: time.Duration(orderTimeoutSec) * time.Second,
	}
}

// Start begins the background monitoring loop
func (m *Monitor) Start() {
	go m.loop()
	log.Println("[Monitor] Price monitoring started (interval: 5s)")
}

func (m *Monitor) Stop() {
	close(m.stopCh)
}

func (m *Monitor) loop() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkPendingOrders()
			m.checkActiveTrades()
		}
	}
}

// ── Pending Order Monitoring ───────────────────────────────
// Checks if limit entry orders have been filled or should be cancelled

func (m *Monitor) checkPendingOrders() {
	if m.exec == nil {
		return
	}

	trades, err := m.store.GetPendingTrades()
	if err != nil {
		log.Printf("[Monitor] Error getting pending trades: %v", err)
		return
	}

	for _, trade := range trades {
		if trade.OrderID == "" {
			continue
		}

		filled, avgPrice, filledQty, err := m.exec.CheckOrderFilled(trade.Symbol, trade.OrderID)
		if err != nil {
			// Order cancelled/rejected — cancel trade
			log.Printf("[Monitor] Order %s for %s failed: %v", trade.OrderID, trade.Symbol, err)
			m.store.CancelTrade(trade.ID)
			m.tgBot.SendMessage("❌ *EMİR İPTAL* — " + trade.Symbol + "\nEmir doldurulamadı.")
			continue
		}

		if filled {
			// Order filled — activate trade
			if err := m.store.ActivateTrade(trade.ID, avgPrice, filledQty); err != nil {
				log.Printf("[Monitor] Error activating trade %d: %v", trade.ID, err)
				continue
			}
			log.Printf("[Monitor] ✅ Order filled: %s %s avg=%.4f qty=%.6f",
				trade.Symbol, trade.Direction, avgPrice, filledQty)

			msg := signals.FormatOrderFilled(trade, avgPrice, filledQty)
			m.tgBot.SendMessage(msg)
			continue
		}

		// Check timeout
		elapsed := time.Since(trade.OpenedAt)
		if elapsed > m.orderTimeout {
			log.Printf("[Monitor] Order timeout: %s (%.0fs)", trade.Symbol, elapsed.Seconds())
			if err := m.exec.CancelOrder(trade.Symbol, trade.OrderID); err != nil {
				log.Printf("[Monitor] Error cancelling order: %v", err)
			}
			m.store.CancelTrade(trade.ID)
			m.tgBot.SendMessage("⏰ *EMİR ZAMAN AŞIMI* — " + trade.Symbol + "\nLimit emir doldurulamadı, iptal edildi.")
		}
	}
}

// ── Active Trade Monitoring ────────────────────────────────
// Checks TP levels and DCA conditions for real positions

func (m *Monitor) checkActiveTrades() {
	trades, err := m.store.GetActiveTrades()
	if err != nil {
		log.Printf("[Monitor] Error getting active trades: %v", err)
		return
	}

	if len(trades) == 0 {
		return
	}

	// Fetch current prices
	tickers, err := m.bybit.FetchTickers()
	if err != nil {
		log.Printf("[Monitor] Error fetching tickers: %v", err)
		return
	}

	for _, trade := range trades {
		ticker, ok := tickers[trade.Symbol]
		if !ok {
			continue
		}

		currentPrice := ticker.LastPrice
		if currentPrice == 0 {
			continue
		}

		// Update current price in DB
		m.store.UpdateCurrentPrice(trade.ID, currentPrice)

		// Evaluate trade conditions
		m.evaluateTrade(trade, currentPrice)
	}
}

func (m *Monitor) evaluateTrade(trade *models.Trade, currentPrice float64) {
	if trade.Direction == models.DirectionLong {
		m.evaluateLong(trade, currentPrice)
	} else {
		m.evaluateShort(trade, currentPrice)
	}
}

func (m *Monitor) evaluateLong(trade *models.Trade, price float64) {
	// ── TP3: close remaining position ──────────────
	if price >= trade.TP3 && trade.Status != models.TradeTP3 {
		m.executeTPClose(trade, price, models.TradeTP3, 1.0) // 100% of remaining
		return
	}

	// ── TP2: close 50% of remaining ────────────────
	if price >= trade.TP2 && trade.Status != models.TradeTP2 && trade.Status != models.TradeTP3 {
		m.executeTPClose(trade, price, models.TradeTP2, 0.50)
		return
	}

	// ── TP1: close 40% of remaining ────────────────
	if price >= trade.TP1 && trade.Status != models.TradeTP1 && trade.Status != models.TradeTP2 && trade.Status != models.TradeTP3 {
		m.executeTPClose(trade, price, models.TradeTP1, 0.40)
		return
	}

	// ── DCA: check if price dropped 20% from avg entry ──
	if m.exec != nil && m.exec.ShouldDCA(trade, price) {
		m.executeDCA(trade, price)
	}
}

func (m *Monitor) evaluateShort(trade *models.Trade, price float64) {
	// ── TP3: close remaining position ──────────────
	if price <= trade.TP3 && trade.Status != models.TradeTP3 {
		m.executeTPClose(trade, price, models.TradeTP3, 1.0)
		return
	}

	// ── TP2: close 50% of remaining ────────────────
	if price <= trade.TP2 && trade.Status != models.TradeTP2 && trade.Status != models.TradeTP3 {
		m.executeTPClose(trade, price, models.TradeTP2, 0.50)
		return
	}

	// ── TP1: close 40% of remaining ────────────────
	if price <= trade.TP1 && trade.Status != models.TradeTP1 && trade.Status != models.TradeTP2 && trade.Status != models.TradeTP3 {
		m.executeTPClose(trade, price, models.TradeTP1, 0.40)
		return
	}

	// ── DCA: check if price rose 20% from avg entry ──
	if m.exec != nil && m.exec.ShouldDCA(trade, price) {
		m.executeDCA(trade, price)
	}
}

// ── TP Execution ───────────────────────────────────────────

func (m *Monitor) executeTPClose(trade *models.Trade, currentPrice float64, status models.TradeStatus, fraction float64) {
	if m.exec == nil {
		// Signal-only mode: just update status
		m.store.UpdateTradeStatus(trade.ID, status)
		if status == models.TradeTP3 {
			m.closeTradeInDB(trade, currentPrice, status)
		}
		return
	}

	if status == models.TradeTP3 {
		// Full close
		_, err := m.exec.ClosePosition(trade)
		if err != nil {
			log.Printf("[Monitor] Error closing position at TP3 for %s: %v", trade.Symbol, err)
			return
		}
		m.closeTradeInDB(trade, currentPrice, status)
	} else {
		// Partial close
		_, closedQty, err := m.exec.ClosePartial(trade, fraction)
		if err != nil {
			log.Printf("[Monitor] Error partial close at %s for %s: %v", status, trade.Symbol, err)
			return
		}

		newRemaining := trade.RemainingQty - closedQty
		if newRemaining < 0 {
			newRemaining = 0
		}

		m.store.UpdateRemainingQty(trade.ID, newRemaining)
		m.store.UpdateTradeStatus(trade.ID, status)

		trade.RemainingQty = newRemaining
		trade.Status = status
		trade.CurrentPrice = currentPrice

		log.Printf("[Monitor] %s %s trade #%d: %s hit — closed %.6f, remaining %.6f",
			trade.Symbol, trade.Direction, trade.ID, status, closedQty, newRemaining)

		msg := signals.FormatTPHit(trade, string(status), currentPrice, closedQty, fraction)
		if err := m.tgBot.SendMessage(msg); err != nil {
			log.Printf("[Monitor] Error sending TP notification: %v", err)
		}
	}
}

// ── DCA Execution ──────────────────────────────────────────

func (m *Monitor) executeDCA(trade *models.Trade, currentPrice float64) {
	if m.exec == nil {
		return
	}

	_, dcaQty, err := m.exec.ExecuteDCA(trade, currentPrice)
	if err != nil {
		log.Printf("[Monitor] DCA error for %s: %v", trade.Symbol, err)
		return
	}

	// Calculate new weighted average entry
	oldNotional := trade.AvgEntryPrice * trade.TotalQty
	newNotional := currentPrice * dcaQty
	newTotalQty := trade.TotalQty + dcaQty
	newAvgEntry := (oldNotional + newNotional) / newTotalQty
	newRemainingQty := trade.RemainingQty + dcaQty
	newDCACount := trade.DCACount + 1
	dcaMargin := trade.MarginPerEntry
	if dcaMargin <= 0 {
		dcaMargin = m.exec.GetMarginUSD()
	}
	newMarginUsed := trade.MarginUsed + dcaMargin

	// Recalculate TPs based on new average entry
	tp1, tp2, tp3 := m.exec.RecalcTPs(newAvgEntry, trade.Direction)

	// Update DB
	if err := m.store.UpdateDCA(trade.ID, newAvgEntry, newTotalQty, newRemainingQty,
		newDCACount, newMarginUsed, currentPrice, tp1, tp2, tp3); err != nil {
		log.Printf("[Monitor] Error updating DCA in DB for trade %d: %v", trade.ID, err)
		return
	}

	log.Printf("[Monitor] 📊 DCA #%d: %s %s — qty=%.6f at %.4f, new avg=%.4f, total margin=$%.0f",
		newDCACount, trade.Symbol, trade.Direction, dcaQty, currentPrice, newAvgEntry, newMarginUsed)

	msg := signals.FormatDCA(trade, newDCACount, currentPrice, dcaQty, newAvgEntry, newMarginUsed, tp1, tp2, tp3)
	if err := m.tgBot.SendMessage(msg); err != nil {
		log.Printf("[Monitor] Error sending DCA notification: %v", err)
	}
}

// ── Close Trade in DB ──────────────────────────────────────

func (m *Monitor) closeTradeInDB(trade *models.Trade, exitPrice float64, status models.TradeStatus) {
	var pnl float64
	if trade.Direction == models.DirectionLong {
		pnl = ((exitPrice - trade.AvgEntryPrice) / trade.AvgEntryPrice) * 100
	} else {
		pnl = ((trade.AvgEntryPrice - exitPrice) / trade.AvgEntryPrice) * 100
	}

	err := m.store.UpdateTrade(trade.ID, status, exitPrice, pnl)
	if err != nil {
		log.Printf("[Monitor] Error closing trade %d: %v", trade.ID, err)
		return
	}

	trade.ExitPrice = exitPrice
	trade.Status = status
	trade.PnLPercent = pnl

	log.Printf("[Monitor] Trade %s %s closed: %s PnL: %.2f%% (margin: $%.0f)",
		trade.Symbol, trade.Direction, status, pnl, trade.MarginUsed)

	msg := signals.FormatTradeClose(trade)
	if err := m.tgBot.SendMessage(msg); err != nil {
		log.Printf("[Monitor] Error sending close notification: %v", err)
	}
}
