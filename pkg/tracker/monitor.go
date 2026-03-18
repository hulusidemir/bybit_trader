package tracker

import (
	"fmt"
	"log"
	"time"

	"bybit_trader/pkg/executor"
	"bybit_trader/pkg/models"
	"bybit_trader/pkg/signals"
	"bybit_trader/pkg/telegram"
)

// ════════════════════════════════════════════════════════════
// Monitor — Event-Driven Execution Monitor
//
// Listens on ExecutionEventCh for real-time order/position
// updates from Bybit Private WebSocket. Zero REST polling.
//
// ORDER_UPDATE   → detect fills (entry, TP, DCA), cancels, rejects
// POSITION_UPDATE → detect position vanish (manual close/liquidation),
//                   track DCA via price updates
//
// A lightweight timeout goroutine runs in parallel to cancel
// stale unfilled limit entry orders.
// ════════════════════════════════════════════════════════════

type Monitor struct {
	store        *Store
	tgBot        *telegram.Bot
	exec         *executor.Executor
	execEventCh  <-chan models.ExecutionEvent
	stopCh       chan struct{}
	orderTimeout time.Duration
}

func NewMonitor(store *Store, tgBot *telegram.Bot, exec *executor.Executor, orderTimeoutSec int, execEventCh <-chan models.ExecutionEvent) *Monitor {
	return &Monitor{
		store:        store,
		tgBot:        tgBot,
		exec:         exec,
		execEventCh:  execEventCh,
		stopCh:       make(chan struct{}),
		orderTimeout: time.Duration(orderTimeoutSec) * time.Second,
	}
}

// Start launches the event-driven monitoring loop + timeout checker.
func (m *Monitor) Start() {
	go m.eventLoop()
	go m.timeoutLoop()
	log.Println("[Monitor] Event-driven monitoring started (Private WebSocket)")
}

func (m *Monitor) Stop() {
	close(m.stopCh)
}

// ── Event Loop (replaces 5s polling) ───────────────────────

func (m *Monitor) eventLoop() {
	for {
		select {
		case <-m.stopCh:
			return
		case evt, ok := <-m.execEventCh:
			if !ok {
				log.Println("[Monitor] ExecutionEventCh closed, stopping")
				return
			}
			switch evt.EventType {
			case models.ExecOrderUpdate:
				if p, ok := evt.Payload.(models.OrderUpdatePayload); ok {
					m.handleOrderUpdate(p)
				}
			case models.ExecPositionUpdate:
				if p, ok := evt.Payload.(models.PositionUpdatePayload); ok {
					m.handlePositionUpdate(p)
				}
			}
		}
	}
}

// ── Timeout Loop (lightweight, checks every 30s) ──────────

func (m *Monitor) timeoutLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkPendingTimeouts()
		}
	}
}

func (m *Monitor) checkPendingTimeouts() {
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

// ════════════════════════════════════════════════════════════
//  ORDER UPDATE HANDLER
// ════════════════════════════════════════════════════════════

func (m *Monitor) handleOrderUpdate(p models.OrderUpdatePayload) {
	if m.exec == nil {
		return
	}

	switch p.OrderStatus {
	case "Filled":
		m.handleOrderFilled(p)
	case "Cancelled", "Rejected", "Deactivated":
		m.handleOrderCancelled(p)
	}
	// "New", "PartiallyFilled" → no action needed, wait for fill
}

func (m *Monitor) handleOrderFilled(p models.OrderUpdatePayload) {
	// 1. Check if this is a pending entry order
	pending, _ := m.store.GetPendingTrades()
	for _, trade := range pending {
		if trade.OrderID == p.OrderID {
			m.activateFromFill(trade, p)
			return
		}
	}

	// 2. Check if this is a TP limit order fill for an active trade
	active, _ := m.store.GetActiveTrades()
	for _, trade := range active {
		switch {
		case trade.TP1OrderID != "" && trade.TP1OrderID == p.OrderID:
			log.Printf("[Monitor] 🎯 TP1 filled via WS: %s qty=%.6f — full close", p.Symbol, p.CumExecQty)
			m.closeTradeInDB(trade, trade.TP1, models.TradeTP1)
			return
		case trade.TP2OrderID != "" && trade.TP2OrderID == p.OrderID:
			log.Printf("[Monitor] 🎯 TP2 filled via WS: %s qty=%.6f", p.Symbol, p.CumExecQty)
			m.handleTP2Filled(trade, p)
			return
		case trade.TP3OrderID != "" && trade.TP3OrderID == p.OrderID:
			log.Printf("[Monitor] 🏆 TP3 filled via WS: %s qty=%.6f — full close", p.Symbol, p.CumExecQty)
			m.closeTradeInDB(trade, trade.TP3, models.TradeTP3)
			return
		}
	}
}

func (m *Monitor) activateFromFill(trade *models.Trade, p models.OrderUpdatePayload) {
	if err := m.store.ActivateTrade(trade.ID, p.AvgPrice, p.CumExecQty); err != nil {
		log.Printf("[Monitor] Error activating trade %d: %v", trade.ID, err)
		return
	}
	log.Printf("[Monitor] ✅ Order filled via WS: %s %s avg=%.4f qty=%.6f",
		trade.Symbol, trade.Direction, p.AvgPrice, p.CumExecQty)

	trade.AvgEntryPrice = p.AvgPrice
	trade.TotalQty = p.CumExecQty
	trade.RemainingQty = p.CumExecQty
	trade.Status = models.TradeActive

	msg := signals.FormatOrderFilled(trade, p.AvgPrice, p.CumExecQty)
	m.tgBot.SendMessage(msg)

	// Immediately place TP1 limit order
	m.placeNextTPOrder(trade, models.TPPhaseWaitingTP1)
}

func (m *Monitor) handleTP2Filled(trade *models.Trade, p models.OrderUpdatePayload) {
	newRemaining := trade.RemainingQty - p.CumExecQty
	if newRemaining < 0 {
		newRemaining = 0
	}
	m.store.UpdateRemainingQty(trade.ID, newRemaining)
	m.store.UpdateTradeStatus(trade.ID, models.TradeTP2)

	trade.RemainingQty = newRemaining
	trade.Status = models.TradeTP2
	trade.CurrentPrice = p.AvgPrice

	msg := signals.FormatTPHit(trade, "TP2_HIT", p.AvgPrice, p.CumExecQty, 0.50)
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
}

func (m *Monitor) handleOrderCancelled(p models.OrderUpdatePayload) {
	// Check if it's a pending entry order that was cancelled
	pending, _ := m.store.GetPendingTrades()
	for _, trade := range pending {
		if trade.OrderID == p.OrderID {
			log.Printf("[Monitor] Order %s for %s cancelled/rejected: %s", p.OrderID, p.Symbol, p.OrderStatus)
			m.store.CancelTrade(trade.ID)
			m.tgBot.SendMessage("❌ *EMİR İPTAL* — " + trade.Symbol + "\n" + p.OrderStatus)
			return
		}
	}

	// Check if it's a TP order that was cancelled externally — re-place it
	active, _ := m.store.GetActiveTrades()
	for _, trade := range active {
		switch {
		case trade.TP1OrderID != "" && trade.TP1OrderID == p.OrderID && trade.TPPhase == models.TPPhaseWaitingTP1:
			log.Printf("[Monitor] TP1 order cancelled externally for %s — re-placing", p.Symbol)
			trade.TP1OrderID = ""
			m.store.UpdateTPOrder(trade.ID, models.TPPhaseWaitingTP1, "", trade.TP2OrderID, trade.TP3OrderID)
			m.placeNextTPOrder(trade, models.TPPhaseWaitingTP1)
			return
		case trade.TP2OrderID != "" && trade.TP2OrderID == p.OrderID && trade.TPPhase == models.TPPhaseWaitingTP2:
			log.Printf("[Monitor] TP2 order cancelled externally for %s — re-placing", p.Symbol)
			trade.TP2OrderID = ""
			m.store.UpdateTPOrder(trade.ID, models.TPPhaseWaitingTP2, trade.TP1OrderID, "", trade.TP3OrderID)
			m.placeNextTPOrder(trade, models.TPPhaseWaitingTP2)
			return
		case trade.TP3OrderID != "" && trade.TP3OrderID == p.OrderID && trade.TPPhase == models.TPPhaseWaitingTP3:
			log.Printf("[Monitor] TP3 order cancelled externally for %s — re-placing", p.Symbol)
			trade.TP3OrderID = ""
			m.store.UpdateTPOrder(trade.ID, models.TPPhaseWaitingTP3, trade.TP1OrderID, trade.TP2OrderID, "")
			m.placeNextTPOrder(trade, models.TPPhaseWaitingTP3)
			return
		}
	}
}

// ════════════════════════════════════════════════════════════
//  POSITION UPDATE HANDLER
// ════════════════════════════════════════════════════════════

func (m *Monitor) handlePositionUpdate(p models.PositionUpdatePayload) {
	if m.exec == nil {
		return
	}

	// Find active trade for this symbol
	trade, err := m.store.GetActiveTradeBySymbol(p.Symbol)
	if err != nil || trade == nil {
		return // no active trade for this symbol
	}

	// ── Hedge Mode Side Filter ──────────────────────────────
	// In hedge mode, Bybit sends separate position updates for Buy (Long)
	// and Sell (Short) sides. Opening a SHORT also fires a size=0 update
	// for the Buy side. Without this guard the bot sees size=0 on the
	// wrong side and kills the active trade ("Ghost Update" bug).
	expectedSide := "Buy"
	if trade.Direction == models.DirectionShort {
		expectedSide = "Sell"
	}
	if p.Side != "" && p.Side != expectedSide {
		return // wrong side for this trade — hedge mode ghost update, ignore
	}

	// Position vanished (size=0) → external close or liquidation
	// CRITICAL: Ignore size=0 for PENDING trades! Bybit sends PositionUpdate
	// with size=0 before a limit order fills. Treating this as "vanished"
	// would kill unfilled orders (Pending Order Suicide).
	if p.Size == 0 {
		if trade.Status == models.TradePending {
			return // order not filled yet — size=0 is expected, ignore
		}

		// ── Delta Update Trap Guard ─────────────────────────────
		// Bybit WS may send delta payloads that omit the "size" field
		// (e.g. after placing a TP limit order). Go decodes the missing
		// field as 0.0, which looks like a closed position. Verify via
		// REST API before taking any destructive action.
		restSize, restErr := m.exec.GetPositionSize(trade.Symbol, trade.Direction)
		if restErr != nil {
			log.Printf("[Monitor] ⚠️ REST position check failed for %s: %v — skipping close", trade.Symbol, restErr)
			return // network error — do NOT close, safer to wait
		}
		if restSize > 0 {
			log.Printf("[Monitor] 🛡️ Delta trap caught: %s WS size=0 but REST size=%.6f — ignoring", trade.Symbol, restSize)
			return // position still alive, WS sent a partial/delta update
		}

		log.Printf("[Monitor] ⚠️ Position vanished (REST confirmed): %s — external close/liquidation", p.Symbol)
		m.exec.CancelAllOrders(trade.Symbol)

		exitPrice := p.MarkPrice
		if exitPrice == 0 {
			exitPrice = trade.CurrentPrice
		}
		m.closeTradeInDB(trade, exitPrice, models.TradeStopped)
		m.tgBot.SendMessage(fmt.Sprintf("⚠️ *POZİSYON KAPANDI* — %s\n\nBorsada pozisyon bulunamadı (elle kapatma veya likidasyon).\nÇıkış Fiyatı: %.4f", p.Symbol, exitPrice))
		return
	}

	// Update mark price in DB
	if p.MarkPrice > 0 {
		m.store.UpdateCurrentPrice(trade.ID, p.MarkPrice)
	}

	// Check DCA conditions (only pre-TP1, use mark price)
	if trade.TPPhase == models.TPPhaseWaitingTP1 && p.MarkPrice > 0 && m.exec.ShouldDCA(trade, p.MarkPrice) {
		m.executeDCA(trade, p.MarkPrice)
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
		fraction = 1.0 // 100% of total position — full close at TP1
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
		log.Printf("[Monitor] 🚨 CRITICAL: %s limit order FAILED for %s after retries: %v", phase, trade.Symbol, err)
		m.tgBot.SendMessage(fmt.Sprintf("🚨 *KRİTİK EMİR HATASI* — %s\n\n%s limit emri 3 deneme sonrası başarısız!\nPozisyon KORUMASIZ — yeniden deneniyor...\n```%v```", trade.Symbol, phase, err))
		// Schedule a delayed retry to avoid naked position
		go m.retryTPOrder(trade, phase, tpPrice, fraction)
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

// retryTPOrder is a background safety net for naked positions.
// If placeNextTPOrder fails after the API-level retries, this function
// makes 3 additional attempts with longer backoff (5s, 15s, 30s).
func (m *Monitor) retryTPOrder(trade *models.Trade, phase models.TPPhase, tpPrice, fraction float64) {
	backoff := [3]time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
	for attempt, delay := range backoff {
		time.Sleep(delay)

		// Re-read trade from DB to check it's still active
		fresh, err := m.store.GetActiveTradeBySymbol(trade.Symbol)
		if err != nil || fresh == nil || fresh.Status != models.TradeActive {
			log.Printf("[Monitor] TP retry cancelled — trade %s no longer active", trade.Symbol)
			return
		}

		orderID, orderQty, err := m.exec.PlaceTPLimitOrder(fresh, tpPrice, fraction)
		if err != nil {
			log.Printf("[Monitor] 🚨 TP retry %d/3 failed for %s: %v", attempt+1, trade.Symbol, err)
			continue
		}

		// Success — update DB
		switch phase {
		case models.TPPhaseWaitingTP1:
			m.store.UpdateTPOrder(fresh.ID, phase, orderID, fresh.TP2OrderID, fresh.TP3OrderID)
		case models.TPPhaseWaitingTP2:
			m.store.UpdateTPOrder(fresh.ID, phase, fresh.TP1OrderID, orderID, fresh.TP3OrderID)
		case models.TPPhaseWaitingTP3:
			m.store.UpdateTPOrder(fresh.ID, phase, fresh.TP1OrderID, fresh.TP2OrderID, orderID)
		}

		log.Printf("[Monitor] ✅ TP retry succeeded for %s: %s price=%.4f qty=%.6f orderID=%s",
			trade.Symbol, phase, tpPrice, orderQty, orderID)
		m.tgBot.SendMessage(fmt.Sprintf("✅ *TP EMRİ KURTARILDI* — %s\n\n%s emri yeniden deneme ile başarılı.\nFiyat: %.4f | Qty: %.6f",
			trade.Symbol, phase, tpPrice, orderQty))
		return
	}

	// All retries exhausted — position truly naked
	log.Printf("[Monitor] 🚨🚨 ALL TP RETRIES EXHAUSTED for %s — POSITION NAKED", trade.Symbol)
	m.tgBot.SendMessage(fmt.Sprintf("🚨🚨 *ACIL UYARI* — %s\n\n%s emri TÜM denemeler sonrası başarısız!\n**POZİSYON KORUMASIZ** — Manuel müdahale gerekli!",
		trade.Symbol, phase))
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
	trade.LastDCATime = time.Now()
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
