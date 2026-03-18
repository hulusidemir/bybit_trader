package bybit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"bybit_trader/pkg/models"
)

// ════════════════════════════════════════════════════════════
// Bybit V5 Private WebSocket Client
//
// Authenticated streaming of order and position updates.
// Replaces REST polling in the execution monitoring loop.
//
// Topics:
//   - order    → ORDER_UPDATE events (fills, cancels, rejects)
//   - position → POSITION_UPDATE events (size, PnL, liq price)
//
// Auth: HMAC-SHA256 of "GET/realtime{expires}" signed with API secret.
// Auto-reconnect + ping/pong built in.
// ════════════════════════════════════════════════════════════

const (
	privateWSMainnet = "wss://stream.bybit.com/v5/private"
	privateWSTestnet = "wss://stream-testnet.bybit.com/v5/private"

	privatePingInterval   = 20 * time.Second
	privateReconnectDelay = 2 * time.Second
	privateMaxReconnect   = 5 * time.Second
	privateAuthExpireMs   = 10000 // auth token validity: 10 seconds
)

// PrivateWSClient streams authenticated order/position updates from Bybit.
type PrivateWSClient struct {
	apiKey    string
	apiSecret string
	wsURL     string
	eventCh   chan models.ExecutionEvent
}

// NewPrivateWSClient creates a new authenticated WebSocket client.
func NewPrivateWSClient(apiKey, apiSecret string, testnet bool) *PrivateWSClient {
	url := privateWSMainnet
	if testnet {
		url = privateWSTestnet
	}
	return &PrivateWSClient{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		wsURL:     url,
		eventCh:   make(chan models.ExecutionEvent, 256),
	}
}

// Start launches the connection loop and returns the event channel.
// Blocks forever (reconnects on failure). Call in a goroutine.
func (c *PrivateWSClient) Start(ctx context.Context) <-chan models.ExecutionEvent {
	go c.runConnection(ctx)
	return c.eventCh
}

// ── Connection Lifecycle ────────────────────────────────────

func (c *PrivateWSClient) runConnection(ctx context.Context) {
	delay := privateReconnectDelay

	for {
		select {
		case <-ctx.Done():
			log.Println("[PrivateWS] Context cancelled, stopping")
			return
		default:
		}

		err := c.connectAndStream(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[PrivateWS] Disconnected: %v — reconnecting in %s", err, delay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = delay * 3 / 2
		if delay > privateMaxReconnect {
			delay = privateMaxReconnect
		}
	}
}

func (c *PrivateWSClient) connectAndStream(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	conn.SetPongHandler(func(string) error { return nil })

	// 1. Authenticate
	if err := c.authenticate(conn); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// 2. Wait for auth success response
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, authResp, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("auth response: %w", err)
	}

	var authResult struct {
		Success bool   `json:"success"`
		RetMsg  string `json:"ret_msg"`
		Op      string `json:"op"`
	}
	if err := json.Unmarshal(authResp, &authResult); err != nil {
		return fmt.Errorf("parse auth response: %w", err)
	}
	if !authResult.Success {
		return fmt.Errorf("auth failed: %s", authResult.RetMsg)
	}
	log.Println("[PrivateWS] Authenticated successfully")

	// 3. Subscribe to order + position topics
	if err := c.subscribe(conn); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// 4. Ping goroutine
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(privatePingInterval)
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

	// 5. Read loop
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

		c.handleMessage(msg)
	}
}

// ── Authentication ──────────────────────────────────────────

func (c *PrivateWSClient) authenticate(conn *websocket.Conn) error {
	expires := time.Now().UnixMilli() + privateAuthExpireMs
	expiresStr := strconv.FormatInt(expires, 10)

	// Bybit V5 Private WS auth: HMAC-SHA256("GET/realtime" + expires)
	h := hmac.New(sha256.New, []byte(c.apiSecret))
	h.Write([]byte("GET/realtime" + expiresStr))
	signature := hex.EncodeToString(h.Sum(nil))

	auth := map[string]any{
		"op":   "auth",
		"args": []any{c.apiKey, expires, signature},
	}
	return conn.WriteJSON(auth)
}

// ── Subscription ────────────────────────────────────────────

func (c *PrivateWSClient) subscribe(conn *websocket.Conn) error {
	sub := map[string]any{
		"op":   "subscribe",
		"args": []string{"order", "position"},
	}
	return conn.WriteJSON(sub)
}

// ── Message Handling ────────────────────────────────────────

type privateWSMsg struct {
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"`
	Op    string          `json:"op"`
}

func (c *PrivateWSClient) handleMessage(msg []byte) {
	var envelope privateWSMsg
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return
	}

	// Skip control messages (pong, subscribe confirmations)
	if envelope.Topic == "" {
		return
	}

	switch envelope.Topic {
	case "order":
		c.parseOrderUpdates(envelope.Data)
	case "position":
		c.parsePositionUpdates(envelope.Data)
	}
}

// ── Order Parsing ───────────────────────────────────────────

type rawOrderUpdate struct {
	OrderID     string `json:"orderId"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	OrderType   string `json:"orderType"`
	OrderStatus string `json:"orderStatus"`
	AvgPrice    string `json:"avgPrice"`
	CumExecQty  string `json:"cumExecQty"`
	CumExecVal  string `json:"cumExecValue"`
	Qty         string `json:"qty"`
	ReduceOnly  bool   `json:"reduceOnly"`
	CreatedTime string `json:"createdTime"`
}

func (c *PrivateWSClient) parseOrderUpdates(data json.RawMessage) {
	var orders []rawOrderUpdate
	if err := json.Unmarshal(data, &orders); err != nil {
		log.Printf("[PrivateWS] Error parsing order data: %v", err)
		return
	}

	now := time.Now().UnixMilli()
	for _, o := range orders {
		avgPrice, _ := strconv.ParseFloat(o.AvgPrice, 64)
		cumQty, _ := strconv.ParseFloat(o.CumExecQty, 64)
		cumVal, _ := strconv.ParseFloat(o.CumExecVal, 64)
		qty, _ := strconv.ParseFloat(o.Qty, 64)
		created, _ := strconv.ParseInt(o.CreatedTime, 10, 64)

		evt := models.ExecutionEvent{
			EventType: models.ExecOrderUpdate,
			Timestamp: now,
			Payload: models.OrderUpdatePayload{
				OrderID:     o.OrderID,
				Symbol:      o.Symbol,
				Side:        o.Side,
				OrderType:   o.OrderType,
				OrderStatus: o.OrderStatus,
				AvgPrice:    avgPrice,
				CumExecQty:  cumQty,
				CumExecVal:  cumVal,
				Qty:         qty,
				ReduceOnly:  o.ReduceOnly,
				CreatedTime: created,
			},
		}

		// Non-blocking send
		select {
		case c.eventCh <- evt:
		default:
			log.Printf("[PrivateWS] Event channel full, dropping order update for %s", o.Symbol)
		}
	}
}

// ── Position Parsing ────────────────────────────────────────

type rawPositionUpdate struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Size          string `json:"size"`
	AvgPrice      string `json:"entryPrice"`
	PositionValue string `json:"positionValue"`
	UnrealisedPnl string `json:"unrealisedPnl"`
	MarkPrice     string `json:"markPrice"`
	LiqPrice      string `json:"liqPrice"`
	Leverage      string `json:"leverage"`
}

func (c *PrivateWSClient) parsePositionUpdates(data json.RawMessage) {
	var positions []rawPositionUpdate
	if err := json.Unmarshal(data, &positions); err != nil {
		log.Printf("[PrivateWS] Error parsing position data: %v", err)
		return
	}

	now := time.Now().UnixMilli()
	for _, p := range positions {
		size, _ := strconv.ParseFloat(p.Size, 64)
		avgPrice, _ := strconv.ParseFloat(p.AvgPrice, 64)
		posVal, _ := strconv.ParseFloat(p.PositionValue, 64)
		upnl, _ := strconv.ParseFloat(p.UnrealisedPnl, 64)
		mark, _ := strconv.ParseFloat(p.MarkPrice, 64)
		liq, _ := strconv.ParseFloat(p.LiqPrice, 64)
		lev, _ := strconv.ParseFloat(p.Leverage, 64)

		evt := models.ExecutionEvent{
			EventType: models.ExecPositionUpdate,
			Timestamp: now,
			Payload: models.PositionUpdatePayload{
				Symbol:        p.Symbol,
				Side:          p.Side,
				Size:          size,
				AvgPrice:      avgPrice,
				PositionValue: posVal,
				UnrealisedPnl: upnl,
				MarkPrice:     mark,
				LiqPrice:      liq,
				Leverage:      lev,
			},
		}

		select {
		case c.eventCh <- evt:
		default:
			log.Printf("[PrivateWS] Event channel full, dropping position update for %s", p.Symbol)
		}
	}
}
