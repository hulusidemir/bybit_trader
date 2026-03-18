package executor

import (
	"fmt"
	"log"
	"strconv"

	"bybit_trader/pkg/exchange/bybit"
	"bybit_trader/pkg/models"
)

// ════════════════════════════════════════════════════════════
// Executor — Handles trade execution on Bybit
// Opens positions, partial TP closes, DCA entries
// Pure exchange operations — no DB dependency
// ════════════════════════════════════════════════════════════

// TradeStore is the interface that executor needs from the store
type TradeStore interface {
	GetActiveTradeBySymbol(symbol string) (*models.Trade, error)
}

type Executor struct {
	trade            *bybit.TradeClient
	store            TradeStore
	leverage         int
	marginUSD        float64            // default margin per entry
	maxDCA           int                // max DCA count per position
	dcaPct           float64            // default DCA threshold percentage
	coinDCAOverrides map[string]float64 // per-coin DCA overrides
	tp1Pct           float64            // TP1 %
	tp2Pct           float64            // TP2 %
	tp3Pct           float64            // TP3 %
}

func New(
	tc *bybit.TradeClient,
	store TradeStore,
	leverage int,
	marginUSD float64,
	maxDCA int,
	dcaPct float64,
	coinDCAOverrides map[string]float64,
	tp1Pct, tp2Pct, tp3Pct float64,
) *Executor {
	return &Executor{
		trade:            tc,
		store:            store,
		leverage:         leverage,
		marginUSD:        marginUSD,
		maxDCA:           maxDCA,
		dcaPct:           dcaPct,
		coinDCAOverrides: coinDCAOverrides,
		tp1Pct:           tp1Pct,
		tp2Pct:           tp2Pct,
		tp3Pct:           tp3Pct,
	}
}

// ── Open Position ──────────────────────────────────────────

// OpenPosition places a limit order for a new signal.
// marginUSD overrides the default margin for this specific trade (per-coin override).
// Returns orderID, entryPrice, qty if successful.
func (e *Executor) OpenPosition(sig *models.Signal, marginUSD float64) (orderID string, entryPrice float64, qty float64, err error) {
	symbol := sig.Symbol

	if marginUSD <= 0 {
		marginUSD = e.marginUSD
	}

	// Check for existing active position
	existing, err := e.store.GetActiveTradeBySymbol(symbol)
	if err != nil {
		return "", 0, 0, fmt.Errorf("check existing: %w", err)
	}
	if existing != nil {
		return "", 0, 0, fmt.Errorf("active position exists for %s (trade #%d)", symbol, existing.ID)
	}

	// Set leverage
	if err := e.trade.SetLeverage(symbol, e.leverage); err != nil {
		return "", 0, 0, fmt.Errorf("set leverage: %w", err)
	}

	// Calculate notional and qty
	notional := marginUSD * float64(e.leverage)
	entryPrice = (sig.EntryLow + sig.EntryHigh) / 2

	rawQty := notional / entryPrice
	qtyStr, err := e.trade.FormatQty(symbol, rawQty)
	if err != nil {
		return "", 0, 0, fmt.Errorf("format qty: %w", err)
	}

	priceStr, err := e.trade.FormatPrice(symbol, entryPrice)
	if err != nil {
		return "", 0, 0, fmt.Errorf("format price: %w", err)
	}

	// Parse back for accurate storage
	qty, _ = strconv.ParseFloat(qtyStr, 64)
	entryPrice, _ = strconv.ParseFloat(priceStr, 64)

	// Determine side
	side := "Buy"
	if sig.Direction == models.DirectionShort {
		side = "Sell"
	}

	// Place limit order with GTC
	result, err := e.trade.PlaceOrder(bybit.PlaceOrderReq{
		Symbol:      symbol,
		Side:        side,
		OrderType:   "Limit",
		Qty:         qtyStr,
		Price:       priceStr,
		TimeInForce: "GTC",
	})
	if err != nil {
		return "", 0, 0, fmt.Errorf("place order: %w", err)
	}

	log.Printf("[Executor] ✅ Order placed: %s %s %s qty=%s price=%s orderID=%s",
		side, symbol, sig.Direction, qtyStr, priceStr, result.OrderID)

	return result.OrderID, entryPrice, qty, nil
}

// ── DCA (Dollar Cost Average) ──────────────────────────────

// ExecuteDCA places a market order to add to an existing position.
// Called when price moves DCA threshold % against avg entry.
func (e *Executor) ExecuteDCA(trade *models.Trade, currentPrice float64) (orderID string, dcaQty float64, err error) {
	if trade.DCACount >= e.maxDCA {
		return "", 0, fmt.Errorf("max DCA count (%d) reached for %s", e.maxDCA, trade.Symbol)
	}

	// Set leverage (ensure it's correct)
	if err := e.trade.SetLeverage(trade.Symbol, e.leverage); err != nil {
		return "", 0, fmt.Errorf("set leverage for DCA: %w", err)
	}

	// Calculate DCA qty: use per-entry margin stored in the trade record
	dcaMargin := trade.MarginPerEntry
	if dcaMargin <= 0 {
		dcaMargin = e.marginUSD
	}
	notional := dcaMargin * float64(e.leverage)
	rawQty := notional / currentPrice

	qtyStr, err := e.trade.FormatQty(trade.Symbol, rawQty)
	if err != nil {
		return "", 0, fmt.Errorf("format DCA qty: %w", err)
	}

	dcaQty, _ = strconv.ParseFloat(qtyStr, 64)

	// DCA uses MARKET order for immediate fill
	side := "Buy"
	if trade.Direction == models.DirectionShort {
		side = "Sell"
	}

	result, err := e.trade.PlaceOrder(bybit.PlaceOrderReq{
		Symbol:    trade.Symbol,
		Side:      side,
		OrderType: "Market",
		Qty:       qtyStr,
	})
	if err != nil {
		return "", 0, fmt.Errorf("place DCA order: %w", err)
	}

	log.Printf("[Executor] 📊 DCA #%d executed: %s %s qty=%s price=%.4f orderID=%s",
		trade.DCACount+1, side, trade.Symbol, qtyStr, currentPrice, result.OrderID)

	return result.OrderID, dcaQty, nil
}

// ── Partial TP Close ───────────────────────────────────────

// ClosePartial closes a portion of the position (reduce-only market order).
// fraction: 0.0 to 1.0 of remainingQty
func (e *Executor) ClosePartial(trade *models.Trade, fraction float64) (orderID string, closedQty float64, err error) {
	closeQty := trade.RemainingQty * fraction
	if closeQty <= 0 {
		return "", 0, fmt.Errorf("invalid close qty for %s", trade.Symbol)
	}

	qtyStr, err := e.trade.FormatQty(trade.Symbol, closeQty)
	if err != nil {
		return "", 0, fmt.Errorf("format close qty: %w", err)
	}

	closedQty, _ = strconv.ParseFloat(qtyStr, 64)

	// Opposite side for close
	side := "Sell"
	if trade.Direction == models.DirectionShort {
		side = "Buy"
	}

	result, err := e.trade.PlaceOrder(bybit.PlaceOrderReq{
		Symbol:     trade.Symbol,
		Side:       side,
		OrderType:  "Market",
		Qty:        qtyStr,
		ReduceOnly: true,
	})
	if err != nil {
		return "", 0, fmt.Errorf("close partial: %w", err)
	}

	log.Printf("[Executor] 🎯 Partial close: %s %s qty=%s (%.0f%%) orderID=%s",
		trade.Symbol, trade.Direction, qtyStr, fraction*100, result.OrderID)

	return result.OrderID, closedQty, nil
}

// ── TP Limit Orders ────────────────────────────────────────

// PlaceTPLimitOrder places a reduce-only limit order at a TP price level.
// fraction: portion of remainingQty to close (0.0 to 1.0)
func (e *Executor) PlaceTPLimitOrder(trade *models.Trade, tpPrice float64, fraction float64) (orderID string, orderQty float64, err error) {
	closeQty := trade.RemainingQty * fraction
	if closeQty <= 0 {
		return "", 0, fmt.Errorf("invalid TP close qty for %s", trade.Symbol)
	}

	qtyStr, err := e.trade.FormatQty(trade.Symbol, closeQty)
	if err != nil {
		return "", 0, fmt.Errorf("format TP qty: %w", err)
	}

	priceStr, err := e.trade.FormatPrice(trade.Symbol, tpPrice)
	if err != nil {
		return "", 0, fmt.Errorf("format TP price: %w", err)
	}

	orderQty, _ = strconv.ParseFloat(qtyStr, 64)

	// Opposite side for close
	side := "Sell"
	if trade.Direction == models.DirectionShort {
		side = "Buy"
	}

	result, err := e.trade.PlaceOrder(bybit.PlaceOrderReq{
		Symbol:      trade.Symbol,
		Side:        side,
		OrderType:   "Limit",
		Qty:         qtyStr,
		Price:       priceStr,
		TimeInForce: "GTC",
		ReduceOnly:  true,
	})
	if err != nil {
		return "", 0, fmt.Errorf("place TP limit order: %w", err)
	}

	log.Printf("[Executor] 📋 TP limit order placed: %s %s qty=%s price=%s orderID=%s",
		trade.Symbol, trade.Direction, qtyStr, priceStr, result.OrderID)

	return result.OrderID, orderQty, nil
}

// SetTradingStop sets position-level stop loss via Bybit API
func (e *Executor) SetTradingStop(symbol string, direction models.SignalDirection, stopLoss float64) error {
	posIdx := 1 // Long
	if direction == models.DirectionShort {
		posIdx = 2 // Short
	}
	return e.trade.SetTradingStop(symbol, posIdx, stopLoss)
}

// CancelAllOrders cancels all open orders for a symbol
func (e *Executor) CancelAllOrders(symbol string) error {
	return e.trade.CancelAllOrders(symbol)
}

// ── Full Close ─────────────────────────────────────────────

// ClosePosition fully closes the remaining position
func (e *Executor) ClosePosition(trade *models.Trade) (orderID string, err error) {
	if trade.RemainingQty <= 0 {
		return "", fmt.Errorf("no remaining qty to close for %s", trade.Symbol)
	}

	qtyStr, err := e.trade.FormatQty(trade.Symbol, trade.RemainingQty)
	if err != nil {
		return "", fmt.Errorf("format close qty: %w", err)
	}

	side := "Sell"
	if trade.Direction == models.DirectionShort {
		side = "Buy"
	}

	result, err := e.trade.PlaceOrder(bybit.PlaceOrderReq{
		Symbol:     trade.Symbol,
		Side:       side,
		OrderType:  "Market",
		Qty:        qtyStr,
		ReduceOnly: true,
	})
	if err != nil {
		return "", fmt.Errorf("close position: %w", err)
	}

	log.Printf("[Executor] 🏁 Full close: %s %s qty=%s orderID=%s",
		trade.Symbol, trade.Direction, qtyStr, result.OrderID)

	return result.OrderID, nil
}

// ── Order Status Check ─────────────────────────────────────

// CheckOrderFilled checks if an order has been filled.
// Returns: filled (bool), avgPrice, filledQty
func (e *Executor) CheckOrderFilled(symbol, orderID string) (filled bool, avgPrice float64, filledQty float64, err error) {
	order, err := e.trade.GetOrder(symbol, orderID)
	if err != nil {
		return false, 0, 0, err
	}

	switch order.OrderStatus {
	case "Filled":
		avg, _ := strconv.ParseFloat(order.AvgPrice, 64)
		qty, _ := strconv.ParseFloat(order.CumExecQty, 64)
		return true, avg, qty, nil
	case "Cancelled", "Rejected", "Deactivated":
		return false, 0, 0, fmt.Errorf("order %s status: %s", orderID, order.OrderStatus)
	default:
		// Still open: "New", "PartiallyFilled", etc.
		return false, 0, 0, nil
	}
}

// CancelOrder cancels a pending order
func (e *Executor) CancelOrder(symbol, orderID string) error {
	return e.trade.CancelOrder(symbol, orderID)
}

// GetPositionSize returns the current position size on the exchange.
// Returns 0 if no position exists (manually closed or liquidated).
func (e *Executor) GetPositionSize(symbol string) (float64, error) {
	pos, err := e.trade.GetPosition(symbol)
	if err != nil {
		return 0, err
	}
	size, _ := strconv.ParseFloat(pos.Size, 64)
	return size, nil
}

// ── Helpers ────────────────────────────────────────────────

// dcaPctForCoin returns the DCA threshold % for a symbol (override or default)
func (e *Executor) dcaPctForCoin(symbol string) float64 {
	if e.coinDCAOverrides != nil {
		if pct, ok := e.coinDCAOverrides[symbol]; ok && pct > 0 {
			return pct
		}
	}
	return e.dcaPct
}

// CalcDCAPrice calculates the price at which DCA should trigger for a symbol
func (e *Executor) CalcDCAPrice(avgEntry float64, direction models.SignalDirection, symbol string) float64 {
	pct := e.dcaPctForCoin(symbol)
	if direction == models.DirectionLong {
		return avgEntry * (1 - pct/100)
	}
	return avgEntry * (1 + pct/100)
}

// ShouldDCA checks if current price has crossed the DCA threshold
func (e *Executor) ShouldDCA(trade *models.Trade, currentPrice float64) bool {
	if trade.DCACount >= e.maxDCA {
		return false
	}

	dcaPrice := e.CalcDCAPrice(trade.AvgEntryPrice, trade.Direction, trade.Symbol)

	if trade.Direction == models.DirectionLong {
		return currentPrice <= dcaPrice
	}
	return currentPrice >= dcaPrice
}

// RecalcTPs recalculates TP levels based on new average entry using configured percentages
func (e *Executor) RecalcTPs(avgEntry float64, direction models.SignalDirection) (tp1, tp2, tp3 float64) {
	p1 := e.tp1Pct / 100
	p2 := e.tp2Pct / 100
	p3 := e.tp3Pct / 100
	if direction == models.DirectionLong {
		tp1 = avgEntry * (1 + p1)
		tp2 = avgEntry * (1 + p2)
		tp3 = avgEntry * (1 + p3)
	} else {
		tp1 = avgEntry * (1 - p1)
		tp2 = avgEntry * (1 - p2)
		tp3 = avgEntry * (1 - p3)
	}
	return
}

// GetMaxDCA returns the max DCA count
func (e *Executor) GetMaxDCA() int {
	return e.maxDCA
}

// GetMarginUSD returns the margin per trade
func (e *Executor) GetMarginUSD() float64 {
	return e.marginUSD
}

// GetLeverage returns the leverage
func (e *Executor) GetLeverage() int {
	return e.leverage
}

// GetDCAPct returns the DCA threshold percentage
func (e *Executor) GetDCAPct() float64 {
	return e.dcaPct
}
