package bitget

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
// Bitget Native WebSocket DataProvider
//
// WebSocket V2:
//   Futures (USDT-FUTURES): wss://ws.bitget.com/v2/ws/public
//   Spot:                   wss://ws.bitget.com/v2/ws/public
//
// Channels:
//   Futures:
//     - books15  → OB_UPDATE (futures orderbook top-15)
//     - trade    → CVD_UPDATE (perp)
//     - ticker   → TICK + OI_UPDATE
//   Spot:
//     - books15  → OB_UPDATE (spot)
//     - trade    → CVD_UPDATE (spot)
//
// Auto-reconnect + ping/pong built in.
// ════════════════════════════════════════════════════════════

const (
	bitgetWSPublic = "wss://ws.bitget.com/v2/ws/public"

	bitgetPingInterval   = 15 * time.Second
	bitgetReconnectDelay = 2 * time.Second
	bitgetMaxReconnect   = 5 * time.Second
)

// WSProvider implements exchange.DataProvider with native WebSocket.
type WSProvider struct {
	mu      sync.Mutex
	eventCh chan models.MarketEvent
	symbols []string // Bybit-format symbols

	connCancel context.CancelFunc
}

// NewWSProvider creates a production Bitget WebSocket data provider.
func NewWSProvider() *WSProvider {
	return &WSProvider{
		eventCh: make(chan models.MarketEvent, 4096),
	}
}

func (p *WSProvider) Name() string { return "bitget" }

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

	futuresArgs := p.buildFuturesArgs(symbols)
	spotArgs := p.buildSpotArgs(symbols)

	if len(futuresArgs) > 0 {
		go p.runConnection(connCtx, futuresArgs, "futures")
	}
	if len(spotArgs) > 0 {
		go p.runConnection(connCtx, spotArgs, "spot")
	}

	log.Printf("[Bitget-WS] Subscribing %d symbols (futures: %d ch, spot: %d ch)",
		len(symbols), len(futuresArgs), len(spotArgs))
	return nil
}

type bitgetSubArg struct {
	InstType string `json:"instType"`
	Channel  string `json:"channel"`
	InstID   string `json:"instId"`
}

func (p *WSProvider) buildFuturesArgs(bybitSymbols []string) []bitgetSubArg {
	var args []bitgetSubArg
	for _, bSym := range bybitSymbols {
		sym := bybitToFuturesSymbol(bSym)
		args = append(args,
			bitgetSubArg{InstType: "USDT-FUTURES", Channel: "books15", InstID: sym},
			bitgetSubArg{InstType: "USDT-FUTURES", Channel: "trade", InstID: sym},
			bitgetSubArg{InstType: "USDT-FUTURES", Channel: "ticker", InstID: sym},
		)
	}
	return args
}

func (p *WSProvider) buildSpotArgs(bybitSymbols []string) []bitgetSubArg {
	var args []bitgetSubArg
	for _, bSym := range bybitSymbols {
		sym := bybitToSpotSymbol(bSym)
		args = append(args,
			bitgetSubArg{InstType: "SPOT", Channel: "books15", InstID: sym},
			bitgetSubArg{InstType: "SPOT", Channel: "trade", InstID: sym},
		)
	}
	return args
}

// ── Connection Lifecycle ────────────────────────────────────

func (p *WSProvider) runConnection(ctx context.Context, args []bitgetSubArg, label string) {
	delay := bitgetReconnectDelay

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
			log.Printf("[Bitget-WS] %s: disconnected: %v — reconnecting in %s", label, err, delay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = delay * 3 / 2
		if delay > bitgetMaxReconnect {
			delay = bitgetMaxReconnect
		}
	}
}

func (p *WSProvider) connectAndStream(ctx context.Context, args []bitgetSubArg, label string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, bitgetWSPublic, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	conn.SetPongHandler(func(string) error { return nil })

	// Subscribe in batches
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

	log.Printf("[Bitget-WS] %s: connected, subscribed %d channels", label, len(args))

	isFutures := label == "futures"

	// Ping goroutine — Bitget expects "ping" text frames
	go func() {
		ticker := time.NewTicker(bitgetPingInterval)
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

		// Bitget pong is literal "pong"
		if string(msg) == "pong" {
			continue
		}

		p.handleMessage(msg, isFutures)
	}
}

// ── Message Parsing ─────────────────────────────────────────

// Bitget push: {"action":"snapshot|update","arg":{"instType":"...","channel":"...","instId":"..."},"data":[...]}
type bitgetPushMsg struct {
	Action string `json:"action"` // "snapshot" or "update"
	Arg    struct {
		InstType string `json:"instType"`
		Channel  string `json:"channel"`
		InstID   string `json:"instId"`
	} `json:"arg"`
	Data json.RawMessage `json:"data"`
	// For subscribe responses
	Event string `json:"event"`
}

func (p *WSProvider) handleMessage(msg []byte, isFutures bool) {
	var envelope bitgetPushMsg
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return
	}

	// Skip subscribe/error responses
	if envelope.Event != "" || envelope.Arg.Channel == "" {
		return
	}

	switch envelope.Arg.Channel {
	case "books15", "books5":
		p.parseOrderbook(envelope.Data, envelope.Arg.InstID, isFutures)
	case "trade":
		p.parseTrades(envelope.Data, envelope.Arg.InstID, isFutures)
	case "ticker":
		p.parseTicker(envelope.Data, envelope.Arg.InstID)
	}
}

// ── Orderbook Parser ────────────────────────────────────────

type bitgetBookData struct {
	Asks [][]string `json:"asks"` // [price, size]
	Bids [][]string `json:"bids"`
	Ts   string     `json:"ts"`
}

func (p *WSProvider) parseOrderbook(data json.RawMessage, instID string, isFutures bool) {
	var books []bitgetBookData
	if err := json.Unmarshal(data, &books); err != nil {
		return
	}
	if len(books) == 0 {
		return
	}

	b := books[0]
	bids := parseBitgetOBLevels(b.Bids)
	asks := parseBitgetOBLevels(b.Asks)
	if len(bids) == 0 && len(asks) == 0 {
		return
	}

	ts, _ := strconv.ParseInt(b.Ts, 10, 64)
	bybitSym := bitgetToBybitSymbol(instID, isFutures)

	p.emit(models.MarketEvent{
		Exchange:  "bitget",
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

func parseBitgetOBLevels(raw [][]string) []models.OrderbookLevel {
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

type bitgetTradeData struct {
	Ts   string `json:"ts"`
	Px   string `json:"price"`
	Sz   string `json:"size"`
	Side string `json:"side"` // "buy" or "sell"
}

func (p *WSProvider) parseTrades(data json.RawMessage, instID string, isFutures bool) {
	var trades []bitgetTradeData
	if err := json.Unmarshal(data, &trades); err != nil {
		return
	}

	var buyVol, sellVol float64
	var lastTs int64
	for _, t := range trades {
		price, _ := strconv.ParseFloat(t.Px, 64)
		sz, _ := strconv.ParseFloat(t.Sz, 64)
		notional := price * sz
		if strings.EqualFold(t.Side, "buy") {
			buyVol += notional
		} else {
			sellVol += notional
		}
		lastTs, _ = strconv.ParseInt(t.Ts, 10, 64)
	}

	if buyVol == 0 && sellVol == 0 {
		return
	}

	bybitSym := bitgetToBybitSymbol(instID, isFutures)

	p.emit(models.MarketEvent{
		Exchange:  "bitget",
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

type bitgetTickerData struct {
	InstID   string `json:"instId"`
	Last     string `json:"lastPr"`
	Open24h  string `json:"open24h"`
	High24h  string `json:"high24h"`
	Low24h   string `json:"low24h"`
	BaseVol  string `json:"baseVolume"`
	QuoteVol string `json:"quoteVolume"`
	OI       string `json:"openInterest"`
	FundRate string `json:"fundingRate"`
	Ts       string `json:"ts"`
}

func (p *WSProvider) parseTicker(data json.RawMessage, instID string) {
	var tickers []bitgetTickerData
	if err := json.Unmarshal(data, &tickers); err != nil {
		return
	}
	if len(tickers) == 0 {
		return
	}

	t := tickers[0]
	lastPrice, _ := strconv.ParseFloat(t.Last, 64)
	vol, _ := strconv.ParseFloat(t.BaseVol, 64)
	turnover, _ := strconv.ParseFloat(t.QuoteVol, 64)
	oi, _ := strconv.ParseFloat(t.OI, 64)
	funding, _ := strconv.ParseFloat(t.FundRate, 64)
	ts, _ := strconv.ParseInt(t.Ts, 10, 64)

	if lastPrice == 0 {
		return
	}

	bybitSym := bitgetToBybitSymbol(instID, true)

	p.emit(models.MarketEvent{
		Exchange:  "bitget",
		Symbol:    bybitSym,
		EventType: models.EventTick,
		Payload: models.TickPayload{
			LastPrice:    lastPrice,
			Volume24h:    vol,
			Turnover24h:  turnover,
			FundingRate:  funding,
			OpenInterest: oi,
		},
		Timestamp: ts,
	})

	if oi > 0 {
		p.emit(models.MarketEvent{
			Exchange:  "bitget",
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

// bitgetToBybitSymbol converts Bitget instId back to Bybit format.
// Futures: BTCUSDT → BTCUSDT (same naming)
// Spot: PEPEUSDT (Bitget spot) → 1000PEPEUSDT (Bybit), but we can't
// reliably reverse this without a lookup, so return as-is for spot.
func bitgetToBybitSymbol(instID string, isFutures bool) string {
	return instID
}

// ── Emit Helper ─────────────────────────────────────────────

func (p *WSProvider) emit(evt models.MarketEvent) {
	select {
	case p.eventCh <- evt:
	default:
	}
}
