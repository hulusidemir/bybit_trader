package okx

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
// OKX Native WebSocket DataProvider
//
// Public WebSocket V5:
//   wss://ws.okx.com:8443/ws/v5/public
//
// Channels:
//   Swap (futures):
//     - books50-l2-tbt (orderbook top-50 tick-by-tick) → OB_UPDATE
//     - trades          → CVD_UPDATE (perp)
//     - tickers         → TICK + OI_UPDATE
//   Spot:
//     - books50-l2-tbt → OB_UPDATE (spot)
//     - trades          → CVD_UPDATE (spot)
//
// Auto-reconnect + ping/pong built in.
// ════════════════════════════════════════════════════════════

const (
	okxPublicWS = "wss://ws.okx.com:8443/ws/v5/public"

	okxPingInterval   = 15 * time.Second
	okxReconnectDelay = 2 * time.Second
	okxMaxReconnect   = 5 * time.Second
)

// WSProvider implements exchange.DataProvider with native WebSocket.
type WSProvider struct {
	mu      sync.Mutex
	eventCh chan models.MarketEvent
	symbols []string // Bybit-format symbols

	connCancel context.CancelFunc
}

// NewWSProvider creates a production OKX WebSocket data provider.
func NewWSProvider() *WSProvider {
	return &WSProvider{
		eventCh: make(chan models.MarketEvent, 4096),
	}
}

func (p *WSProvider) Name() string { return "okx" }

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

	connCtx, cancel := context.WithCancel(ctx)
	p.connCancel = cancel

	// OKX uses a single public WS endpoint for both swap and spot.
	// We open two connections: one for swap topics, one for spot topics.
	swapArgs := p.buildSwapArgs(symbols)
	spotArgs := p.buildSpotArgs(symbols)

	if len(swapArgs) > 0 {
		go p.runConnection(connCtx, swapArgs, "swap")
	}
	if len(spotArgs) > 0 {
		go p.runConnection(connCtx, spotArgs, "spot")
	}

	log.Printf("[OKX-WS] Subscribing %d symbols (swap: %d ch, spot: %d ch)",
		len(symbols), len(swapArgs), len(spotArgs))
	return nil
}

type okxSubArg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

func (p *WSProvider) buildSwapArgs(bybitSymbols []string) []okxSubArg {
	var args []okxSubArg
	for _, bSym := range bybitSymbols {
		instID := bybitToSwapInstID(bSym)
		args = append(args,
			okxSubArg{Channel: "books50-l2-tbt", InstID: instID},
			okxSubArg{Channel: "trades", InstID: instID},
			okxSubArg{Channel: "tickers", InstID: instID},
		)
	}
	return args
}

func (p *WSProvider) buildSpotArgs(bybitSymbols []string) []okxSubArg {
	var args []okxSubArg
	for _, bSym := range bybitSymbols {
		instID := bybitToSpotInstID(bSym)
		args = append(args,
			okxSubArg{Channel: "books50-l2-tbt", InstID: instID},
			okxSubArg{Channel: "trades", InstID: instID},
		)
	}
	return args
}

// ── Connection Lifecycle ────────────────────────────────────

func (p *WSProvider) runConnection(ctx context.Context, args []okxSubArg, label string) {
	delay := okxReconnectDelay

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := p.connectAndStream(ctx, args, label)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[OKX-WS] %s: disconnected: %v — reconnecting in %s", label, err, delay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = delay * 3 / 2
		if delay > okxMaxReconnect {
			delay = okxMaxReconnect
		}
	}
}

func (p *WSProvider) connectAndStream(ctx context.Context, args []okxSubArg, label string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, okxPublicWS, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	conn.SetPongHandler(func(string) error { return nil })

	// Subscribe in batches (keep messages reasonable)
	batchSize := 30
	for i := 0; i < len(args); i += batchSize {
		end := i + batchSize
		if end > len(args) {
			end = len(args)
		}
		sub := map[string]any{
			"op":   "subscribe",
			"args": args[i:end],
		}
		if err := conn.WriteJSON(sub); err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
	}

	log.Printf("[OKX-WS] %s: connected, subscribed %d channels", label, len(args))

	isFutures := label == "swap"

	// Ping goroutine — OKX expects "ping" text frames
	go func() {
		ticker := time.NewTicker(okxPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
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

		// OKX pong is literal "pong" text
		if string(msg) == "pong" {
			continue
		}

		p.handleMessage(msg, isFutures)
	}
}

// ── Message Parsing ─────────────────────────────────────────

// OKX push format: {"arg":{"channel":"...","instId":"..."},"data":[...]}
type okxPushMsg struct {
	Arg struct {
		Channel string `json:"channel"`
		InstID  string `json:"instId"`
	} `json:"arg"`
	Data json.RawMessage `json:"data"`
	// For subscribe responses
	Event string `json:"event"`
}

func (p *WSProvider) handleMessage(msg []byte, isFutures bool) {
	var envelope okxPushMsg
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return
	}

	// Skip subscribe/error responses
	if envelope.Event != "" || envelope.Arg.Channel == "" {
		return
	}

	switch envelope.Arg.Channel {
	case "books50-l2-tbt", "books5-l2-tbt":
		p.parseOrderbook(envelope.Data, envelope.Arg.InstID, isFutures)
	case "trades":
		p.parseTrades(envelope.Data, envelope.Arg.InstID, isFutures)
	case "tickers":
		p.parseTicker(envelope.Data, envelope.Arg.InstID)
	}
}

// ── Orderbook Parser ────────────────────────────────────────

type okxBookData struct {
	Asks [][]string `json:"asks"` // [price, size, liquidOrders, numOrders]
	Bids [][]string `json:"bids"`
	Ts   string     `json:"ts"`
}

func (p *WSProvider) parseOrderbook(data json.RawMessage, instID string, isFutures bool) {
	var books []okxBookData
	if err := json.Unmarshal(data, &books); err != nil {
		return
	}
	if len(books) == 0 {
		return
	}

	b := books[0]
	bids := parseOkxOBLevels(b.Bids)
	asks := parseOkxOBLevels(b.Asks)
	if len(bids) == 0 && len(asks) == 0 {
		return
	}

	ts, _ := strconv.ParseInt(b.Ts, 10, 64)
	bybitSym := okxInstIDToBybit(instID)

	p.emit(models.MarketEvent{
		Exchange:  "okx",
		Symbol:    bybitSym,
		EventType: models.EventOBUpdate,
		Payload: models.OBPayload{
			IsFutures: isFutures,
			Bids:      bids,
			Asks:      asks,
		},
		Timestamp: ts,
	})
}

func parseOkxOBLevels(raw [][]string) []models.OrderbookLevel {
	levels := make([]models.OrderbookLevel, 0, len(raw))
	for _, r := range raw {
		if len(r) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(r[0], 64)
		size, _ := strconv.ParseFloat(r[1], 64)
		if price > 0 && size > 0 {
			levels = append(levels, models.OrderbookLevel{Price: price, Amount: size})
		}
	}
	return levels
}

// ── Trade Parser → CVD Events ───────────────────────────────

type okxTradeData struct {
	InstID string `json:"instId"`
	Px     string `json:"px"`
	Sz     string `json:"sz"`
	Side   string `json:"side"` // "buy" or "sell"
	Ts     string `json:"ts"`
}

func (p *WSProvider) parseTrades(data json.RawMessage, instID string, isFutures bool) {
	var trades []okxTradeData
	if err := json.Unmarshal(data, &trades); err != nil {
		return
	}

	var buyVol, sellVol float64
	var lastTs int64
	for _, t := range trades {
		price, _ := strconv.ParseFloat(t.Px, 64)
		sz, _ := strconv.ParseFloat(t.Sz, 64)
		notional := price * sz
		if t.Side == "buy" {
			buyVol += notional
		} else {
			sellVol += notional
		}
		lastTs, _ = strconv.ParseInt(t.Ts, 10, 64)
	}

	if buyVol == 0 && sellVol == 0 {
		return
	}

	bybitSym := okxInstIDToBybit(instID)

	p.emit(models.MarketEvent{
		Exchange:  "okx",
		Symbol:    bybitSym,
		EventType: models.EventCVDUpdate,
		Payload: models.CVDPayload{
			IsSpot:     !isFutures,
			BuyVolume:  buyVol,
			SellVolume: sellVol,
			NetDelta:   buyVol - sellVol,
		},
		Timestamp: lastTs,
	})
}

// ── Ticker Parser → TICK + OI Events ────────────────────────

type okxTickerData struct {
	InstID  string `json:"instId"`
	Last    string `json:"last"`
	Vol24h  string `json:"vol24h"`  // 24h volume in base currency
	VolCcy  string `json:"volCcy24h"` // 24h volume in quote currency (turnover)
	OI      string `json:"oi"`      // open interest (contracts)
	Ts      string `json:"ts"`
}

func (p *WSProvider) parseTicker(data json.RawMessage, instID string) {
	var tickers []okxTickerData
	if err := json.Unmarshal(data, &tickers); err != nil {
		return
	}
	if len(tickers) == 0 {
		return
	}

	t := tickers[0]
	lastPrice, _ := strconv.ParseFloat(t.Last, 64)
	vol, _ := strconv.ParseFloat(t.Vol24h, 64)
	turnover, _ := strconv.ParseFloat(t.VolCcy, 64)
	oi, _ := strconv.ParseFloat(t.OI, 64)
	ts, _ := strconv.ParseInt(t.Ts, 10, 64)

	if lastPrice == 0 {
		return
	}

	bybitSym := okxInstIDToBybit(instID)

	p.emit(models.MarketEvent{
		Exchange:  "okx",
		Symbol:    bybitSym,
		EventType: models.EventTick,
		Payload: models.TickPayload{
			LastPrice:    lastPrice,
			Volume24h:    vol,
			Turnover24h:  turnover,
			OpenInterest: oi,
		},
		Timestamp: ts,
	})

	if oi > 0 {
		p.emit(models.MarketEvent{
			Exchange:  "okx",
			Symbol:    bybitSym,
			EventType: models.EventOIUpdate,
			Payload: models.OIPayload{
				OpenInterest: oi,
			},
			Timestamp: ts,
		})
	}
}

// ── Symbol Conversion ───────────────────────────────────────

// okxInstIDToBybit converts OKX instId back to Bybit format.
// "BTC-USDT-SWAP" → "BTCUSDT", "BTC-USDT" → "BTCUSDT"
func okxInstIDToBybit(instID string) string {
	s := strings.ReplaceAll(instID, "-SWAP", "")
	s = strings.ReplaceAll(s, "-", "")
	return s
}

// ── Emit Helper ─────────────────────────────────────────────

func (p *WSProvider) emit(evt models.MarketEvent) {
	select {
	case p.eventCh <- evt:
	default:
	}
}
