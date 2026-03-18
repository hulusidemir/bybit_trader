package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bybit_trader/pkg/analysis"
	"bybit_trader/pkg/config"
	"bybit_trader/pkg/dashboard"
	"bybit_trader/pkg/exchange"
	"bybit_trader/pkg/exchange/binance"
	"bybit_trader/pkg/exchange/bybit"
	"bybit_trader/pkg/exchange/coinbase"
	"bybit_trader/pkg/exchange/okx"
	"bybit_trader/pkg/executor"
	"bybit_trader/pkg/models"
	"bybit_trader/pkg/signals"
	"bybit_trader/pkg/telegram"
	"bybit_trader/pkg/tracker"
)

const version = "1.0.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("═══════════════════════════════════════")
	log.Println("  Bybit Trader v" + version)
	log.Println("  Perpetual Futures Signal Scanner")
	log.Println("═══════════════════════════════════════")

	// ── Load Config ────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}
	log.Printf("Config loaded: scan=%ds, minVol=$%.0f, port=%d",
		cfg.ScanIntervalSec, cfg.MinVolume24H, cfg.DashboardPort)

	// ── Initialize Clients ─────────────────────────────
	bybitClient := bybit.NewClient()
	binanceClient := binance.NewClient()
	okxClient := okx.NewClient()
	coinbaseClient := coinbase.NewClient()
	tgBot := telegram.NewBot(cfg.TelegramBotToken, cfg.TelegramChatID)
	defer func() {
		if r := recover(); r != nil {
			reason := fmt.Sprintf("panic: %v", r)
			log.Printf("Fatal panic: %v", r)
			tgBot.SendShutdown(reason)
			panic(r)
		}
	}()

	// Build aggregated data providers (Bybit + Binance + OKX + Coinbase)
	providers := []exchange.DataProvider{
		bybit.NewProvider(bybitClient),
		binance.NewProvider(binanceClient),
		okxClient,      // OKX client implements DataProvider directly
		coinbaseClient, // Coinbase: spot orderbook depth
	}
	log.Printf("Data providers: %d exchanges (Bybit + Binance + OKX + Coinbase)", len(providers))
	engine := analysis.NewEngine(bybitClient, providers)

	// ── Initialize Trade Store ─────────────────────────
	store, err := tracker.NewStore("trades.db")
	if err != nil {
		log.Fatalf("Store error: %v", err)
	}
	defer store.Close()

	// ── Initialize Trade Client & Executor ─────────────
	var exec *executor.Executor

	if cfg.TradingEnabled {
		tradeClient := bybit.NewTradeClient(cfg.BybitAPIKey, cfg.BybitAPISecret, cfg.BybitTestnet)

		// Load instrument precision data
		if err := tradeClient.LoadInstruments(); err != nil {
			log.Fatalf("Failed to load instrument data: %v", err)
		}

		// Check wallet balance
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
	} else {
		log.Println("📊 Signal-only mode (TRADING_ENABLED=false)")
	}

	// ── Start Price Monitor ────────────────────────────
	monitor := tracker.NewMonitor(store, bybitClient, tgBot, exec, cfg.OrderTimeoutSec)
	monitor.Start()

	// ── Start Dashboard ────────────────────────────────
	dash := dashboard.NewServer(store, cfg.DashboardPort)
	dash.Start()

	// ── Initial Coin Fetch ─────────────────────────────
	coins, err := bybitClient.FetchInstruments()
	if err != nil {
		log.Fatalf("Failed to fetch instruments: %v", err)
	}
	log.Printf("Fetched %d USDT perpetual instruments", len(coins))

	// Build blacklist slice for startup message
	blacklistSlice := make([]string, 0, len(cfg.BlacklistedCoins))
	for sym := range cfg.BlacklistedCoins {
		blacklistSlice = append(blacklistSlice, sym)
	}

	// Send startup message
	tgBot.SendStartup(telegram.StartupInfo{
		CoinCount:       len(coins),
		Version:         version,
		TradingEnabled:  cfg.TradingEnabled,
		Testnet:         cfg.BybitTestnet,
		Leverage:        cfg.Leverage,
		MarginPerTrade:  cfg.MarginPerTrade,
		MaxDCACount:     cfg.MaxDCACount,
		DCAThresholdPct: cfg.DCAThresholdPct,
		TP1Pct:          cfg.TP1Pct,
		TP2Pct:          cfg.TP2Pct,
		TP3Pct:          cfg.TP3Pct,
		BlacklistedCoins: blacklistSlice,
		MarginOverrides:  cfg.CoinMarginOverrides,
		DCAOverrides:     cfg.CoinDCAOverrides,
	})

	// ── Signal Tracking ────────────────────────────────
	// Keep track of recently sent signals to avoid duplicates
	recentSignals := make(map[string]time.Time)
	var recentMu sync.Mutex

	// ── Graceful Shutdown ──────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// ── Main Scan Loop ─────────────────────────────────
	scanTicker := time.NewTicker(time.Duration(cfg.ScanIntervalSec) * time.Second)
	defer scanTicker.Stop()

	// Do first scan immediately
	runScan(bybitClient, engine, tgBot, store, cfg, coins, recentSignals, &recentMu, exec)

	for {
		select {
		case sig := <-quit:
			reason := fmt.Sprintf("signal: %s", sig.String())
			log.Printf("Shutting down gracefully: %s", reason)
			monitor.Stop()
			tgBot.SendShutdown(reason)
			return
		case <-scanTicker.C:
			runScan(bybitClient, engine, tgBot, store, cfg, coins, recentSignals, &recentMu, exec)
		}
	}
}

func runScan(
	bybitClient *bybit.Client,
	engine *analysis.Engine,
	tgBot *telegram.Bot,
	store *tracker.Store,
	cfg *config.Config,
	coins []models.Coin,
	recentSignals map[string]time.Time,
	recentMu *sync.Mutex,
	exec *executor.Executor,
) {
	start := time.Now()
	log.Printf("━━━ Scan started (%d coins) ━━━", len(coins))

	// Fetch current tickers for volume filter
	tickers, err := bybitClient.FetchTickers()
	if err != nil {
		log.Printf("Error fetching tickers: %v", err)
		return
	}

	// Filter coins by volume
	var filtered []*models.Coin
	for i := range coins {
		ticker, ok := tickers[coins[i].Symbol]
		if !ok {
			continue
		}
		coins[i].LastPrice = ticker.LastPrice
		coins[i].Turnover24h = ticker.Turnover24h
		coins[i].Volume24h = ticker.Volume24h
		coins[i].FundingRate = ticker.FundingRate
		coins[i].OpenInterest = ticker.OpenInterest
		coins[i].NextFundingTime = ticker.NextFundingTime
		coins[i].FundingInterval = ticker.FundingInterval

		if ticker.Turnover24h >= cfg.MinVolume24H {
			if cfg.IsBlacklisted(coins[i].Symbol) {
				log.Printf("Skipping blacklisted coin: %s", coins[i].Symbol)
				continue
			}
			coin := coins[i] // copy
			filtered = append(filtered, &coin)
		}
	}

	// Sort by volume descending
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Turnover24h > filtered[j].Turnover24h
	})

	log.Printf("Volume filter: %d/%d coins passed ($%.0f+ threshold)",
		len(filtered), len(coins), cfg.MinVolume24H)

	// Semaphore for concurrent analysis (limit to 3 parallel to respect API rate limits)
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var allSignals []*models.Signal
	var signalMu sync.Mutex
	totalToScan := len(filtered)
	var scannedCount int32

	if totalToScan == 0 {
		log.Printf("[Scan] No coins passed volume filter in this cycle")
	}

	for i, coin := range filtered {
		wg.Add(1)
		sem <- struct{}{}
		index := i + 1

		go func(c *models.Coin, scanIndex int) {
			defer wg.Done()
			defer func() { <-sem }()

			coinStart := time.Now()
			signalCount := 0
			log.Printf("[Scan] ▶ %s (%d/%d) analyzing...", c.Symbol, scanIndex, totalToScan)

			defer func() {
				completed := atomic.AddInt32(&scannedCount, 1)
				log.Printf("[Scan] ✓ %s (%d/%d) done in %.1fs, signals=%d, progress=%d/%d",
					c.Symbol, scanIndex, totalToScan, time.Since(coinStart).Seconds(), signalCount, completed, totalToScan)
			}()

			// Analyze
			coinAnalysis := engine.AnalyzeCoin(c)
			if coinAnalysis == nil {
				return
			}

			// Multi-timeframe pattern matching
			mtfResults := analysis.AnalyzeMTF(coinAnalysis)
			if len(mtfResults) == 0 {
				return
			}

			// Generate signals
			tpCfg := signals.TPConfig{
				TP1Pct:                cfg.TP1Pct,
				TP2Pct:                cfg.TP2Pct,
				TP3Pct:                cfg.TP3Pct,
				ATRTP2Mult:            cfg.ATRTP2Mult,
				ATRTP3Mult:            cfg.ATRTP3Mult,
				DCAThresholdPct:       cfg.DCAThresholdPct,
				CoinDCAOverrides:      cfg.CoinDCAOverrides,
				ShortFundingRateLimit: cfg.ShortFundingRateLimit,
			}
			sigs := signals.GenerateSignals(mtfResults, tpCfg)
			if len(sigs) == 0 {
				return
			}
			signalCount = len(sigs)

			signalMu.Lock()
			allSignals = append(allSignals, sigs...)
			signalMu.Unlock()
		}(coin, index)
	}

	wg.Wait()

	// Process signals
	newSignals := 0
	recentMu.Lock()
// Clean old entries (older than 30 minutes)
		for k, t := range recentSignals {
			if time.Since(t) > 30*time.Minute {
			delete(recentSignals, k)
		}
	}

	for _, sig := range allSignals {
		// Dedup key: symbol + direction (same coin same direction = skip)
		// Prevents multiple patterns triggering on the same setup
		key := string(sig.Symbol) + string(sig.Direction)
		if _, exists := recentSignals[key]; exists {
			continue
		}
		recentSignals[key] = time.Now()

		// Send to Telegram
		msg := signals.FormatTelegramMessage(sig)
		if err := tgBot.SendMessage(msg); err != nil {
			log.Printf("Error sending signal: %v", err)
			continue
		}

		// Execute trade on Bybit (if trading enabled)
		var orderID string
		var entryPrice, qty float64

		if exec != nil {
			margin := cfg.MarginForCoin(sig.Symbol)
			orderID, entryPrice, qty, err = exec.OpenPosition(sig, margin)
			if err != nil {
				log.Printf("⚠️ Trade execution failed for %s: %v", sig.Symbol, err)
				tgBot.SendMessage("⚠️ *EMİR HATA* — " + sig.Symbol + "\n" + err.Error())
				continue
			}
		} else {
			// Signal-only mode: use midpoint as entry
			entryPrice = (sig.EntryLow + sig.EntryHigh) / 2
		}

		// Record trade
		trade, err := store.CreateTrade(sig, orderID, entryPrice, qty, cfg.MarginForCoin(sig.Symbol))
		if err != nil {
			log.Printf("Error creating trade: %v", err)
			continue
		}

		log.Printf("🔥 Signal: %s %s %s (Score: %d, Grade: %s, Trade #%d, OrderID: %s)",
			sig.Direction, sig.Symbol, sig.Pattern, sig.Confidence, sig.Grade, trade.ID, orderID)
		newSignals++
	}
	recentMu.Unlock()

	elapsed := time.Since(start)
	log.Printf("━━━ Scan complete: %d signals, %d new (%.1fs) ━━━",
		len(allSignals), newSignals, elapsed.Seconds())
}
