package tracker

import (
	"fmt"
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

			// Update trade object with filled data for TP placement
			trade.AvgEntryPrice = avgPrice
			trade.TotalQty = filledQty
			trade.RemainingQty = filledQty
			trade.Status = models.TradeActive

			msg := signals.FormatOrderFilled(trade, avgPrice, filledQty)
			m.tgBot.SendMessage(msg)

			// Immediately place TP1 limit order (50% of position)
			m.placeNextTPOrder(trade, models.TPPhaseWaitingTP1)
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
		m.tgBot.SendMessage(fmt.Sprintf("⚠️ *API HATASI* — Fiyat verisi alınamadı\n\n```%v```", err))
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
	if m.exec == nil {
		return
	}

	// Check TP limit order fill status
	m.checkTPOrders(trade, currentPrice)

	// Check DCA conditions (only if no TP has been hit yet — after TP1 we're in profit-protection mode)
	if trade.TPPhase == models.TPPhaseWaitingTP1 && m.exec.ShouldDCA(trade, currentPrice) {
		m.executeDCA(trade, currentPrice)
	}
}

// checkTPOrders monitors TP limit order fills and cascades to next TP + SL update
func (m *Monitor) checkTPOrders(trade *models.Trade, currentPrice float64) {
	switch trade.TPPhase {
	case models.TPPhaseNone:
		// No TP order placed yet — place TP1 (this can happen if bot restarted)
		m.placeNextTPOrder(trade, models.TPPhaseWaitingTP1)

	case models.TPPhaseWaitingTP1:
		if trade.TP1OrderID == "" {
			return
		}
		filled, _, filledQty, err := m.exec.CheckOrderFilled(trade.Symbol, trade.TP1OrderID)
		if err != nil {
			log.Printf("[Monitor] TP1 order error for %s: %v — will retry", trade.Symbol, err)
			// Order might have been cancelled externally; re-place it
			trade.TP1OrderID = ""
			m.store.UpdateTPOrder(trade.ID, models.TPPhaseWaitingTP1, "", trade.TP2OrderID, trade.TP3OrderID)
			m.placeNextTPOrder(trade, models.TPPhaseWaitingTP1)
			return
		}
		if !filled {
			return // still waiting
		}

		// TP1 filled!
		log.Printf("[Monitor] 🎯 TP1 limit order filled: %s qty=%.6f", trade.Symbol, filledQty)

		newRemaining := trade.RemainingQty - filledQty
		if newRemaining < 0 {
			newRemaining = 0
		}
		m.store.UpdateRemainingQty(trade.ID, newRemaining)
		m.store.UpdateTradeStatus(trade.ID, models.TradeTP1)

		trade.RemainingQty = newRemaining
		trade.Status = models.TradeTP1
		trade.CurrentPrice = currentPrice

		msg := signals.FormatTPHit(trade, "TP1_HIT", currentPrice, filledQty, 0.50)
		m.tgBot.SendMessage(msg)

		// Move stop loss to entry price (breakeven)
		if err := m.exec.SetTradingStop(trade.Symbol, trade.Direction, trade.AvgEntryPrice); err != nil {
			log.Printf("[Monitor] Error setting SL to breakeven for %s: %v", trade.Symbol, err)
			m.tgBot.SendMessage(fmt.Sprintf("⚠️ *SL HATASI* — %s\n\nStop loss giriş fiyatına çekilemedi\n```%v```", trade.Symbol, err))
		} else {
			m.store.UpdateStopLoss(trade.ID, trade.AvgEntryPrice)
			m.store.MarkStopMoved(trade.ID, "TP1", time.Now())
			stopMsg := signals.FormatStopMoved(trade, "TP1 → Giriş (Breakeven)", trade.AvgEntryPrice)
			m.tgBot.SendMessage(stopMsg)
		}

		// Place TP2 limit order (50% of remaining)
		m.placeNextTPOrder(trade, models.TPPhaseWaitingTP2)

	case models.TPPhaseWaitingTP2:
		if trade.TP2OrderID == "" {
			return
		}
		filled, _, filledQty, err := m.exec.CheckOrderFilled(trade.Symbol, trade.TP2OrderID)
		if err != nil {
			log.Printf("[Monitor] TP2 order error for %s: %v — will retry", trade.Symbol, err)
			trade.TP2OrderID = ""
			m.store.UpdateTPOrder(trade.ID, models.TPPhaseWaitingTP2, trade.TP1OrderID, "", trade.TP3OrderID)
			m.placeNextTPOrder(trade, models.TPPhaseWaitingTP2)
			return
		}
		if !filled {
			return
		}

		// TP2 filled!
		log.Printf("[Monitor] 🎯 TP2 limit order filled: %s qty=%.6f", trade.Symbol, filledQty)

		newRemaining := trade.RemainingQty - filledQty
		if newRemaining < 0 {
			newRemaining = 0
		}
		m.store.UpdateRemainingQty(trade.ID, newRemaining)
		m.store.UpdateTradeStatus(trade.ID, models.TradeTP2)

		trade.RemainingQty = newRemaining
		trade.Status = models.TradeTP2
		trade.CurrentPrice = currentPrice

		msg := signals.FormatTPHit(trade, "TP2_HIT", currentPrice, filledQty, 0.50)
		m.tgBot.SendMessage(msg)

		// Move stop loss to TP1
		if err := m.exec.SetTradingStop(trade.Symbol, trade.Direction, trade.TP1); err != nil {
			log.Printf("[Monitor] Error setting SL to TP1 for %s: %v", trade.Symbol, err)
			m.tgBot.SendMessage(fmt.Sprintf("⚠️ *SL HATASI* — %s\n\nStop loss TP1'e çekilemedi\n```%v```", trade.Symbol, err))
		} else {
			m.store.UpdateStopLoss(trade.ID, trade.TP1)
			m.store.MarkStopMoved(trade.ID, "TP2", time.Now())
			stopMsg := signals.FormatStopMoved(trade, "TP2 → TP1", trade.TP1)
			m.tgBot.SendMessage(stopMsg)
		}

		// Place TP3 limit order (100% of remaining)
		m.placeNextTPOrder(trade, models.TPPhaseWaitingTP3)

	case models.TPPhaseWaitingTP3:
		if trade.TP3OrderID == "" {
			return
		}
		filled, _, filledQty, err := m.exec.CheckOrderFilled(trade.Symbol, trade.TP3OrderID)
		if err != nil {
			log.Printf("[Monitor] TP3 order error for %s: %v — will retry", trade.Symbol, err)
			trade.TP3OrderID = ""
			m.store.UpdateTPOrder(trade.ID, models.TPPhaseWaitingTP3, trade.TP1OrderID, trade.TP2OrderID, "")
			m.placeNextTPOrder(trade, models.TPPhaseWaitingTP3)
			return
		}
		if !filled {
			return
		}

		// TP3 filled — full close!
		log.Printf("[Monitor] 🏆 TP3 limit order filled: %s qty=%.6f — position fully closed", trade.Symbol, filledQty)
		m.closeTradeInDB(trade, trade.TP3, models.TradeTP3)
	}
}

// placeNextTPOrder places the appropriate TP limit order based on the phase
func (m *Monitor) placeNextTPOrder(trade *models.Trade, phase models.TPPhase) {
	if m.exec == nil {
		return
	}

	var tpPrice float64
	var fraction float64

	switch phase {
	case models.TPPhaseWaitingTP1:
		tpPrice = trade.TP1
		fraction = 0.50 // 50% of total position
	case models.TPPhaseWaitingTP2:
		tpPrice = trade.TP2
		fraction = 0.50 // 50% of remaining
	case models.TPPhaseWaitingTP3:
		tpPrice = trade.TP3
		fraction = 1.0 // 100% of remaining
	default:
		return
	}

	orderID, orderQty, err := m.exec.PlaceTPLimitOrder(trade, tpPrice, fraction)
	if err != nil {
		log.Printf("[Monitor] Error placing %s limit order for %s: %v", phase, trade.Symbol, err)
		m.tgBot.SendMessage(fmt.Sprintf("⚠️ *EMİR HATASI* — %s\n\n%s limit emri girilemedi\n```%v```", trade.Symbol, phase, err))
		return
	}

	// Update DB with order ID
	switch phase {
	case models.TPPhaseWaitingTP1:
		trade.TP1OrderID = orderID
		m.store.UpdateTPOrder(trade.ID, phase, orderID, trade.TP2OrderID, trade.TP3OrderID)
	case models.TPPhaseWaitingTP2:
		trade.TP2OrderID = orderID
		m.store.UpdateTPOrder(trade.ID, phase, trade.TP1OrderID, orderID, trade.TP3OrderID)
	case models.TPPhaseWaitingTP3:
		trade.TP3OrderID = orderID
		m.store.UpdateTPOrder(trade.ID, phase, trade.TP1OrderID, trade.TP2OrderID, orderID)
	}

	trade.TPPhase = phase

	log.Printf("[Monitor] 📋 %s limit order placed: %s price=%.4f qty=%.6f orderID=%s",
		phase, trade.Symbol, tpPrice, orderQty, orderID)

	msg := signals.FormatTPOrderPlaced(trade, string(phase), tpPrice, orderQty)
	m.tgBot.SendMessage(msg)
}

// ── DCA Execution ──────────────────────────────────────────

func (m *Monitor) executeDCA(trade *models.Trade, currentPrice float64) {
	if m.exec == nil {
		return
	}

	// Cancel existing TP1 limit order before DCA (qty/price will change)
	if trade.TP1OrderID != "" {
		if err := m.exec.CancelOrder(trade.Symbol, trade.TP1OrderID); err != nil {
			log.Printf("[Monitor] Warning: could not cancel TP1 order before DCA for %s: %v", trade.Symbol, err)
			m.tgBot.SendMessage(fmt.Sprintf("⚠️ *EMİR İPTAL HATASI* — %s\n\nDCA öncesi TP1 emri iptal edilemedi\n```%v```", trade.Symbol, err))
		}
		trade.TP1OrderID = ""
	}

	_, dcaQty, err := m.exec.ExecuteDCA(trade, currentPrice)
	if err != nil {
		log.Printf("[Monitor] DCA error for %s: %v", trade.Symbol, err)
		m.tgBot.SendMessage(fmt.Sprintf("⚠️ *DCA HATASI* — %s\n\nDCA emri girilemedi\n```%v```", trade.Symbol, err))
		// Re-place the TP1 order since DCA failed
		m.placeNextTPOrder(trade, models.TPPhaseWaitingTP1)
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

	// Update trade object with new values
	trade.AvgEntryPrice = newAvgEntry
	trade.TotalQty = newTotalQty
	trade.RemainingQty = newRemainingQty
	trade.DCACount = newDCACount
	trade.MarginUsed = newMarginUsed
	trade.TP1 = tp1
	trade.TP2 = tp2
	trade.TP3 = tp3

	log.Printf("[Monitor] 📊 DCA #%d: %s %s — qty=%.6f at %.4f, new avg=%.4f, total margin=$%.0f",
		newDCACount, trade.Symbol, trade.Direction, dcaQty, currentPrice, newAvgEntry, newMarginUsed)

	msg := signals.FormatDCA(trade, newDCACount, currentPrice, dcaQty, newAvgEntry, newMarginUsed, tp1, tp2, tp3)
	if err := m.tgBot.SendMessage(msg); err != nil {
		log.Printf("[Monitor] Error sending DCA notification: %v", err)
	}

	// Re-place TP1 limit order with new TP levels and updated qty
	m.placeNextTPOrder(trade, models.TPPhaseWaitingTP1)
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
