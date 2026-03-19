package bitget

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
	baseURL   = "https://api.bitget.com"
	rateLimit = 10
)

// Client is a Bitget exchange data client.
// Provides futures OI, futures orderbook, spot orderbook, futures taker volume, and L/S ratio.
type Client struct {
	http           *http.Client
	limiter        chan struct{}
	futuresSymbols map[string]bool // "BTCUSDT" => true (mix/USDT-FUTURES)
	spotSymbols    map[string]bool // "BTCUSDT" => true (spot)
	mu             sync.RWMutex
}

func NewClient() *Client {
	c := &Client{
		http:           &http.Client{Timeout: 15 * time.Second},
		limiter:        make(chan struct{}, rateLimit),
		futuresSymbols: make(map[string]bool),
		spotSymbols:    make(map[string]bool),
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

	c.loadSymbols()
	return c
}

func (c *Client) loadSymbols() {
	// Load futures (USDT-FUTURES) symbols
	data, err := c.doGet("/api/v2/mix/market/tickers", map[string]string{
		"productType": "USDT-FUTURES",
	})
	if err != nil {
		log.Printf("[Bitget] Failed to load futures symbols: %v", err)
	} else {
		var tickers []struct {
			Symbol string `json:"symbol"`
		}
		if err := json.Unmarshal(data, &tickers); err != nil {
			log.Printf("[Bitget] Failed to parse futures tickers: %v", err)
		} else {
			c.mu.Lock()
			for _, t := range tickers {
				c.futuresSymbols[t.Symbol] = true
			}
			c.mu.Unlock()
		}
	}

	// Load spot symbols
	spotData, err := c.doGet("/api/v2/spot/market/tickers", nil)
	if err != nil {
		log.Printf("[Bitget] Failed to load spot symbols: %v", err)
	} else {
		var spotTickers []struct {
			Symbol string `json:"symbol"`
		}
		if err := json.Unmarshal(spotData, &spotTickers); err != nil {
			log.Printf("[Bitget] Failed to parse spot tickers: %v", err)
		} else {
			c.mu.Lock()
			for _, t := range spotTickers {
				if strings.HasSuffix(t.Symbol, "USDT") {
					c.spotSymbols[t.Symbol] = true
				}
			}
			c.mu.Unlock()
		}
	}

	log.Printf("[Bitget] Loaded %d futures, %d spot symbols", len(c.futuresSymbols), len(c.spotSymbols))
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

	var result struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if result.Code != "00000" {
		return nil, fmt.Errorf("bitget error %s: %s", result.Code, result.Msg)
	}

	return result.Data, nil
}

// ── Symbol Conversion ───────────────────────────────────────

func extractBase(symbol string) string {
	if strings.HasSuffix(symbol, "USDT") {
		return symbol[:len(symbol)-4]
	}
	return symbol
}

// bybitToFuturesSymbol converts Bybit symbol for Bitget futures.
// Bitget uses same naming: BTCUSDT, 1000PEPEUSDT, etc.
func bybitToFuturesSymbol(bybitSymbol string) string {
	return bybitSymbol
}

// bybitToSpotSymbol converts Bybit perp symbol to Bitget spot.
// Strip 1000/10000 prefix for spot markets.
func bybitToSpotSymbol(bybitSymbol string) string {
	base := extractBase(bybitSymbol)
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	return base + "USDT"
}

func (c *Client) hasFuturesSymbol(sym string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.futuresSymbols[sym]
}

func (c *Client) hasSpotSymbol(sym string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.spotSymbols[sym]
}

// ── DataProvider Interface ──────────────────────────────────

func (c *Client) Name() string { return "bitget" }

func (c *Client) SupportsSymbol(bybitSymbol string) bool {
	sym := bybitToFuturesSymbol(bybitSymbol)
	return c.hasFuturesSymbol(sym)
}

func (c *Client) FetchOI(bybitSymbol, tfKey string, limit int) ([]models.OpenInterestPoint, error) {
	sym := bybitToFuturesSymbol(bybitSymbol)
	if !c.hasFuturesSymbol(sym) {
		return nil, nil
	}

	data, err := c.doGet("/api/v2/mix/market/open-interest", map[string]string{
		"productType": "USDT-FUTURES",
		"symbol":      sym,
	})
	if err != nil {
		return nil, err
	}

	// Bitget returns a single OI snapshot, not historical.
	// We return it as a single-point slice.
	var resp struct {
		Symbol       string `json:"symbol"`
		OpenInterest string `json:"openInterest"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		// Might be a list
		var list []struct {
			Symbol       string `json:"symbol"`
			OpenInterest string `json:"openInterest"`
		}
		if err2 := json.Unmarshal(data, &list); err2 != nil {
			return nil, fmt.Errorf("unmarshal OI: %w", err)
		}
		if len(list) == 0 {
			return nil, nil
		}
		oi, _ := strconv.ParseFloat(list[0].OpenInterest, 64)
		return []models.OpenInterestPoint{{
			Timestamp:    time.Now().UnixMilli(),
			OpenInterest: oi,
		}}, nil
	}

	oi, _ := strconv.ParseFloat(resp.OpenInterest, 64)
	return []models.OpenInterestPoint{{
		Timestamp:    time.Now().UnixMilli(),
		OpenInterest: oi,
	}}, nil
}

func (c *Client) FetchPerpTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

func (c *Client) FetchSpotTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

func (c *Client) FetchFuturesOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	sym := bybitToFuturesSymbol(bybitSymbol)
	if !c.hasFuturesSymbol(sym) {
		return nil, nil
	}

	data, err := c.doGet("/api/v2/mix/market/merge-depth", map[string]string{
		"productType": "USDT-FUTURES",
		"symbol":      sym,
		"limit":       strconv.Itoa(depth),
	})
	if err != nil {
		return nil, err
	}

	return c.parseOrderbook(data, sym)
}

func (c *Client) FetchSpotOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	sym := bybitToSpotSymbol(bybitSymbol)
	if !c.hasSpotSymbol(sym) {
		return nil, nil
	}

	data, err := c.doGet("/api/v2/spot/market/merge-depth", map[string]string{
		"symbol": sym,
		"limit":  strconv.Itoa(depth),
	})
	if err != nil {
		return nil, err
	}

	return c.parseOrderbook(data, sym)
}

// parseOrderbook parses Bitget's orderbook format: {"asks":[["price","size"]], "bids":[["price","size"]], "ts":"..."}
func (c *Client) parseOrderbook(data json.RawMessage, symbol string) (*models.OrderbookSnapshot, error) {
	var resp struct {
		Asks [][]string `json:"asks"`
		Bids [][]string `json:"bids"`
		Ts   string     `json:"ts"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal orderbook: %w", err)
	}

	ts, _ := strconv.ParseInt(resp.Ts, 10, 64)
	ob := &models.OrderbookSnapshot{
		Symbol:    symbol,
		Timestamp: ts,
		Bids:      make([]models.OrderbookLevel, 0, len(resp.Bids)),
		Asks:      make([]models.OrderbookLevel, 0, len(resp.Asks)),
	}

	for _, b := range resp.Bids {
		if len(b) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(b[0], 64)
		size, _ := strconv.ParseFloat(b[1], 64)
		if price > 0 {
			ob.Bids = append(ob.Bids, models.OrderbookLevel{Price: price, Amount: size})
		}
	}
	for _, a := range resp.Asks {
		if len(a) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(a[0], 64)
		size, _ := strconv.ParseFloat(a[1], 64)
		if price > 0 {
			ob.Asks = append(ob.Asks, models.OrderbookLevel{Price: price, Amount: size})
		}
	}

	return ob, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
