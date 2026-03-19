package bybit

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
// Bybit Native WebSocket DataProvider
//
// Streams real-time data from Bybit V5 WebSocket API:
//   - Futures orderbook (orderbook.50)  → OB_UPDATE events
//   - Spot orderbook (orderbook.50)     → OB_UPDATE events
//   - Futures trades (publicTrade)      → CVD_UPDATE events (perp)
//   - Spot trades (publicTrade)         → CVD_UPDATE events (spot)
//   - Futures ticker (tickers)          → TICK + OI_UPDATE events
//
// Auto-reconnect + ping/pong built in.
// ════════════════════════════════════════════════════════════

const (
	bybitFuturesWS = "wss://stream.bybit.com/v5/public/linear"
	bybitSpotWS    = "wss://stream.bybit.com/v5/public/spot"

	bybitPingInterval   = 20 * time.Second
	bybitReconnectDelay = 2 * time.Second
	bybitMaxReconnect   = 5 * time.Second
)

// WSProvider implements exchange.DataProvider with native WebSocket.
type WSProvider struct {
	mu      sync.Mutex
	eventCh chan models.MarketEvent
	symbols []string

	// connCancel is used to tear down existing WS connections on re-subscribe
	connCancel context.CancelFunc
}

// NewWSProvider creates a production Bybit WebSocket data provider.
func NewWSProvider() *WSProvider {
	return &WSProvider{
		eventCh: make(chan models.MarketEvent, 4096),
	}
}

func (p *WSProvider) Name() string { return "bybit" }

// StreamEvents returns the shared event channel. Never closes.
func (p *WSProvider) StreamEvents(_ context.Context) <-chan models.MarketEvent {
	return p.eventCh
}

// Subscribe (re-)subscribes to the given symbols.
// Tears down old connections and creates new ones.
func (p *WSProvider) Subscribe(ctx context.Context, symbols []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Tear down previous connections
	if p.connCancel != nil {
		p.connCancel()
	}

	p.symbols = make([]string, len(symbols))
	copy(p.symbols, symbols)

	connCtx, cancel := context.WithCancel(ctx)
	p.connCancel = cancel

	// Futures WebSocket
	go p.runConnection(connCtx, bybitFuturesWS, p.buildFuturesTopics(symbols), true)

	// Spot WebSocket — subscribe to orderbook + trades for CVD
	go p.runConnection(connCtx, bybitSpotWS, p.buildSpotTopics(symbols), false)

	log.Printf("[Bybit-WS] Subscribing %d symbols (futures + spot)", len(symbols))
	return nil
}

// buildFuturesTopics creates subscription topics for futures linear perps.
func (p *WSProvider) buildFuturesTopics(symbols []string) []string {
	var topics []string
	for _, sym := range symbols {
		topics = append(topics,
			"orderbook.50."+sym,    // L2 orderbook 50 levels
			"publicTrade."+sym,     // real-time trades → CVD
			"tickers."+sym,         // ticker: price, OI, funding, volume
		)
	}
	return topics
}

// buildSpotTopics creates subscription topics for spot markets.
func (p *WSProvider) buildSpotTopics(symbols []string) []string {
	var topics []string
	for _, sym := range symbols {
		topics = append(topics,
			"orderbook.50."+sym, // spot orderbook depth
			"publicTrade."+sym,  // spot trades → spot CVD
		)
	}
	return topics
}

// ── Connection Lifecycle ────────────────────────────────────

// runConnection manages a single WebSocket connection with auto-reconnect.
func (p *WSProvider) runConnection(ctx context.Context, url string, topics []string, isFutures bool) {
	label := "futures"
	if !isFutures {
		label = "spot"
	}

	delay := bybitReconnectDelay

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Bybit-WS] %s: context cancelled, stopping", label)
			return
		default:
		}

		err := p.connectAndStream(ctx, url, topics, isFutures, label)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[Bybit-WS] %s: disconnected: %v — reconnecting in %s", label, err, delay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		// Exponential backoff capped at max
		delay = delay * 3 / 2
		if delay > bybitMaxReconnect {
			delay = bybitMaxReconnect
		}
	}
}

// connectAndStream dials WebSocket, subscribes, and reads messages until error/close.
func (p *WSProvider) connectAndStream(ctx context.Context, url string, topics []string, isFutures bool, label string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Setup ping handler
	conn.SetPongHandler(func(string) error {
		return nil
	})

	// Subscribe to topics in batches (Bybit max 10 args per subscribe)
	for i := 0; i < len(topics); i += 10 {
		end := i + 10
		if end > len(topics) {
			end = len(topics)
		}
		batch := topics[i:end]
		sub := map[string]any{
			"op":   "subscribe",
			"args": batch,
		}
		if err := conn.WriteJSON(sub); err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
	}
	log.Printf("[Bybit-WS] %s: connected, subscribed %d topics", label, len(topics))

	// Ping goroutine
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(bybitPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteJSON(map[string]string{"op": "ping"}); err != nil {
					return
				}
			}
		}
	}()

	// Read loop
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

		p.handleBybitMessage(msg, isFutures)
	}
}

// ── Message Parsing ─────────────────────────────────────────

// bybitWSMessage is the common envelope for all Bybit WS messages.
type bybitWSMessage struct {
	Topic string          `json:"topic"`
	Type  string          `json:"type"` // "snapshot" or "delta"
	Ts    int64           `json:"ts"`
	Data  json.RawMessage `json:"data"`
	Op    string          `json:"op"` // for pong/subscribe responses
}

func (p *WSProvider) handleBybitMessage(msg []byte, isFutures bool) {
	var envelope bybitWSMessage
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return
	}

	// Skip operational responses (pong, sub confirmations)
	if envelope.Op != "" || envelope.Topic == "" {
		return
	}

	topic := envelope.Topic
	ts := envelope.Ts

	switch {
	case strings.HasPrefix(topic, "orderbook."):
		p.parseOrderbook(envelope.Data, topic, ts, isFutures)
	case strings.HasPrefix(topic, "publicTrade."):
		p.parseTrades(envelope.Data, topic, ts, isFutures)
	case strings.HasPrefix(topic, "tickers."):
		p.parseTicker(envelope.Data, topic, ts)
	}
}

// ── Orderbook Parser ────────────────────────────────────────

type bybitOBData struct {
	S string     `json:"s"` // symbol
	B [][]string `json:"b"` // bids [price, size]
	A [][]string `json:"a"` // asks [price, size]
}

func (p *WSProvider) parseOrderbook(data json.RawMessage, topic string, ts int64, isFutures bool) {
	var ob bybitOBData
	if err := json.Unmarshal(data, &ob); err != nil {
		return
	}

	bids := parseOBLevels(ob.B)
	asks := parseOBLevels(ob.A)
	if len(bids) == 0 && len(asks) == 0 {
		return
	}

	p.emit(models.MarketEvent{
		Exchange:  "bybit",
		Symbol:    ob.S,
		EventType: models.EventOBUpdate,
		Payload: models.OBPayload{
			IsFutures: isFutures,
			Bids:      bids,
			Asks:      asks,
		},
		Timestamp: ts,
	})
}

func parseOBLevels(raw [][]string) []models.OrderbookLevel {
	levels := make([]models.OrderbookLevel, 0, len(raw))
	for _, r := range raw {
		if len(r) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(r[0], 64)
		size, _ := strconv.ParseFloat(r[1], 64)
		if price > 0 {
			levels = append(levels, models.OrderbookLevel{Price: price, Amount: size})
		}
	}
	return levels
}

// ── Trade Parser → CVD Events ───────────────────────────────

type bybitTradeItem struct {
	S  string `json:"S"` // side: "Buy" or "Sell"
	V  string `json:"v"` // volume (qty)
	P  string `json:"p"` // price
	T  int64  `json:"T"` // timestamp ms
	Symbol string `json:"s"` // symbol
}

func (p *WSProvider) parseTrades(data json.RawMessage, topic string, ts int64, isFutures bool) {
	var trades []bybitTradeItem
	if err := json.Unmarshal(data, &trades); err != nil {
		return
	}

	// Extract symbol from topic: "publicTrade.BTCUSDT"
	parts := strings.SplitN(topic, ".", 2)
	if len(parts) < 2 {
		return
	}
	sym := parts[1]

	var buyVol, sellVol float64
	for _, t := range trades {
		qty, _ := strconv.ParseFloat(t.V, 64)
		price, _ := strconv.ParseFloat(t.P, 64)
		notional := qty * price
		if t.S == "Buy" {
			buyVol += notional
		} else {
			sellVol += notional
		}
	}

	if buyVol == 0 && sellVol == 0 {
		return
	}

	p.emit(models.MarketEvent{
		Exchange:  "bybit",
		Symbol:    sym,
		EventType: models.EventCVDUpdate,
		Payload: models.CVDPayload{
			IsSpot:     !isFutures,
			BuyVolume:  buyVol,
			SellVolume: sellVol,
			NetDelta:   buyVol - sellVol,
		},
		Timestamp: ts,
	})
}

// ── Ticker Parser → TICK + OI Events ────────────────────────

type bybitTickerData struct {
	Symbol       string `json:"symbol"`
	LastPrice    string `json:"lastPrice"`
	Volume24h    string `json:"volume24h"`
	Turnover24h  string `json:"turnover24h"`
	FundingRate  string `json:"fundingRate"`
	OpenInterest string `json:"openInterest"`
	NextFundingTime string `json:"nextFundingTime"`
}

func (p *WSProvider) parseTicker(data json.RawMessage, topic string, ts int64) {
	var t bybitTickerData
	if err := json.Unmarshal(data, &t); err != nil {
		return
	}

	lastPrice, _ := strconv.ParseFloat(t.LastPrice, 64)
	vol24h, _ := strconv.ParseFloat(t.Volume24h, 64)
	turnover, _ := strconv.ParseFloat(t.Turnover24h, 64)
	funding, _ := strconv.ParseFloat(t.FundingRate, 64)
	oi, _ := strconv.ParseFloat(t.OpenInterest, 64)

	// Emit TICK event (always, even if some fields are zero for delta updates)
	if lastPrice > 0 {
		p.emit(models.MarketEvent{
			Exchange:  "bybit",
			Symbol:    t.Symbol,
			EventType: models.EventTick,
			Payload: models.TickPayload{
				LastPrice:    lastPrice,
				Volume24h:    vol24h,
				Turnover24h:  turnover,
				FundingRate:  funding,
				OpenInterest: oi,
			},
			Timestamp: ts,
		})
	}

	// Also emit dedicated OI event if OI is present
	if oi > 0 {
		p.emit(models.MarketEvent{
			Exchange:  "bybit",
			Symbol:    t.Symbol,
			EventType: models.EventOIUpdate,
			Payload:   models.OIPayload{OpenInterest: oi},
			Timestamp: ts,
		})
	}
}

// ── Kline Parser ────────────────────────────────────────────

// ── Emit Helper ─────────────────────────────────────────────

// emit sends an event to the channel without blocking.
func (p *WSProvider) emit(evt models.MarketEvent) {
	select {
	case p.eventCh <- evt:
	default:
		// Channel full — drop (acceptable in HFT hot path)
	}
}
