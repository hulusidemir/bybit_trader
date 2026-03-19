package kraken

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"bybit_trader/pkg/models"
)

const (
	baseURL   = "https://api.kraken.com"
	rateLimit = 5
)

// Client is a Kraken exchange data client providing spot orderbook data.
type Client struct {
	http        *http.Client
	limiter     chan struct{}
	pairMap     map[string]string // bybit base coin -> kraken pair name
	mu          sync.RWMutex
}

func NewClient() *Client {
	c := &Client{
		http:    &http.Client{Timeout: 15 * time.Second},
		limiter: make(chan struct{}, rateLimit),
		pairMap: make(map[string]string),
	}
	for i := 0; i < rateLimit; i++ {
		c.limiter <- struct{}{}
	}
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(rateLimit))
		defer ticker.Stop()
		for range ticker.C {
			select {
			case c.limiter <- struct{}{}:
			default:
			}
		}
	}()

	c.loadPairs()
	return c
}

func (c *Client) loadPairs() {
	body, err := c.doGet("/0/public/AssetPairs", nil)
	if err != nil {
		log.Printf("[Kraken] Failed to load pairs: %v", err)
		return
	}

	var resp struct {
		Error  []string                       `json:"error"`
		Result map[string]json.RawMessage     `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		log.Printf("[Kraken] Failed to parse pairs: %v", err)
		return
	}

	count := 0
	c.mu.Lock()
	for pairName, raw := range resp.Result {
		var info struct {
			WSName string `json:"wsname"`
			Quote  string `json:"quote"`
			Base   string `json:"base"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		// We want USDT or USD quote pairs
		if info.Quote != "ZUSDT" && info.Quote != "ZUSD" && info.Quote != "USDT" && info.Quote != "USD" {
			continue
		}
		// Normalize base: XXBT -> BTC, XETH -> ETH, etc.
		base := normalizeKrakenBase(info.Base)
		if base == "" {
			continue
		}
		// Build bybit symbol key (e.g., "BTC")
		// Prefer USDT pairs over USD
		existing, exists := c.pairMap[base]
		if !exists || (!strings.Contains(existing, "USDT") && strings.Contains(info.Quote, "USDT")) {
			c.pairMap[base] = pairName
			count++
		}
	}
	c.mu.Unlock()
	log.Printf("[Kraken] Loaded %d trading pairs", count)
}

func normalizeKrakenBase(base string) string {
	// Kraken uses X-prefix for some coins: XXBT, XETH, XLTC, etc.
	switch base {
	case "XXBT", "XBT":
		return "BTC"
	case "XETH":
		return "ETH"
	case "XLTC":
		return "LTC"
	case "XXRP":
		return "XRP"
	case "XXLM":
		return "XLM"
	case "XZEC":
		return "ZEC"
	case "XXMR":
		return "XMR"
	}
	// Strip leading X if 4+ chars (Kraken convention)
	if len(base) >= 4 && base[0] == 'X' {
		return base[1:]
	}
	return base
}

func (c *Client) doGet(endpoint string, params map[string]string) ([]byte, error) {
	<-c.limiter

	url := baseURL + endpoint
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "BybitTrader/1.0")

	if params != nil {
		q := req.URL.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}

	return body, nil
}

// resolveKrakenPair converts a Bybit symbol (e.g., "BTCUSDT") to a Kraken pair name.
func (c *Client) resolveKrakenPair(bybitSymbol string) string {
	base := extractBase(bybitSymbol)
	// Strip 1000/10000 prefix
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pairMap[base]
}

func extractBase(symbol string) string {
	if strings.HasSuffix(symbol, "USDT") {
		return symbol[:len(symbol)-4]
	}
	return symbol
}

// ── DataProvider Interface ──────────────────────────────────

func (c *Client) Name() string { return "kraken" }

func (c *Client) SupportsSymbol(bybitSymbol string) bool {
	return c.resolveKrakenPair(bybitSymbol) != ""
}

func (c *Client) FetchOI(_ string, _ string, _ int) ([]models.OpenInterestPoint, error) {
	return nil, nil
}

func (c *Client) FetchPerpTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

func (c *Client) FetchSpotTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

func (c *Client) FetchFuturesOrderbook(_ string, _ int) (*models.OrderbookSnapshot, error) {
	return nil, nil
}

func (c *Client) FetchSpotOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	pair := c.resolveKrakenPair(bybitSymbol)
	if pair == "" {
		return nil, nil
	}

	body, err := c.doGet("/0/public/Depth", map[string]string{
		"pair":  pair,
		"count": strconv.Itoa(depth),
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Error  []string                   `json:"error"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal orderbook: %w", err)
	}
	if len(resp.Error) > 0 {
		return nil, fmt.Errorf("kraken error: %s", resp.Error[0])
	}

	// Result has one key (the pair name) with the orderbook
	for _, raw := range resp.Result {
		var book struct {
			Asks [][]json.RawMessage `json:"asks"`
			Bids [][]json.RawMessage `json:"bids"`
		}
		if err := json.Unmarshal(raw, &book); err != nil {
			return nil, fmt.Errorf("unmarshal book: %w", err)
		}

		ob := &models.OrderbookSnapshot{
			Symbol:    pair,
			Timestamp: time.Now().UnixMilli(),
			Bids:      make([]models.OrderbookLevel, 0, len(book.Bids)),
			Asks:      make([]models.OrderbookLevel, 0, len(book.Asks)),
		}

		parseLevel := func(row []json.RawMessage) (float64, float64) {
			if len(row) < 2 {
				return 0, 0
			}
			var ps, ss string
			json.Unmarshal(row[0], &ps)
			json.Unmarshal(row[1], &ss)
			p, _ := strconv.ParseFloat(ps, 64)
			s, _ := strconv.ParseFloat(ss, 64)
			return p, s
		}

		for _, b := range book.Bids {
			price, size := parseLevel(b)
			if price > 0 {
				ob.Bids = append(ob.Bids, models.OrderbookLevel{Price: price, Amount: size})
			}
		}
		for _, a := range book.Asks {
			price, size := parseLevel(a)
			if price > 0 {
				ob.Asks = append(ob.Asks, models.OrderbookLevel{Price: price, Amount: size})
			}
		}

		return ob, nil
	}

	return nil, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
