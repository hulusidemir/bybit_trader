package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"bybit_trader/pkg/analysis"
	"bybit_trader/pkg/models"
	"bybit_trader/pkg/tracker"

	"github.com/gorilla/websocket"
)

//go:embed static
var staticFiles embed.FS

// ═══════════════════════════════════════════════════════════
//  WebSocket Hub — broadcasts events to all dashboard clients
// ═══════════════════════════════════════════════════════════

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// wsMessage is the JSON envelope sent to dashboard clients.
type wsMessage struct {
	Type    string `json:"type"`              // "trade_update", "stats_update", "active_update", "log"
	Payload any    `json:"payload,omitempty"` // typed data
	Message string `json:"message,omitempty"` // for type=log
	Level   string `json:"level,omitempty"`   // for type=log
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

// Hub manages WebSocket client lifecycle and broadcasting.
type Hub struct {
	mu         sync.RWMutex
	clients    map[*wsClient]struct{}
	broadcast  chan []byte
	register   chan *wsClient
	unregister chan *wsClient
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*wsClient]struct{}),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *wsClient),
		unregister: make(chan *wsClient),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = struct{}{}
			h.mu.Unlock()
			log.Printf("[WS-Hub] Client connected (%d total)", h.clientCount())

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("[WS-Hub] Client disconnected (%d total)", h.clientCount())

		case msg := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- msg:
				default:
					// Slow consumer — drop & disconnect
					h.mu.RUnlock()
					h.mu.Lock()
					delete(h.clients, client)
					close(client.send)
					h.mu.Unlock()
					h.mu.RLock()
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// broadcastJSON marshals and sends a wsMessage to all clients (non-blocking).
func (h *Hub) broadcastJSON(msg wsMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case h.broadcast <- data:
	default:
		// broadcast channel full — drop
	}
}

// BalanceFetcher can retrieve wallet balance from the exchange.
type BalanceFetcher interface {
	GetWalletBalance() (float64, error)
}

// ═══════════════════════════════════════════════════════════
//  Server
// ═══════════════════════════════════════════════════════════

type Server struct {
	store          *tracker.Store
	port           int
	hub            *Hub
	execEventCh    <-chan models.ExecutionEvent
	balanceFetcher BalanceFetcher
	strategyMode   string
}

func NewServer(store *tracker.Store, port int, execEventCh <-chan models.ExecutionEvent, bf BalanceFetcher, strategyMode string) *Server {
	return &Server{
		store:          store,
		port:           port,
		hub:            newHub(),
		execEventCh:    execEventCh,
		balanceFetcher: bf,
		strategyMode:   strategyMode,
	}
}

func (s *Server) Start() {
	go s.hub.run()
	if s.execEventCh != nil {
		go s.eventBroadcaster()
	}

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/trades", s.handleTrades)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/active", s.handleActive)
	mux.HandleFunc("/api/anomalies", s.handleAnomalies)
	mux.HandleFunc("/api/patterns", handlePatterns)
	mux.HandleFunc("/api/balance", s.handleBalance)
	mux.HandleFunc("/api/config", s.handleConfig)

	// WebSocket endpoint
	mux.HandleFunc("/ws", s.handleWS)

	// Static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("Failed to setup static files: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[Dashboard] Starting on http://localhost%s (WebSocket enabled)", addr)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("Dashboard server failed: %v", err)
		}
	}()
}

// handleWS upgrades HTTP to WebSocket and manages the client lifecycle.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] Upgrade failed: %v", err)
		return
	}

	client := &wsClient{
		conn: conn,
		send: make(chan []byte, 64),
	}
	s.hub.register <- client

	go s.writePump(client)
	go s.readPump(client)
}

// writePump sends queued messages to the WebSocket connection.
func (s *Server) writePump(c *wsClient) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump reads (and discards) messages from the client to detect disconnection.
func (s *Server) readPump(c *wsClient) {
	defer func() {
		s.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			break
		}
	}
}

// eventBroadcaster reads ExecutionEvents and broadcasts to all WS clients.
func (s *Server) eventBroadcaster() {
	log.Println("[WS-Hub] Event broadcaster started")
	for evt := range s.execEventCh {
		switch evt.EventType {
		case models.ExecOrderUpdate:
			s.hub.broadcastJSON(wsMessage{
				Type:    "trade_update",
				Payload: evt.Payload,
				Message: formatOrderEvent(evt.Payload),
				Level:   "exec",
			})
		case models.ExecPositionUpdate:
			s.hub.broadcastJSON(wsMessage{
				Type:    "trade_update",
				Payload: evt.Payload,
				Message: formatPositionEvent(evt.Payload),
				Level:   "exec",
			})
		}

		// Also push a log event for the terminal
		s.hub.broadcastJSON(wsMessage{
			Type:    "log",
			Message: formatExecLog(evt),
			Level:   "exec",
		})
	}
}

func formatOrderEvent(payload any) string {
	p, ok := payload.(models.OrderUpdatePayload)
	if !ok {
		return "order update"
	}
	return fmt.Sprintf("%s %s %s | %s | avg=%.6f qty=%.4f", p.Side, p.Symbol, p.OrderType, p.OrderStatus, p.AvgPrice, p.CumExecQty)
}

func formatPositionEvent(payload any) string {
	p, ok := payload.(models.PositionUpdatePayload)
	if !ok {
		return "position update"
	}
	return fmt.Sprintf("%s %s | size=%.4f avg=%.6f uPnL=%.4f", p.Side, p.Symbol, p.Size, p.AvgPrice, p.UnrealisedPnl)
}

func formatExecLog(evt models.ExecutionEvent) string {
	switch evt.EventType {
	case models.ExecOrderUpdate:
		return "📋 " + formatOrderEvent(evt.Payload)
	case models.ExecPositionUpdate:
		return "📈 " + formatPositionEvent(evt.Payload)
	default:
		return "execution event"
	}
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	filter := r.URL.Query().Get("filter")

	var trades interface{}
	var err error

	switch filter {
	case "active":
		trades, err = s.store.GetActiveTrades()
	case "closed":
		trades, err = s.store.GetClosedTrades()
	default:
		trades, err = s.store.GetAllTrades()
	}

	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	json.NewEncoder(w).Encode(trades)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	stats, err := s.store.GetStats()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleActive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	trades, err := s.store.GetActiveTrades()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	json.NewEncoder(w).Encode(trades)
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if s.balanceFetcher == nil {
		json.NewEncoder(w).Encode(map[string]float64{"balance": 0})
		return
	}
	bal, err := s.balanceFetcher.GetWalletBalance()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]float64{"balance": bal})
}

func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	anomalies, err := s.store.GetAnomalies()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	json.NewEncoder(w).Encode(anomalies)
}

func handlePatterns(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	type PatternInfo struct {
		Name      string `json:"name"`
		Direction string `json:"direction"`
		Desc      string `json:"description"`
	}

	result := make([]PatternInfo, 0, len(analysis.AllPatterns))
	for _, p := range analysis.AllPatterns {
		result = append(result, PatternInfo{
			Name:      string(p.Name),
			Direction: string(p.Direction),
			Desc:      p.Description,
		})
	}

	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	json.NewEncoder(w).Encode(map[string]string{
		"strategyMode": s.strategyMode,
	})
}
