package models

import "time"

// ── Pattern Definitions ─────────────────────────────────────

type PatternName string

const (
	PatternStealthAccumulation  PatternName = "Stealth Accumulation"
	PatternAggressiveDistro     PatternName = "Aggressive Distribution"
	PatternWhaleSqueezeSetup    PatternName = "Whale Squeeze Setup"
	PatternCapitulationReversal PatternName = "Capitulation Reversal"
	PatternSmartMoneyShort      PatternName = "Smart Money Short"
	PatternSilentDistribution   PatternName = "Silent Distribution"
	PatternDivergentStrength    PatternName = "Divergent Strength"
	PatternBearishConvergence   PatternName = "Bearish Convergence"
	PatternLiqCascadeShort      PatternName = "Liquidation Cascade Short"
	PatternLiqCascadeLong       PatternName = "Liquidation Cascade Long"
	PatternAbsorption           PatternName = "Absorption Pattern"
	PatternHiddenSelling        PatternName = "Hidden Selling"
	PatternExhaustionTop        PatternName = "Exhaustion Top"
	PatternExhaustionBottom     PatternName = "Exhaustion Bottom"
	PatternMTFBullishConf       PatternName = "MTF Bullish Confluence"
	PatternMTFBearishConf       PatternName = "MTF Bearish Confluence"
)

type SignalDirection string

const (
	DirectionLong  SignalDirection = "LONG"
	DirectionShort SignalDirection = "SHORT"
)

type SignalGrade string

const (
	GradeAPlus SignalGrade = "A+"
	GradeA     SignalGrade = "A"
	GradeB     SignalGrade = "B"
)

type TradeStatus string

const (
	TradePending TradeStatus = "PENDING"
	TradeActive  TradeStatus = "ACTIVE"
	TradeTP1     TradeStatus = "TP1_HIT"
	TradeTP2     TradeStatus = "TP2_HIT"
	TradeTP3     TradeStatus = "TP3_HIT"
	TradeStopped TradeStatus = "STOPPED"
	TradeCancelled TradeStatus = "CANCELLED"
)

// TPPhase tracks which TP limit order is currently active
type TPPhase string

const (
	TPPhaseNone       TPPhase = ""           // no TP orders placed yet
	TPPhaseWaitingTP1 TPPhase = "WAITING_TP1" // TP1 limit order placed, waiting fill
	TPPhaseWaitingTP2 TPPhase = "WAITING_TP2" // TP2 limit order placed, waiting fill
	TPPhaseWaitingTP3 TPPhase = "WAITING_TP3" // TP3 limit order placed, waiting fill
	TPPhaseDone       TPPhase = "DONE"        // all TPs completed
)

// ── Market Data Types ───────────────────────────────────────

type Coin struct {
	Symbol          string
	BaseCoin        string
	QuoteCoin       string
	LaunchTime      int64
	Status          string
	Volume24h       float64
	Turnover24h     float64
	LastPrice       float64
	FundingRate     float64
	OpenInterest    float64
	NextFundingTime int64  // unix ms
	FundingInterval int    // hours (8 = every 8h)
}

type OHLCV struct {
	Timestamp int64
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	Turnover  float64
}

type OpenInterestPoint struct {
	Timestamp    int64
	OpenInterest float64
}

type OrderbookLevel struct {
	Price  float64
	Amount float64
}

type OrderbookSnapshot struct {
	Symbol    string
	Timestamp int64
	Bids      []OrderbookLevel
	Asks      []OrderbookLevel
}

type TakerVolume struct {
	Timestamp  int64
	BuyVolume  float64
	SellVolume float64
	BuySellRatio float64
}

type LongShortRatio struct {
	Timestamp int64
	BuyRatio  float64
	SellRatio float64
	Ratio     float64
}

// ── Analysis Types ──────────────────────────────────────────

type Trend int

const (
	TrendStrongDown Trend = -2
	TrendDown       Trend = -1
	TrendNeutral    Trend = 0
	TrendUp         Trend = 1
	TrendStrongUp   Trend = 2
)

type OrderbookBias int

const (
	OBBidHeavy   OrderbookBias = 2
	OBBidWall    OrderbookBias = 1
	OBBalanced   OrderbookBias = 0
	OBAskWall    OrderbookBias = -1
	OBAskHeavy   OrderbookBias = -2
)

type TimeframeMetrics struct {
	Timeframe     string
	OIChange      float64 // percentage
	OITrend       Trend
	PerpCVD       float64
	PerpCVDTrend  Trend
	SpotCVD       float64
	SpotCVDTrend  Trend
	OBImbalance   float64 // bid_vol / ask_vol
	OBBias        OrderbookBias
	BidWallPrice  float64
	BidWallSize   float64
	AskWallPrice  float64
	AskWallSize   float64
	FundingRate      float64
	LSRatio          float64
	LastPrice        float64
	Volume24h        float64
	NextFundingTime  int64
	FundingInterval  int
	ATR              float64 // Average True Range (14-period)
	PriceTrend       Trend   // price direction from recent candles
	PriceRangePos    float64 // 0-100: position within recent high-low range (0=bottom, 100=top)
}

type CoinAnalysis struct {
	Symbol     string
	Timestamp  time.Time
	Metrics    map[string]*TimeframeMetrics // key: "5", "15", "60", "240"
	LastPrice  float64
	Volume24h  float64
	FundingRate float64
}

// ── Signal Types ────────────────────────────────────────────

type Signal struct {
	ID             string
	Symbol         string
	Direction      SignalDirection
	Pattern        PatternName
	Grade          SignalGrade
	Confidence     int // 0-100

	EntryLow       float64
	EntryHigh      float64
	StopLoss       float64 // Legacy: kept for signal display, not used in trading
	TP1            float64
	TP2            float64
	TP3            float64
	RiskRewardTP1  float64
	RiskRewardTP2  float64
	RiskRewardTP3  float64

	DCALevel       float64 // Price level for first DCA entry

	Explanation    string
	Metrics        map[string]*TimeframeMetrics
	LSRatio        float64
	Volume24h      float64
	FundingRate    float64
	NextFundingTime int64
	FundingInterval int

	Timestamp      time.Time
}

// ── Trade Tracker Types ─────────────────────────────────────

type Trade struct {
	ID            int64
	SignalID      string
	Symbol        string
	Direction     SignalDirection
	Pattern       PatternName
	Grade         SignalGrade

	EntryPrice    float64
	StopLoss      float64 // Legacy: not used in live trading
	TP1           float64
	TP2           float64
	TP3           float64

	ExitPrice     float64
	Status        TradeStatus
	PnLPercent    float64
	CurrentPrice  float64

	// Execution fields
	OrderID        string  // Bybit order ID for entry
	AvgEntryPrice  float64 // Weighted average entry (updated after DCA)
	TotalQty       float64 // Total position qty (initial + DCA)
	RemainingQty   float64 // Remaining qty after partial TP closes
	DCACount       int     // Number of DCA entries done
	MarginUsed     float64 // Total margin deployed (USD)
	MarginPerEntry float64 // Margin for each individual entry/DCA ($)
	LastDCAPrice   float64 // Price of the most recent DCA entry

	// TP limit order tracking
	TP1OrderID     string  // Bybit order ID for TP1 limit order
	TP2OrderID     string  // Bybit order ID for TP2 limit order
	TP3OrderID     string  // Bybit order ID for TP3 limit order
	TPPhase        TPPhase // Current TP phase

	OpenedAt      time.Time
	ClosedAt      *time.Time
	MovedToTP1At  *time.Time
	MovedToTP2At  *time.Time
}

type TradeStats struct {
	TotalTrades      int
	WinTrades        int
	LossTrades       int
	ActiveTrades     int
	CancelledTrades  int
	WinRate          float64
	TotalPnL         float64
	AvgWin         float64
	AvgLoss        float64
	BestTrade      float64
	WorstTrade     float64
	TP1Count       int
	TP2Count       int
	TP3Count       int
	TotalMargin    float64

	PatternStats   map[PatternName]*PatternStat
}

type PatternStat struct {
	Name       PatternName
	Total      int
	Wins       int
	Losses     int
	WinRate    float64
	AvgPnL     float64
}
