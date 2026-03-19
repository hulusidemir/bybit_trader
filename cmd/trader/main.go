package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"bybit_trader/pkg/analysis"
	"bybit_trader/pkg/config"
	"bybit_trader/pkg/dashboard"
	"bybit_trader/pkg/exchange"
	"bybit_trader/pkg/exchange/binance"
	"bybit_trader/pkg/exchange/bitget"
	"bybit_trader/pkg/exchange/bybit"
	"bybit_trader/pkg/exchange/coinbase"
	"bybit_trader/pkg/exchange/kraken"
	"bybit_trader/pkg/exchange/okx"
	"bybit_trader/pkg/executor"
	"bybit_trader/pkg/models"
	"bybit_trader/pkg/signals"
	"bybit_trader/pkg/telegram"
	"bybit_trader/pkg/tracker"
)

const version = "2.0.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("═══════════════════════════════════════")
	log.Println("  Bybit Trader v" + version)
	log.Println("  Event-Driven HFT Architecture")
	log.Println("═══════════════════════════════════════")

	// ── Load Config ────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}
	log.Printf("Config loaded: screener=%ds, minVol=$%.0f, port=%d",
		cfg.ScanIntervalSec, cfg.MinVolume24H, cfg.DashboardPort)

	// ── Root Context ───────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Initialize Clients ─────────────────────────────
	bybitClient := bybit.NewClient()
	tgBot := telegram.NewBot(cfg.TelegramBotToken, cfg.TelegramChatID)
	defer func() {
		if r := recover(); r != nil {
			reason := fmt.Sprintf("panic: %v", r)
			log.Printf("Fatal panic: %v", r)
			tgBot.SendShutdown(reason)
			panic(r)
		}
	}()

	// ── Build Native WebSocket Data Providers ──────────
	// All 6 exchanges use native WebSocket streaming.
	// Zero REST polling for market data.
	providers := []exchange.DataProvider{
		bybit.NewWSProvider(),
		binance.NewWSProvider(),
		okx.NewWSProvider(),
		coinbase.NewWSProvider(),
		kraken.NewWSProvider(),
		bitget.NewWSProvider(),
	}
	log.Printf("Data providers: %d exchanges (all native WebSocket)", len(providers))

	// ── Core Event Channels ────────────────────────────
	// HotListCh: Screener → Subscription Manager (coin discovery)
	// MarketEventCh: All Providers → Analysis Engine (market data)
	// SignalCh: Analysis Engine → Signal Processor (trade signals)
	hotListCh := make(chan []string, 1)
	marketEventCh := make(chan models.MarketEvent, 8192)
	signalCh := make(chan *models.Signal, 64)

	// ── Initialize Trade Store ─────────────────────────
	store, err := tracker.NewStore("trades.db")
	if err != nil {
		log.Fatalf("Store error: %v", err)
	}
	defer store.Close()

	// ── Initialize Trade Client & Executor ─────────────
	var exec *executor.Executor
	var execEventCh <-chan models.ExecutionEvent
	var tradeClient *bybit.TradeClient
	if cfg.TradingEnabled {
		tradeClient = bybit.NewTradeClient(cfg.BybitAPIKey, cfg.BybitAPISecret, cfg.BybitTestnet)
		if err := tradeClient.LoadInstruments(); err != nil {
			log.Fatalf("Failed to load instrument data: %v", err)
		}
		balance, err := tradeClient.GetWalletBalance()
		if err != nil {
			log.Printf("Warning: could not check wallet balance: %v", err)
		} else {
			log.Printf("Wallet balance: $%.2f USDT", balance)
		}
		exec = executor.New(tradeClient, store, cfg.Leverage, cfg.MarginPerTrade, cfg.MaxDCACount, cfg.DCAThresholdPct,
			cfg.CoinDCAOverrides, cfg.TP1Pct, cfg.TP2Pct, cfg.TP3Pct)
		log.Printf("🔥 LIVE TRADING ENABLED: %dx leverage, $%.0f margin/trade, DCA %.0f%% (max %d)",
			cfg.Leverage, cfg.MarginPerTrade, cfg.DCAThresholdPct, cfg.MaxDCACount)

		// Launch Private WebSocket for real-time order/position updates
		privateWS := bybit.NewPrivateWSClient(cfg.BybitAPIKey, cfg.BybitAPISecret, cfg.BybitTestnet)
		execEventCh = privateWS.Start(ctx)
		log.Println("🔌 Private WebSocket started (order + position streams)")
	} else {
		log.Println("📊 Signal-only mode (TRADING_ENABLED=false)")
	}

	// ── Start Event-Driven Monitor ─────────────────────
	// Fan-out: execEventCh → monitor + dashboard WS broadcaster
	var monitorCh <-chan models.ExecutionEvent
	var dashExecCh <-chan models.ExecutionEvent

	if execEventCh != nil {
		mCh := make(chan models.ExecutionEvent, 256)
		dCh := make(chan models.ExecutionEvent, 256)
		monitorCh = mCh
		dashExecCh = dCh
		go fanOutExecEvents(ctx, execEventCh, mCh, dCh)
	}

	monitor := tracker.NewMonitor(store, tgBot, exec, cfg.OrderTimeoutSec, monitorCh)
	monitor.Start()

	// ── Start Dashboard ────────────────────────────────
	var balanceFetcher dashboard.BalanceFetcher
	if cfg.TradingEnabled {
		balanceFetcher = tradeClient
	}
	dash := dashboard.NewServer(store, cfg.DashboardPort, dashExecCh, balanceFetcher)
	dash.Start()

	// ── Fetch Instruments (one-time REST) ──────────────
	coins, err := bybitClient.FetchInstruments()
	if err != nil {
		log.Fatalf("Failed to fetch instruments: %v", err)
	}
	log.Printf("Fetched %d USDT perpetual instruments", len(coins))

	// ── Startup Notification ───────────────────────────
	blacklistSlice := make([]string, 0, len(cfg.BlacklistedCoins))
	for sym := range cfg.BlacklistedCoins {
		blacklistSlice = append(blacklistSlice, sym)
	}
	tgBot.SendStartup(telegram.StartupInfo{
		CoinCount:        len(coins),
		Version:          version,
		TradingEnabled:   cfg.TradingEnabled,
		Testnet:          cfg.BybitTestnet,
		Leverage:         cfg.Leverage,
		MarginPerTrade:   cfg.MarginPerTrade,
		MaxDCACount:      cfg.MaxDCACount,
		DCAThresholdPct:  cfg.DCAThresholdPct,
		TP1Pct:           cfg.TP1Pct,
		TP2Pct:           cfg.TP2Pct,
		TP3Pct:           cfg.TP3Pct,
		BlacklistedCoins: blacklistSlice,
		MarginOverrides:  cfg.CoinMarginOverrides,
		DCAOverrides:     cfg.CoinDCAOverrides,
	})

	// ═══════════════════════════════════════════════════════
	//  EVENT-DRIVEN GOROUTINE ORCHESTRA
	//
	//  ┌───────────┐     HotListCh     ┌──────────────┐
	//  │  Screener  │ ────────────────▶ │  Subscription │
	//  │  (REST 5m) │                   │   Manager     │
	//  └───────────┘                   └──────┬───────┘
	//                                         │ Subscribe()
	//                              ┌──────────┼──────────┐
	//                              ▼          ▼          ▼
	//                         ┌────────┐ ┌────────┐ ┌────────┐
	//                         │ Bybit  │ │Binance │ │  OKX   │ ...
	//                         │Provider│ │Provider│ │Provider│
	//                         └───┬────┘ └───┬────┘ └───┬────┘
	//                             │          │          │
	//                             ▼          ▼          ▼
	//                         ┌──────── Fan-In ────────────┐
	//                         │      MarketEventCh         │
	//                         └─────────────┬──────────────┘
	//                                       ▼
	//                              ┌─────────────────┐
	//                              │  Analysis Engine │
	//                              │  (EventEngine)   │
	//                              └────────┬────────┘
	//                                       │ SignalCh
	//                                       ▼
	//                              ┌─────────────────┐
	//                              │ Signal Processor │
	//                              │ (Dedup+TG+Exec) │
	//                              └─────────────────┘
	// ═══════════════════════════════════════════════════════

	// 1. Fan-In: Each provider's WebSocket stream → single MarketEventCh
	for _, p := range providers {
		go fanInEvents(ctx, p, marketEventCh)
	}
	log.Printf("Fan-in started: %d providers → MarketEventCh", len(providers))

	// 2. Screener: REST every ScanInterval → HotListCh (ONLY remaining REST)
	go screener(ctx, bybitClient, cfg, coins, hotListCh)

	// 3. Subscription Manager: HotListCh → Subscribe() on all providers
	go subscriptionManager(ctx, providers, hotListCh)

	// 4. Analysis Engine: MarketEventCh → SignalCh
	tpCfg := signals.TPConfig{
		TP1Pct:                cfg.TP1Pct,
		TP2Pct:                cfg.TP2Pct,
		TP3Pct:                cfg.TP3Pct,
		DCAThresholdPct:       cfg.DCAThresholdPct,
		CoinDCAOverrides:      cfg.CoinDCAOverrides,
		ShortFundingRateLimit: cfg.ShortFundingRateLimit,
	}
	engine := analysis.NewEventEngine()
	go analysisLoop(ctx, engine, tpCfg, marketEventCh, signalCh)

	// 5. Signal Processor: SignalCh → Telegram + Trade Execution
	go signalProcessor(ctx, signalCh, tgBot, store, exec, cfg)

	log.Println("═══════════════════════════════════════")
	log.Println("  All goroutines launched. Listening...")
	log.Println("═══════════════════════════════════════")

	// ── Graceful Shutdown ──────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	sig := <-quit
	reason := fmt.Sprintf("signal: %s", sig.String())
	log.Printf("Shutting down gracefully: %s", reason)
	cancel() // cancels ctx → all goroutines wind down
	monitor.Stop()
	tgBot.SendShutdown(reason)
}

// ════════════════════════════════════════════════════════════
//  GOROUTINE 1: Fan-In (per provider)
// ════════════════════════════════════════════════════════════

// fanInEvents drains a single provider's event stream into the shared channel.
// Non-blocking send: drops events if MarketEventCh is full.
// HFT rule: never block the WebSocket reader goroutine.
func fanInEvents(ctx context.Context, p exchange.DataProvider, out chan<- models.MarketEvent) {
	ch := p.StreamEvents(ctx)
	name := p.Name()
	log.Printf("[FanIn] %s: stream connected", name)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[FanIn] %s: shutting down", name)
			return
		case evt, ok := <-ch:
			if !ok {
				log.Printf("[FanIn] %s: stream closed", name)
				return
			}
			// Non-blocking send: HFT hot path — never block the producer
			select {
			case out <- evt:
			default:
				// MarketEventCh full — drop stale event (acceptable in HFT)
			}
		}
	}
}

// ════════════════════════════════════════════════════════════
//  GOROUTINE 2: Screener (REST — only remaining REST call)
// ════════════════════════════════════════════════════════════

// screener discovers high-volume coins via Bybit REST API on a fixed interval.
// Pushes the ranked symbol list into HotListCh for the subscription manager.
func screener(ctx context.Context, client *bybit.Client, cfg *config.Config, coins []models.Coin, hotListCh chan []string) {
	discover := func() {
		tickers, err := client.FetchTickers()
		if err != nil {
			log.Printf("[Screener] Error fetching tickers: %v", err)
			return
		}

		type ranked struct {
			Symbol   string
			Turnover float64
		}
		var candidates []ranked
		for _, coin := range coins {
			t, ok := tickers[coin.Symbol]
			if !ok {
				continue
			}
			if t.Turnover24h >= cfg.MinVolume24H && !cfg.IsBlacklisted(coin.Symbol) {
				candidates = append(candidates, ranked{coin.Symbol, t.Turnover24h})
			}
		}

		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Turnover > candidates[j].Turnover
		})

		symbols := make([]string, len(candidates))
		for i, c := range candidates {
			symbols[i] = c.Symbol
		}

		log.Printf("[Screener] Hot list updated: %d symbols (min vol $%.0f)", len(symbols), cfg.MinVolume24H)

		// Non-blocking push: replace stale list if channel is full
		select {
		case hotListCh <- symbols:
		default:
			select {
			case <-hotListCh:
			default:
			}
			hotListCh <- symbols
		}
	}

	// Initial discovery immediately
	discover()

	ticker := time.NewTicker(time.Duration(cfg.ScanIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[Screener] Shutting down")
			return
		case <-ticker.C:
			discover()
		}
	}
}

// ════════════════════════════════════════════════════════════
//  GOROUTINE 3: Subscription Manager
// ════════════════════════════════════════════════════════════

// subscriptionManager listens for hot list updates and re-subscribes
// all data providers to the new symbol set. Handles the dynamic
// symbol rotation that is core to the screener → stream pipeline.
func subscriptionManager(ctx context.Context, providers []exchange.DataProvider, hotListCh <-chan []string) {
	for {
		select {
		case <-ctx.Done():
			log.Println("[SubMgr] Shutting down")
			return
		case symbols := <-hotListCh:
			log.Printf("[SubMgr] Updating subscriptions: %d symbols across %d providers",
				len(symbols), len(providers))

			var wg sync.WaitGroup
			for _, p := range providers {
				wg.Add(1)
				go func(prov exchange.DataProvider) {
					defer wg.Done()
					if err := prov.Subscribe(ctx, symbols); err != nil {
						log.Printf("[SubMgr] %s subscribe error: %v", prov.Name(), err)
					} else {
						log.Printf("[SubMgr] %s subscribed to %d symbols", prov.Name(), len(symbols))
					}
				}(p)
			}
			wg.Wait()
			log.Printf("[SubMgr] All providers subscribed")
		}
	}
}

// ════════════════════════════════════════════════════════════
//  GOROUTINE 4: Analysis Loop
// ════════════════════════════════════════════════════════════

// analysisLoop is the core event processor. Every MarketEvent flows through
// here. The EventEngine updates in-memory state and evaluates signal
// conditions on each event — sub-millisecond latency target.
func analysisLoop(ctx context.Context, engine *analysis.EventEngine, tpCfg signals.TPConfig, events <-chan models.MarketEvent, signalCh chan<- *models.Signal) {
	var evtCount int64
	logTicker := time.NewTicker(30 * time.Second)
	defer logTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Analysis] Shutting down (processed %d events)", evtCount)
			return
		case <-logTicker.C:
			log.Printf("[Analysis] Events processed: %d", evtCount)
		case evt := <-events:
			evtCount++

			mtfResults := engine.ProcessEvent(evt)
			if len(mtfResults) == 0 {
				continue
			}

			// Signal generation (quality gates, TP/SL calc) runs here
			// to avoid import cycle between analysis ↔ signals packages
			sigs := signals.GenerateSignals(mtfResults, tpCfg)
			for _, sig := range sigs {
				select {
				case signalCh <- sig:
				default:
					log.Printf("[Analysis] Signal channel full, dropping: %s %s %s",
						sig.Direction, sig.Symbol, sig.Pattern)
				}
			}
		}
	}
}

// ════════════════════════════════════════════════════════════
//  GOROUTINE 5: Signal Processor
// ════════════════════════════════════════════════════════════

// signalProcessor handles signal deduplication, Telegram notification,
// and trade execution. Consumes from SignalCh.
func signalProcessor(
	ctx context.Context,
	signalCh <-chan *models.Signal,
	tgBot *telegram.Bot,
	store *tracker.Store,
	exec *executor.Executor,
	cfg *config.Config,
) {
	// ── Cooldown state ─────────────────────────────────
	// Symbol cooldown: 30 min per symbol (prevents re-signaling same coin)
	// Global cooldown: 5 min between any trade (prevents mass-opening)
	const symbolCooldown = 30 * time.Minute
	const globalCooldown = 5 * time.Minute

	recentSignals := make(map[string]time.Time) // key: symbol → last signal time
	var lastGlobalSignal time.Time

	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[Signals] Shutting down")
			return

		case <-cleanupTicker.C:
			// Purge expired symbol cooldowns
			now := time.Now()
			for k, t := range recentSignals {
				if now.Sub(t) > symbolCooldown {
					delete(recentSignals, k)
				}
			}

		case sig := <-signalCh:
			now := time.Now()

			// ── Global Cooldown: 2 min between any signal ──
			if !lastGlobalSignal.IsZero() && now.Sub(lastGlobalSignal) < globalCooldown {
				continue
			}

			// ── Symbol Cooldown: 15 min per symbol ─────────
			if lastTime, exists := recentSignals[sig.Symbol]; exists {
				if now.Sub(lastTime) < symbolCooldown {
					continue
				}
			}

			// ── Active Position Guard ───────────────────
			// Skip if there's already an open trade for this symbol.
			// Prevents sending misleading signal then error message.
			if existing, _ := store.GetActiveTradeBySymbol(sig.Symbol); existing != nil {
				continue
			}

			recentSignals[sig.Symbol] = now
			lastGlobalSignal = now

			// ── Telegram Notification ──────────────────
			msg := signals.FormatTelegramMessage(sig)
			if err := tgBot.SendMessage(msg); err != nil {
				log.Printf("[Signals] Telegram error: %v", err)
				continue
			}

			// ── Trade Execution ────────────────────────
			var orderID string
			var entryPrice, qty float64
			var execErr error

			if exec != nil {
				margin := cfg.MarginForCoin(sig.Symbol)
				orderID, entryPrice, qty, execErr = exec.OpenPosition(sig, margin)
				if execErr != nil {
					log.Printf("[Signals] ⚠️ Trade execution failed for %s: %v", sig.Symbol, execErr)
					tgBot.SendMessage("⚠️ *EMİR HATA* — " + sig.Symbol + "\n" + execErr.Error())
					continue
				}
			} else {
				entryPrice = (sig.EntryLow + sig.EntryHigh) / 2
			}

			// ── Persist Trade ──────────────────────────
			trade, err := store.CreateTrade(sig, orderID, entryPrice, qty, cfg.MarginForCoin(sig.Symbol))
			if err != nil {
				log.Printf("[Signals] Error creating trade: %v", err)
				continue
			}

			log.Printf("🔥 Signal: %s %s %s (Score: %d, Grade: %s, Trade #%d, OrderID: %s)",
				sig.Direction, sig.Symbol, sig.Pattern, sig.Confidence, sig.Grade, trade.ID, orderID)
		}
	}
}

// ════════════════════════════════════════════════════════════
//  Fan-Out: ExecutionEvent → Monitor + Dashboard
// ════════════════════════════════════════════════════════════

// fanOutExecEvents duplicates each ExecutionEvent to both the monitor
// and dashboard channels. Non-blocking sends: never block the private WS.
func fanOutExecEvents(ctx context.Context, src <-chan models.ExecutionEvent, monCh, dashCh chan<- models.ExecutionEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-src:
			if !ok {
				return
			}
			// Non-blocking send to monitor (critical path — monitor handles trade logic)
			select {
			case monCh <- evt:
			default:
			}
			// Non-blocking send to dashboard (best-effort — only for UI updates)
			select {
			case dashCh <- evt:
			default:
			}
		}
	}
}
