package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	TelegramBotToken string
	TelegramChatID   string
	ScanIntervalSec  int
	MinVolume24H     float64
	DashboardPort    int

	// ── Trading Config ────────────────────────────────
	BybitAPIKey      string
	BybitAPISecret   string
	BybitTestnet     bool    // true = testnet, false = mainnet
	Leverage         int     // e.g. 10
	MarginPerTrade   float64 // USD margin per entry, e.g. 100
	MaxDCACount      int     // max DCA entries per position, e.g. 3
	DCAThresholdPct  float64 // DCA trigger %, e.g. 20.0
	OrderTimeoutSec  int     // cancel unfilled limit orders after N seconds
	TradingEnabled   bool    // master switch for live trading

	// ── TP Configuration ──────────────────────────────
	TP1Pct      float64 // TP1 target %, e.g. 1.0
	TP2Pct      float64 // TP2 target %, e.g. 2.5
	TP3Pct      float64 // TP3 target %, e.g. 5.0


	// ── Funding Rate Filter ───────────────────────────
	ShortFundingRateLimit float64 // skip SHORT if funding < this (e.g. -0.0001)

	// ── Blacklist & Per-Coin Overrides ─────────────────
	BlacklistedCoins    map[string]bool    // COIN_BLACKLIST=BTCUSDT,ETHUSDT
	CoinMarginOverrides map[string]float64 // COIN_MARGIN_OVERRIDES=BTCUSDT:500,ETHUSDT:300
	CoinDCAOverrides    map[string]float64 // COIN_DCA_OVERRIDES=BTCUSDT:3,SOLUSDT:5
}

func Load() (*Config, error) {
	if err := loadEnvFile(".env"); err != nil {
		return nil, fmt.Errorf("failed to load .env: %w", err)
	}

	cfg := &Config{
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:   os.Getenv("TELEGRAM_CHAT_ID"),
		ScanIntervalSec:  getEnvInt("SCAN_INTERVAL_SECONDS", 300),
		MinVolume24H:     getEnvFloat("MIN_VOLUME_24H_USD", 10_000_000),
		DashboardPort:    getEnvInt("DASHBOARD_PORT", 8081),

		// Trading
		BybitAPIKey:     os.Getenv("BYBIT_API_KEY"),
		BybitAPISecret:  os.Getenv("BYBIT_API_SECRET"),
		BybitTestnet:    getEnvBool("BYBIT_TESTNET", false),
		Leverage:        getEnvInt("LEVERAGE", 10),
		MarginPerTrade:  getEnvFloat("MARGIN_PER_TRADE", 100),
		MaxDCACount:     getEnvInt("MAX_DCA_COUNT", 3),
		DCAThresholdPct: getEnvFloat("DCA_THRESHOLD_PCT", 20.0),
		OrderTimeoutSec: getEnvInt("ORDER_TIMEOUT_SEC", 300),
		TradingEnabled:  getEnvBool("TRADING_ENABLED", false),

		// TP Config
		TP1Pct:     getEnvFloat("TP1_PCT", 1.0),
		TP2Pct:     getEnvFloat("TP2_PCT", 2.5),
		TP3Pct:     getEnvFloat("TP3_PCT", 5.0),

		// Funding rate filter
		ShortFundingRateLimit: getEnvFloat("SHORT_FUNDING_RATE_LIMIT", -0.008),

		// Blacklist & per-coin overrides
		BlacklistedCoins:    parseBlacklist(os.Getenv("COIN_BLACKLIST")),
		CoinMarginOverrides: parseOverrides(os.Getenv("COIN_MARGIN_OVERRIDES")),
		CoinDCAOverrides:    parseOverrides(os.Getenv("COIN_DCA_OVERRIDES")),
	}

	if cfg.TelegramBotToken == "" || cfg.TelegramBotToken == "your_bot_token_here" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is not set")
	}
	if cfg.TelegramChatID == "" || cfg.TelegramChatID == "your_chat_id_here" {
		return nil, fmt.Errorf("TELEGRAM_CHAT_ID is not set")
	}
	if cfg.TradingEnabled {
		if cfg.BybitAPIKey == "" {
			return nil, fmt.Errorf("BYBIT_API_KEY is required when TRADING_ENABLED=true")
		}
		if cfg.BybitAPISecret == "" {
			return nil, fmt.Errorf("BYBIT_API_SECRET is required when TRADING_ENABLED=true")
		}
	}

	return cfg, nil
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no .env file is okay, use system env
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
	return scanner.Err()
}

func getEnvInt(key string, def int) int {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return def
	}
	return n
}

func getEnvFloat(key string, def float64) float64 {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return def
	}
	return f
}

func getEnvBool(key string, def bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	switch strings.ToLower(val) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return def
}

// parseBlacklist parses "BTCUSDT,ETHUSDT" into a set
func parseBlacklist(raw string) map[string]bool {
	result := make(map[string]bool)
	if raw == "" {
		return result
	}
	for _, coin := range strings.Split(raw, ",") {
		coin = strings.TrimSpace(strings.ToUpper(coin))
		if coin != "" {
			result[coin] = true
		}
	}
	return result
}

// parseOverrides parses "BTCUSDT:500,ETHUSDT:300" into a map
func parseOverrides(raw string) map[string]float64 {
	result := make(map[string]float64)
	if raw == "" {
		return result
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			continue
		}
		coin := strings.TrimSpace(strings.ToUpper(parts[0]))
		amt, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil || coin == "" || amt <= 0 {
			continue
		}
		result[coin] = amt
	}
	return result
}

// MarginForCoin returns the configured margin for a coin (override or default)
func (c *Config) MarginForCoin(symbol string) float64 {
	if amt, ok := c.CoinMarginOverrides[symbol]; ok {
		return amt
	}
	return c.MarginPerTrade
}

// IsBlacklisted returns true if the coin should be skipped
func (c *Config) IsBlacklisted(symbol string) bool {
	return c.BlacklistedCoins[strings.ToUpper(symbol)]
}

// DCAForCoin returns the DCA threshold % for a coin (override or default)
func (c *Config) DCAForCoin(symbol string) float64 {
	if pct, ok := c.CoinDCAOverrides[symbol]; ok {
		return pct
	}
	return c.DCAThresholdPct
}
