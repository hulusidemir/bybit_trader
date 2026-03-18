package kraken

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"bybit_trader/pkg/models"
)

// ════════════════════════════════════════════════════════════
// Kraken Native WebSocket DataProvider
//
// WebSocket V2:
//   wss://ws.kraken.com/v2
//
// Channels (spot only — Kraken Futures is separate):
//   - book (depth 25) → OB_UPDATE (spot)
//   - trade           → CVD_UPDATE (spot)
//
// Auto-reconnect + ping/pong built in.
// ════════════════════════════════════════════════════════════

const (
	krakenWSv2 = "wss://ws.kraken.com/v2"

	krakenPingInterval   = 15 * time.Second
	krakenReconnectDelay = 2 * time.Second
	krakenMaxReconnect   = 5 * time.Second
)

// WSProvider implements exchange.DataProvider with native WebSocket.
type WSProvider struct {
	mu      sync.Mutex
	eventCh chan models.MarketEvent
	symbols []string // Bybit-format symbols

	// wsname → bybit symbol reverse map
	pairToBybit map[string]string

	connCancel context.CancelFunc
}

// NewWSProvider creates a production Kraken WebSocket data provider.
func NewWSProvider() *WSProvider {
	return &WSProvider{
		eventCh:     make(chan models.MarketEvent, 4096),
		pairToBybit: make(map[string]string),
	}
}

func (p *WSProvider) Name() string { return "kraken" }

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

	// Build Kraken WS pair names and reverse mapping
	p.pairToBybit = make(map[string]string)
	var wsPairs []string
	for _, bSym := range symbols {
		pair := bybitToKrakenWS(bSym)
		if pair == "" {
			continue
		}
		wsPairs = append(wsPairs, pair)
		p.pairToBybit[pair] = bSym
	}

	if len(wsPairs) == 0 {
		log.Printf("[Kraken-WS] No valid pairs to subscribe")
		return nil
	}

	connCtx, cancel := context.WithCancel(ctx)
	p.connCancel = cancel

	go p.runConnection(connCtx, wsPairs)

	log.Printf("[Kraken-WS] Subscribing %d pairs", len(wsPairs))
	return nil
}

// bybitToKrakenWS converts Bybit symbol to Kraken WS v2 pair name.
// "BTCUSDT" → "BTC/USDT", "ETHUSDT" → "ETH/USDT"
func bybitToKrakenWS(bybitSymbol string) string {
	base := extractBase(bybitSymbol)
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	return base + "/USDT"
}

// ── Connection Lifecycle ────────────────────────────────────

func (p *WSProvider) runConnection(ctx context.Context, wsPairs []string) {
	delay := krakenReconnectDelay

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := p.connectAndStream(ctx, wsPairs)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[Kraken-WS] disconnected: %v — reconnecting in %s", err, delay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = delay * 3 / 2
		if delay > krakenMaxReconnect {
			delay = krakenMaxReconnect
		}
	}
}

func (p *WSProvider) connectAndStream(ctx context.Context, wsPairs []string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, krakenWSv2, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	conn.SetPongHandler(func(string) error { return nil })

	// Subscribe to book channel (Kraken v2 format)
	bookSub := map[string]any{
		"method": "subscribe",
		"params": map[string]any{
			"channel": "book",
			"depth":   25,
			"symbol":  wsPairs,
		},
	}
	if err := conn.WriteJSON(bookSub); err != nil {
		return fmt.Errorf("subscribe book: %w", err)
	}

	// Subscribe to trade channel
	tradeSub := map[string]any{
		"method": "subscribe",
		"params": map[string]any{
			"channel": "trade",
			"symbol":  wsPairs,
		},
	}
	if err := conn.WriteJSON(tradeSub); err != nil {
		return fmt.Errorf("subscribe trade: %w", err)
	}

	log.Printf("[Kraken-WS] connected, subscribed %d pairs (book + trade)", len(wsPairs))

	// Ping goroutine — Kraken v2 uses method "ping"
	go func() {
		ticker := time.NewTicker(krakenPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ping := map[string]any{"method": "ping"}
				if err := conn.WriteJSON(ping); err != nil {
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

// Kraken V2 push: {"channel":"book","type":"snapshot","data":[...]}
type krakenV2Msg struct {
	Channel string          `json:"channel"`
	Type    string          `json:"type"` // "snapshot", "update"
	Data    json.RawMessage `json:"data"`
	// For subscribe/pong responses
	Method string `json:"method"`
}

func (p *WSProvider) handleMessage(msg []byte) {
	var m krakenV2Msg
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}

	// Skip system messages (pong, subscribe confirmations, heartbeat)
	if m.Method != "" || m.Channel == "heartbeat" || m.Channel == "status" || m.Channel == "" {
		return
	}

	switch m.Channel {
	case "book":
		p.parseBook(m.Data)
	case "trade":
		p.parseTrade(m.Data)
	}
}

// ── Book Parser ─────────────────────────────────────────────

type krakenBookData struct {
	Symbol string     `json:"symbol"` // "BTC/USDT"
	Bids   []krakenOBLevel `json:"bids"`
	Asks   []krakenOBLevel `json:"asks"`
}

type krakenOBLevel struct {
	Price float64 `json:"price"`
	Qty   float64 `json:"qty"`
}

func (p *WSProvider) parseBook(data json.RawMessage) {
	var books []krakenBookData
	if err := json.Unmarshal(data, &books); err != nil {
		return
	}

	for _, b := range books {
		bids := make([]models.OrderbookLevel, 0, len(b.Bids))
		asks := make([]models.OrderbookLevel, 0, len(b.Asks))

		for _, bid := range b.Bids {
			if bid.Price > 0 {
				bids = append(bids, models.OrderbookLevel{Price: bid.Price, Amount: bid.Qty})
			}
		}
		for _, ask := range b.Asks {
			if ask.Price > 0 {
				asks = append(asks, models.OrderbookLevel{Price: ask.Price, Amount: ask.Qty})
			}
		}

		if len(bids) == 0 && len(asks) == 0 {
			continue
		}

		bybitSym := p.resolveBybitSymbol(b.Symbol)

		p.emit(models.MarketEvent{
			Exchange:  "kraken",
			Symbol:    bybitSym,
			EventType: models.EventOBUpdate,
			Payload: models.OBPayload{
				IsFutures: false, // Kraken spot only
				Bids:      bids,
				Asks:      asks,
			},
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

// ── Trade Parser → CVD Events ───────────────────────────────

type krakenTradeData struct {
	Symbol string `json:"symbol"` // "BTC/USDT"
	Side   string `json:"side"`   // "buy" or "sell"
	Price  float64 `json:"price"`
	Qty    float64 `json:"qty"`
	Timestamp string `json:"timestamp"` // RFC3339
}

func (p *WSProvider) parseTrade(data json.RawMessage) {
	var trades []krakenTradeData
	if err := json.Unmarshal(data, &trades); err != nil {
		return
	}

	// Group by symbol
	type volAccum struct {
		buy, sell float64
	}
	bySymbol := make(map[string]*volAccum)

	for _, t := range trades {
		notional := t.Price * t.Qty

		acc, ok := bySymbol[t.Symbol]
		if !ok {
			acc = &volAccum{}
			bySymbol[t.Symbol] = acc
		}

		if t.Side == "buy" {
			acc.buy += notional
		} else {
			acc.sell += notional
		}
	}

	now := time.Now().UnixMilli()
	for symbol, acc := range bySymbol {
		if acc.buy == 0 && acc.sell == 0 {
			continue
		}
		bybitSym := p.resolveBybitSymbol(symbol)

		p.emit(models.MarketEvent{
			Exchange:  "kraken",
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

// ── Symbol Conversion ───────────────────────────────────────

func (p *WSProvider) resolveBybitSymbol(krakenPair string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if bSym, ok := p.pairToBybit[krakenPair]; ok {
		return bSym
	}
	// Fallback: "BTC/USDT" → "BTCUSDT"
	return strings.ReplaceAll(krakenPair, "/", "")
}

// ── Emit Helper ─────────────────────────────────────────────

func (p *WSProvider) emit(evt models.MarketEvent) {
	select {
	case p.eventCh <- evt:
	default:
	}
}
