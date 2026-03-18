package coinbase

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"bybit_trader/pkg/models"
)

// ════════════════════════════════════════════════════════════
// Coinbase (Advanced Trade) Native WebSocket DataProvider
//
// WebSocket Feed:
//   wss://advanced-trade-ws.coinbase.com
//
// Channels:
//   Spot only (Coinbase has no futures):
//     - level2  → OB_UPDATE (spot orderbook)
//     - market_trades → CVD_UPDATE (spot)
//
// Auto-reconnect + ping/pong built in.
// ════════════════════════════════════════════════════════════

const (
	coinbaseWS = "wss://advanced-trade-ws.coinbase.com"

	cbPingInterval   = 15 * time.Second
	cbReconnectDelay = 2 * time.Second
	cbMaxReconnect   = 5 * time.Second
)

// WSProvider implements exchange.DataProvider with native WebSocket.
type WSProvider struct {
	mu      sync.Mutex
	eventCh chan models.MarketEvent
	symbols []string // Bybit-format symbols

	// Track which product IDs map back to bybit symbols
	productToBybit map[string]string

	connCancel context.CancelFunc
}

// NewWSProvider creates a production Coinbase WebSocket data provider.
func NewWSProvider() *WSProvider {
	return &WSProvider{
		eventCh:        make(chan models.MarketEvent, 4096),
		productToBybit: make(map[string]string),
	}
}

func (p *WSProvider) Name() string { return "coinbase" }

func (p *WSProvider) StreamEvents(_ context.Context) <-chan models.MarketEvent {
	return p.eventCh
}

// Subscribe (re-)subscribes to the given symbols (Bybit format).
func (p *WSProvider) Subscribe(ctx context.Context, symbols []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.connCancel != nil {
		p.connCancel()
	}

	p.symbols = make([]string, len(symbols))
	copy(p.symbols, symbols)

	// Build product IDs and reverse mapping
	p.productToBybit = make(map[string]string)
	var productIDs []string
	for _, bSym := range symbols {
		pid := bybitToProductID(bSym)
		productIDs = append(productIDs, pid)
		p.productToBybit[pid] = bSym
		// Also try USD variant
		base := extractBase(bSym)
		if strings.HasPrefix(base, "10000") {
			base = base[5:]
		} else if strings.HasPrefix(base, "1000") {
			base = base[4:]
		}
		usdPid := base + "-USD"
		productIDs = append(productIDs, usdPid)
		p.productToBybit[usdPid] = bSym
	}

	connCtx, cancel := context.WithCancel(ctx)
	p.connCancel = cancel

	go p.runConnection(connCtx, productIDs)

	log.Printf("[Coinbase-WS] Subscribing %d symbols (%d product IDs)", len(symbols), len(productIDs))
	return nil
}

// ── Connection Lifecycle ────────────────────────────────────

func (p *WSProvider) runConnection(ctx context.Context, productIDs []string) {
	delay := cbReconnectDelay

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := p.connectAndStream(ctx, productIDs)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[Coinbase-WS] disconnected: %v — reconnecting in %s", err, delay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = delay * 3 / 2
		if delay > cbMaxReconnect {
			delay = cbMaxReconnect
		}
	}
}

func (p *WSProvider) connectAndStream(ctx context.Context, productIDs []string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, coinbaseWS, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	conn.SetPongHandler(func(string) error { return nil })

	// Coinbase Advanced Trade WS subscribe format
	sub := map[string]any{
		"type":        "subscribe",
		"product_ids": productIDs,
		"channel":     "level2",
	}
	if err := conn.WriteJSON(sub); err != nil {
		return fmt.Errorf("subscribe level2: %w", err)
	}

	sub2 := map[string]any{
		"type":        "subscribe",
		"product_ids": productIDs,
		"channel":     "market_trades",
	}
	if err := conn.WriteJSON(sub2); err != nil {
		return fmt.Errorf("subscribe market_trades: %w", err)
	}

	log.Printf("[Coinbase-WS] connected, subscribed %d products", len(productIDs))

	// Ping goroutine
	go func() {
		ticker := time.NewTicker(cbPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		p.handleMessage(msg)
	}
}

// ── Message Parsing ─────────────────────────────────────────

type cbMessage struct {
	Channel string          `json:"channel"`
	Events  json.RawMessage `json:"events"`
}

func (p *WSProvider) handleMessage(msg []byte) {
	var m cbMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}

	switch m.Channel {
	case "l2_data":
		p.parseL2(m.Events)
	case "market_trades":
		p.parseMarketTrades(m.Events)
	}
}

// ── L2 (Orderbook) Parser ───────────────────────────────────

type cbL2Event struct {
	Type    string `json:"type"` // "snapshot" or "update"
	Product string `json:"product_id"`
	Updates []struct {
		Side      string `json:"side"` // "bid" or "offer"
		PriceStr  string `json:"price_level"`
		QtyStr    string `json:"new_quantity"`
	} `json:"updates"`
}

func (p *WSProvider) parseL2(eventsRaw json.RawMessage) {
	var events []cbL2Event
	if err := json.Unmarshal(eventsRaw, &events); err != nil {
		return
	}

	for _, evt := range events {
		var bids, asks []models.OrderbookLevel
		for _, u := range evt.Updates {
			price, _ := strconv.ParseFloat(u.PriceStr, 64)
			qty, _ := strconv.ParseFloat(u.QtyStr, 64)
			if price <= 0 {
				continue
			}
			level := models.OrderbookLevel{Price: price, Amount: qty}
			if u.Side == "bid" {
				bids = append(bids, level)
			} else {
				asks = append(asks, level)
			}
		}

		if len(bids) == 0 && len(asks) == 0 {
			continue
		}

		bybitSym := p.resolveBybitSymbol(evt.Product)

		p.emit(models.MarketEvent{
			Exchange:  "coinbase",
			Symbol:    bybitSym,
			EventType: models.EventOBUpdate,
			Payload: models.OBPayload{
				IsFutures: false, // Coinbase = spot only
				Bids:      bids,
				Asks:      asks,
			},
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

// ── Market Trades Parser → CVD Events ───────────────────────

type cbTradeEvent struct {
	Type   string `json:"type"`
	Trades []struct {
		Product string `json:"product_id"`
		Side    string `json:"side"` // "BUY" or "SELL"
		Price   string `json:"price"`
		Size    string `json:"size"`
		Time    string `json:"time"` // RFC3339
	} `json:"trades"`
}

func (p *WSProvider) parseMarketTrades(eventsRaw json.RawMessage) {
	var events []cbTradeEvent
	if err := json.Unmarshal(eventsRaw, &events); err != nil {
		return
	}

	for _, evt := range events {
		// Group by product
		type volAccum struct {
			buy, sell float64
		}
		byProduct := make(map[string]*volAccum)

		for _, t := range evt.Trades {
			price, _ := strconv.ParseFloat(t.Price, 64)
			size, _ := strconv.ParseFloat(t.Size, 64)
			notional := price * size

			acc, ok := byProduct[t.Product]
			if !ok {
				acc = &volAccum{}
				byProduct[t.Product] = acc
			}

			if strings.EqualFold(t.Side, "BUY") {
				acc.buy += notional
			} else {
				acc.sell += notional
			}
		}

		now := time.Now().UnixMilli()
		for product, acc := range byProduct {
			if acc.buy == 0 && acc.sell == 0 {
				continue
			}
			bybitSym := p.resolveBybitSymbol(product)

			p.emit(models.MarketEvent{
				Exchange:  "coinbase",
				Symbol:    bybitSym,
				EventType: models.EventCVDUpdate,
				Payload: models.CVDPayload{
					IsSpot:     true,
					BuyVolume:  acc.buy,
					SellVolume: acc.sell,
					NetDelta:   acc.buy - acc.sell,
				},
				Timestamp: now,
			})
		}
	}
}

// ── Symbol Conversion ───────────────────────────────────────

func (p *WSProvider) resolveBybitSymbol(productID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if bSym, ok := p.productToBybit[productID]; ok {
		return bSym
	}
	// Fallback: "BTC-USDT" → "BTCUSDT"
	return strings.ReplaceAll(productID, "-", "")
}

// ── Emit Helper ─────────────────────────────────────────────

func (p *WSProvider) emit(evt models.MarketEvent) {
	select {
	case p.eventCh <- evt:
	default:
	}
}
