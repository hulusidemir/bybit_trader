package coinbase

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
	baseURL   = "https://api.exchange.coinbase.com"
	rateLimit = 8
)

// Client is a Coinbase Exchange (formerly Coinbase Pro) data client.
// Provides spot orderbook depth for aggregation.
type Client struct {
	http        *http.Client
	limiter     chan struct{}
	spotProducts map[string]bool // "BTC-USDT" => true
	mu          sync.RWMutex
}

func NewClient() *Client {
	c := &Client{
		http:         &http.Client{Timeout: 15 * time.Second},
		limiter:      make(chan struct{}, rateLimit),
		spotProducts: make(map[string]bool),
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

	c.loadProducts()
	return c
}

func (c *Client) loadProducts() {
	data, err := c.doGet("/products", nil)
	if err != nil {
		log.Printf("[Coinbase] Failed to load products: %v", err)
		return
	}

	var products []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &products); err != nil {
		log.Printf("[Coinbase] Failed to parse products: %v", err)
		return
	}

	c.mu.Lock()
	for _, p := range products {
		if p.Status == "online" && (strings.HasSuffix(p.ID, "-USDT") || strings.HasSuffix(p.ID, "-USD")) {
			c.spotProducts[p.ID] = true
		}
	}
	c.mu.Unlock()

	log.Printf("[Coinbase] Loaded %d spot products", len(c.spotProducts))
}

// ── HTTP ────────────────────────────────────────────────────

func (c *Client) doGet(endpoint string, params map[string]string) (json.RawMessage, error) {
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

// ── Symbol Conversion ───────────────────────────────────────

func extractBase(symbol string) string {
	if strings.HasSuffix(symbol, "USDT") {
		return symbol[:len(symbol)-4]
	}
	return symbol
}

func bybitToProductID(symbol string) string {
	base := extractBase(symbol)
	// Strip 1000/10000 prefix — Coinbase uses real symbols
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	return base + "-USDT"
}

func (c *Client) resolveProductID(symbol string) string {
	// Try USDT pair first, fall back to USD
	id := bybitToProductID(symbol)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.spotProducts[id] {
		return id
	}
	// Try USD pair
	base := extractBase(symbol)
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	usdID := base + "-USD"
	if c.spotProducts[usdID] {
		return usdID
	}
	return ""
}

// ── DataProvider Interface ──────────────────────────────────

func (c *Client) Name() string { return "coinbase" }

func (c *Client) SupportsSymbol(bybitSymbol string) bool {
	return c.resolveProductID(bybitSymbol) != ""
}

// FetchOI returns nil — Coinbase has no futures.
func (c *Client) FetchOI(_ string, _ string, _ int) ([]models.OpenInterestPoint, error) {
	return nil, nil
}

// FetchPerpTakerVolume returns nil — Coinbase has no futures.
func (c *Client) FetchPerpTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

// FetchSpotTakerVolume returns nil — Coinbase klines don't have taker buy/sell split.
func (c *Client) FetchSpotTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

// FetchOrderbook returns spot orderbook depth from Coinbase.
func (c *Client) FetchOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	productID := c.resolveProductID(bybitSymbol)
	if productID == "" {
		return nil, nil
	}

	// Coinbase depth levels: 1, 50, full. Use level 2 (top 50).
	level := "2"
	data, err := c.doGet("/products/"+productID+"/book", map[string]string{"level": level})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Bids    [][]json.RawMessage `json:"bids"` // [price, size, num_orders]
		Asks    [][]json.RawMessage `json:"asks"`
		Auction json.RawMessage     `json:"auction"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal orderbook: %w", err)
	}

	ob := &models.OrderbookSnapshot{
		Symbol:    productID,
		Timestamp: time.Now().UnixMilli(),
		Bids:      make([]models.OrderbookLevel, 0, len(resp.Bids)),
		Asks:      make([]models.OrderbookLevel, 0, len(resp.Asks)),
	}

	parseLevel := func(row []json.RawMessage) (float64, float64) {
		if len(row) < 2 {
			return 0, 0
		}
		var priceStr, sizeStr string
		json.Unmarshal(row[0], &priceStr)
		json.Unmarshal(row[1], &sizeStr)
		price, _ := strconv.ParseFloat(priceStr, 64)
		size, _ := strconv.ParseFloat(sizeStr, 64)
		return price, size
	}

	for _, b := range resp.Bids {
		price, size := parseLevel(b)
		if price > 0 {
			ob.Bids = append(ob.Bids, models.OrderbookLevel{Price: price, Amount: size})
		}
	}
	for _, a := range resp.Asks {
		price, size := parseLevel(a)
		if price > 0 {
			ob.Asks = append(ob.Asks, models.OrderbookLevel{Price: price, Amount: size})
		}
	}

	return ob, nil
}

// FetchLSRatio returns nil — Coinbase has no futures L/S data.
func (c *Client) FetchLSRatio(_ string, _ string, _ int) ([]models.LongShortRatio, error) {
	return nil, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
