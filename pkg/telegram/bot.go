package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type Bot struct {
	token    string
	chatID   string
	http     *http.Client
	limiter  chan struct{}
	mu       sync.Mutex
}

func NewBot(token, chatID string) *Bot {
	b := &Bot{
		token:   token,
		chatID:  chatID,
		http:    &http.Client{Timeout: 10 * time.Second},
		limiter: make(chan struct{}, 1),
	}
	b.limiter <- struct{}{}
	go func() {
		ticker := time.NewTicker(time.Second / 20) // 20 msg/sec max
		defer ticker.Stop()
		for range ticker.C {
			select {
			case b.limiter <- struct{}{}:
			default:
			}
		}
	}()
	return b
}

func (b *Bot) SendMessage(text string) error {
	<-b.limiter

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)

	payload := map[string]interface{}{
		"chat_id":    b.chatID,
		"text":       text,
		"parse_mode": "Markdown",
		"disable_web_page_preview": true,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	resp, err := b.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// StartupInfo bundles all information shown in the startup Telegram message.
type StartupInfo struct {
	CoinCount        int
	Version          string
	TradingEnabled   bool
	Testnet          bool
	Leverage         int
	MarginPerTrade   float64
	MaxDCACount      int
	DCAThresholdPct  float64
	TP1Pct           float64
	TP2Pct           float64
	TP3Pct           float64
	BlacklistedCoins []string
	MarginOverrides  map[string]float64
	DCAOverrides     map[string]float64
}

func (b *Bot) SendStartup(info StartupInfo) {
	tradingMode := "🔴 Signal-only (trading disabled)"
	if info.TradingEnabled {
		env := "Mainnet"
		if info.Testnet {
			env = "Testnet"
		}
		tradingMode = fmt.Sprintf("🟢 Live trading ON (%s)", env)
	}

	blacklistStr := "none"
	if len(info.BlacklistedCoins) > 0 {
		blacklistStr = ""
		for i, c := range info.BlacklistedCoins {
			if i > 0 {
				blacklistStr += ", "
			}
			blacklistStr += c
		}
	}

	overridesStr := "none"
	if len(info.MarginOverrides) > 0 {
		overridesStr = ""
		first := true
		for sym, m := range info.MarginOverrides {
			if !first {
				overridesStr += ", "
			}
			overridesStr += fmt.Sprintf("%s:$%.0f", sym, m)
			first = false
		}
	}

	dcaOverridesStr := "none"
	if len(info.DCAOverrides) > 0 {
		dcaOverridesStr = ""
		first := true
		for sym, pct := range info.DCAOverrides {
			if !first {
				dcaOverridesStr += ", "
			}
			dcaOverridesStr += fmt.Sprintf("%s:%.1f%%", sym, pct)
			first = false
		}
	}

	msg := fmt.Sprintf(
		"🤖 *TRADER BOT BAŞLADI*\n\n"+
			"📊 Taranan coin: %d\n"+
			"⏰ Tarama aralığı: 5 dakika\n"+
			"📈 Timeframe'ler: 5m, 15m, 1h, 4h\n"+
			"📡 Veri kaynakları: Bybit + Binance\n"+
			"🌐 Dashboard hazır\n"+
			"🔄 Versiyon: %s\n\n"+
			"⚙️ *Trading Ayarları*\n"+
			"├ Mod: %s\n"+
			"├ Kaldıraç: %dx\n"+
			"├ Varsayılan margin: $%.0f\n"+
			"├ Max DCA: %d\n"+
			"├ DCA eşiği: %.1f%%\n"+
			"├ TP1/TP2/TP3: %.1f%% / %.1f%% / %.1f%%\n"+
			"├ Kara liste: %s\n"+
			"├ Coin margin override: %s\n"+
			"└ Coin DCA override: %s\n\n"+
			"✅ Sistem hazır, tarama başlıyor...",
		info.CoinCount, info.Version,
		tradingMode,
		info.Leverage, info.MarginPerTrade,
		info.MaxDCACount, info.DCAThresholdPct,
		info.TP1Pct, info.TP2Pct, info.TP3Pct,
		blacklistStr, overridesStr, dcaOverridesStr,
	)

	if err := b.SendMessage(msg); err != nil {
		log.Printf("Failed to send startup message: %v", err)
	}
}

func (b *Bot) SendShutdown(reason string) {
	if reason == "" {
		reason = "bilinmiyor"
	}

	msg := fmt.Sprintf("🔴 *TRADER BOT KAPANDI*\n\nSebep: %s", reason)
	if err := b.SendMessage(msg); err != nil {
		log.Printf("Failed to send shutdown message: %v", err)
	}
}

// SendDiagnostic is kept for backward compatibility.
func (b *Bot) SendDiagnostic(coinCount int, version string) {
	b.SendStartup(StartupInfo{
		CoinCount: coinCount,
		Version:   version,
	})
}
