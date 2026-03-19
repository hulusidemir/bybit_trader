package models

import "sync"

// ── Event-Driven Architecture Types ─────────────────────────
// All exchange WebSocket data is normalized into MarketEvent and
// flows through a single channel. Zero REST calls for market data.

type EventType string

const (
	EventTick      EventType = "TICK"
	EventOIUpdate  EventType = "OI_UPDATE"
	EventOBUpdate  EventType = "OB_UPDATE"
	EventCVDUpdate EventType = "CVD_UPDATE"
)

// MarketEvent is the universal envelope for all exchange WebSocket data.
// Producers (DataProviders) normalize raw exchange messages into this format.
// Consumers (AnalysisEngine) type-assert Payload based on EventType.
type MarketEvent struct {
	Exchange  string    // "bybit", "binance", "okx", etc.
	Symbol    string    // Bybit-format symbol (e.g., "BTCUSDT")
	EventType EventType // Discriminator for Payload type assertion
	Payload   any       // One of: TickPayload, OIPayload, OBPayload, CVDPayload, KlinePayload
	Timestamp int64     // Unix milliseconds (exchange time, not local)
}

// ── Typed Event Payloads ────────────────────────────────────

type TickPayload struct {
	LastPrice    float64
	Volume24h    float64
	Turnover24h  float64
	FundingRate  float64
	OpenInterest float64
}

type OIPayload struct {
	OpenInterest float64
	ChangePct    float64 // OI change %, if computable from stream
}

type OBPayload struct {
	IsFutures bool
	Bids      []OrderbookLevel
	Asks      []OrderbookLevel
}

type CVDPayload struct {
	IsSpot     bool    // true = spot market, false = perpetual futures
	BuyVolume  float64 // Taker buy volume in this update
	SellVolume float64 // Taker sell volume in this update
	NetDelta   float64 // BuyVolume - SellVolume (cumulative delta)
}

// ── In-Memory Symbol State ──────────────────────────────────
// Accumulated from MarketEvents. Lock-free reads via RWMutex.

type SymbolState struct {
	Mu sync.RWMutex

	Symbol      string
	LastPrice   float64
	Volume24h   float64
	Turnover24h float64
	FundingRate float64

	// Per-exchange orderbooks (latest snapshot per exchange)
	FuturesOBs map[string]*OrderbookSnapshot // key: exchange name
	SpotOBs    map[string]*OrderbookSnapshot

	// Per-exchange OI (latest value per exchange)
	OI map[string]float64

	// Per-exchange cumulative volume delta
	PerpCVD map[string]float64 // key: exchange name
	SpotCVD map[string]float64

	// OrderFlow tracking state
	OIHistory   []OISnapshot   // rolling OI snapshots for rate-of-change
	WallHistory []WallSnapshot // recent orderbook wall positions
	PriceTicks  []PriceTick    // recent price ticks for absorption detection
	CVDHistory  []CVDSnapshot  // rolling CVD snapshots for divergence detection

	// Funding metadata
	NextFundingTime int64
	FundingInterval int

	LastUpdate int64 // unix ms of last event processed
	EventCount int64 // total events received for this symbol
}

// NewSymbolState creates a zero-value state ready for event accumulation.
func NewSymbolState(symbol string) *SymbolState {
	return &SymbolState{
		Symbol:     symbol,
		FuturesOBs: make(map[string]*OrderbookSnapshot),
		SpotOBs:    make(map[string]*OrderbookSnapshot),
		OI:         make(map[string]float64),
		PerpCVD:    make(map[string]float64),
		SpotCVD:    make(map[string]float64),
	}
}

// OrderFlow tracking buffer sizes
const (
	MaxOIHistory   = 120 // ~2 minutes of 1s OI updates
	MaxWallHistory = 60  // ~1 minute of wall snapshots
	MaxPriceTicks  = 300 // ~5 minutes of price ticks
	MaxCVDHistory  = 120 // ~2 minutes of CVD snapshots
)

// ── OrderFlow Tracking Types ────────────────────────────────

type OISnapshot struct {
	Timestamp int64
	TotalOI   float64
}

type WallSnapshot struct {
	Timestamp int64
	Side      string  // "bid" or "ask"
	Price     float64
	Size      float64
}

type PriceTick struct {
	Timestamp int64
	Price     float64
}

type CVDSnapshot struct {
	Timestamp int64
	PerpCVD   float64
	SpotCVD   float64
}

// ── Execution Event Types (Private WebSocket) ───────────────
// Order and position updates from Bybit Private WebSocket.
// Flows through ExecutionEventCh → Monitor goroutine.

type ExecEventType string

const (
	ExecOrderUpdate    ExecEventType = "ORDER_UPDATE"
	ExecPositionUpdate ExecEventType = "POSITION_UPDATE"
)

// ExecutionEvent is the envelope for private WebSocket data.
type ExecutionEvent struct {
	EventType ExecEventType
	Payload   any   // OrderUpdatePayload or PositionUpdatePayload
	Timestamp int64 // Unix milliseconds
}

// OrderUpdatePayload represents a Bybit order status change.
type OrderUpdatePayload struct {
	OrderID     string
	Symbol      string
	Side        string  // "Buy" or "Sell"
	OrderType   string  // "Limit", "Market"
	OrderStatus string  // "New", "PartiallyFilled", "Filled", "Cancelled", "Rejected", "Deactivated"
	AvgPrice    float64
	CumExecQty  float64 // filled qty so far
	CumExecVal  float64 // filled value so far
	Qty         float64 // original order qty
	ReduceOnly  bool
	CreatedTime int64
}

// PositionUpdatePayload represents a Bybit position change.
type PositionUpdatePayload struct {
	Symbol        string
	Side          string  // "Buy", "Sell", "None"
	Size          float64
	AvgPrice      float64
	PositionValue float64
	UnrealisedPnl float64
	MarkPrice     float64
	LiqPrice      float64
	Leverage      float64
}
