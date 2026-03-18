package exchange

import (
	"context"
	"log"
	"sync"
	"time"

	"bybit_trader/pkg/models"
)

// ════════════════════════════════════════════════════════════
// DataProvider — Event-Driven WebSocket Streaming Interface
//
// All market data flows through WebSocket streams.
// Zero REST calls for ongoing market data.
// Each provider manages its own WebSocket connections internally.
// ════════════════════════════════════════════════════════════

type DataProvider interface {
	// Name returns the exchange identifier (e.g., "bybit", "binance").
	Name() string

	// Subscribe updates the set of symbols this provider streams data for.
	// Must handle dynamic re-subscription: add new symbols, drop removed ones.
	// Called each time the screener publishes a new hot list.
	// Implementations must be safe for concurrent calls and non-blocking.
	Subscribe(ctx context.Context, symbols []string) error

	// StreamEvents returns a read-only channel of normalized MarketEvents.
	// The channel remains open for the lifetime of ctx.
	// Provider pushes all WebSocket data (ticks, OI, orderbook, CVD, klines)
	// through this single channel. Callers must NOT close the returned channel.
	StreamEvents(ctx context.Context) <-chan models.MarketEvent
}

// ════════════════════════════════════════════════════════════
// LegacyDataProvider — REST-based interface (deprecated)
//
// Kept for backward compatibility during migration.
// Exchange adapters should migrate to DataProvider.
// ════════════════════════════════════════════════════════════

type LegacyDataProvider interface {
	Name() string
	SupportsSymbol(bybitSymbol string) bool
	FetchOI(bybitSymbol, tfKey string, limit int) ([]models.OpenInterestPoint, error)
	FetchPerpTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error)
	FetchSpotTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error)
	FetchFuturesOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error)
	FetchSpotOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error)
	FetchLSRatio(bybitSymbol, tfKey string, limit int) ([]models.LongShortRatio, error)
}

// ════════════════════════════════════════════════════════════
// LegacyAdapter — Bridge from REST to Streaming Interface
//
// Wraps a LegacyDataProvider to satisfy the DataProvider interface
// using periodic REST polling under the hood. This is the migration
// bridge — exchange adapters should implement DataProvider natively
// with WebSocket for production HFT use.
// ════════════════════════════════════════════════════════════

type LegacyAdapter struct {
	legacy  LegacyDataProvider
	eventCh chan models.MarketEvent

	mu      sync.Mutex
	symbols []string
	cancel  context.CancelFunc
}

// AdaptLegacy wraps a LegacyDataProvider into the new DataProvider interface.
// It polls REST endpoints at the given interval and emits MarketEvents.
func AdaptLegacy(legacy LegacyDataProvider) DataProvider {
	return &LegacyAdapter{
		legacy:  legacy,
		eventCh: make(chan models.MarketEvent, 256),
	}
}

func (a *LegacyAdapter) Name() string {
	return a.legacy.Name()
}

func (a *LegacyAdapter) Subscribe(ctx context.Context, symbols []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Stop previous polling goroutines
	if a.cancel != nil {
		a.cancel()
	}

	a.symbols = make([]string, len(symbols))
	copy(a.symbols, symbols)

	pollCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel

	// Start polling goroutine for each symbol
	for _, sym := range a.symbols {
		if !a.legacy.SupportsSymbol(sym) {
			continue
		}
		go a.pollSymbol(pollCtx, sym)
	}

	return nil
}

func (a *LegacyAdapter) StreamEvents(ctx context.Context) <-chan models.MarketEvent {
	return a.eventCh
}

// pollSymbol periodically fetches REST data and emits events.
// Temporary bridge — replace with native WebSocket in each provider.
func (a *LegacyAdapter) pollSymbol(ctx context.Context, symbol string) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	fetch := func() {
		now := time.Now().UnixMilli()
		name := a.legacy.Name()

		// Futures orderbook → OB_UPDATE event
		if ob, err := a.legacy.FetchFuturesOrderbook(symbol, 50); err == nil && ob != nil {
			a.emit(models.MarketEvent{
				Exchange:  name,
				Symbol:    symbol,
				EventType: models.EventOBUpdate,
				Payload:   models.OBPayload{IsFutures: true, Bids: ob.Bids, Asks: ob.Asks},
				Timestamp: now,
			})
		}

		// Spot orderbook → OB_UPDATE event
		if ob, err := a.legacy.FetchSpotOrderbook(symbol, 50); err == nil && ob != nil {
			a.emit(models.MarketEvent{
				Exchange:  name,
				Symbol:    symbol,
				EventType: models.EventOBUpdate,
				Payload:   models.OBPayload{IsFutures: false, Bids: ob.Bids, Asks: ob.Asks},
				Timestamp: now,
			})
		}

		// OI → OI_UPDATE event
		if points, err := a.legacy.FetchOI(symbol, "5", 2); err == nil && len(points) > 0 {
			last := points[len(points)-1]
			a.emit(models.MarketEvent{
				Exchange:  name,
				Symbol:    symbol,
				EventType: models.EventOIUpdate,
				Payload:   models.OIPayload{OpenInterest: last.OpenInterest},
				Timestamp: now,
			})
		}

		// Perp taker volume → CVD_UPDATE event
		if tv, err := a.legacy.FetchPerpTakerVolume(symbol, "5", 5); err == nil && len(tv) > 0 {
			last := tv[len(tv)-1]
			a.emit(models.MarketEvent{
				Exchange:  name,
				Symbol:    symbol,
				EventType: models.EventCVDUpdate,
				Payload: models.CVDPayload{
					IsSpot:     false,
					BuyVolume:  last.BuyVolume,
					SellVolume: last.SellVolume,
					NetDelta:   last.BuyVolume - last.SellVolume,
				},
				Timestamp: now,
			})
		}

		// Spot taker volume → CVD_UPDATE event
		if tv, err := a.legacy.FetchSpotTakerVolume(symbol, "5", 5); err == nil && len(tv) > 0 {
			last := tv[len(tv)-1]
			a.emit(models.MarketEvent{
				Exchange:  name,
				Symbol:    symbol,
				EventType: models.EventCVDUpdate,
				Payload: models.CVDPayload{
					IsSpot:     true,
					BuyVolume:  last.BuyVolume,
					SellVolume: last.SellVolume,
					NetDelta:   last.BuyVolume - last.SellVolume,
				},
				Timestamp: now,
			})
		}
	}

	// Initial fetch
	fetch()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetch()
		}
	}
}

func (a *LegacyAdapter) emit(evt models.MarketEvent) {
	select {
	case a.eventCh <- evt:
	default:
		log.Printf("[LegacyAdapter] %s channel full, dropping %s event for %s",
			a.legacy.Name(), evt.EventType, evt.Symbol)
	}
}

// AggregateOIChange calculates aggregated OI % change across multiple exchange series.
// It sums the first and last OI values from all series, then computes the total % change.
func AggregateOIChange(allSeries [][]models.OpenInterestPoint) float64 {
	if len(allSeries) == 0 {
		return 0
	}

	var totalFirst, totalLast float64
	valid := false
	for _, series := range allSeries {
		if len(series) < 2 {
			continue
		}
		totalFirst += series[0].OpenInterest
		totalLast += series[len(series)-1].OpenInterest
		valid = true
	}

	if !valid || totalFirst == 0 {
		return 0
	}
	return ((totalLast - totalFirst) / totalFirst) * 100
}
