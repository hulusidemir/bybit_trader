package okx

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
	baseURL   = "https://www.okx.com"
	rateLimit = 10
)

// Client is an OKX exchange data client implementing exchange.DataProvider.
type Client struct {
	http        *http.Client
	limiter     chan struct{}
	swapSymbols map[string]bool
	spotSymbols map[string]bool
	mu          sync.RWMutex
}

func NewClient() *Client {
	c := &Client{
		http:        &http.Client{Timeout: 15 * time.Second},
		limiter:     make(chan struct{}, rateLimit),
		swapSymbols: make(map[string]bool),
		spotSymbols: make(map[string]bool),
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
	// Fetch SWAP tickers
	data, err := c.doGet("/api/v5/market/tickers", map[string]string{"instType": "SWAP"})
	if err != nil {
		log.Printf("[OKX] Failed to load swap symbols: %v", err)
		return
	}
	var swapResp []struct {
		InstID string `json:"instId"`
	}
	if err := json.Unmarshal(data, &swapResp); err != nil {
		log.Printf("[OKX] Failed to parse swap tickers: %v", err)
		return
	}
	c.mu.Lock()
	for _, t := range swapResp {
		c.swapSymbols[t.InstID] = true
	}
	c.mu.Unlock()

	// Fetch SPOT tickers
	data, err = c.doGet("/api/v5/market/tickers", map[string]string{"instType": "SPOT"})
	if err != nil {
		log.Printf("[OKX] Failed to load spot symbols: %v", err)
		return
	}
	var spotResp []struct {
		InstID string `json:"instId"`
	}
	if err := json.Unmarshal(data, &spotResp); err != nil {
		log.Printf("[OKX] Failed to parse spot tickers: %v", err)
		return
	}
	c.mu.Lock()
	for _, t := range spotResp {
		c.spotSymbols[t.InstID] = true
	}
	c.mu.Unlock()

	log.Printf("[OKX] Loaded %d swap symbols, %d spot symbols", len(c.swapSymbols), len(c.spotSymbols))
}

// ── HTTP ────────────────────────────────────────────────────

func (c *Client) doGet(endpoint string, params map[string]string) (json.RawMessage, error) {
	<-c.limiter

	url := baseURL + endpoint
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()

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
	if result.Code != "0" {
		return nil, fmt.Errorf("OKX error %s: %s", result.Code, result.Msg)
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

func bybitToSwapInstID(symbol string) string {
	base := extractBase(symbol)
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	return base + "-USDT-SWAP"
}

func bybitToSpotInstID(symbol string) string {
	base := extractBase(symbol)
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	return base + "-USDT"
}

func bybitToCcy(symbol string) string {
	base := extractBase(symbol)
	if strings.HasPrefix(base, "10000") {
		base = base[5:]
	} else if strings.HasPrefix(base, "1000") {
		base = base[4:]
	}
	return base
}

func (c *Client) hasSwapSymbol(instID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.swapSymbols[instID]
}

// ── Internal API Methods ────────────────────────────────────

// OKX rubik endpoints only support: 5m, 1H, 1D
var okxPeriodMap = map[string]string{
	"5":  "5m",
	"60": "1H",
}

func (c *Client) fetchOIHistory(ccy, period string) ([]models.OpenInterestPoint, error) {
	data, err := c.doGet("/api/v5/rubik/stat/contracts/open-interest-volume", map[string]string{
		"ccy":    ccy,
		"period": period,
	})
	if err != nil {
		return nil, err
	}

	var rows [][]string
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("unmarshal OI: %w", err)
	}

	points := make([]models.OpenInterestPoint, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if len(row) < 2 {
			continue
		}
		ts, _ := strconv.ParseInt(row[0], 10, 64)
		oi, _ := strconv.ParseFloat(row[1], 64)
		points = append(points, models.OpenInterestPoint{
			Timestamp:    ts,
			OpenInterest: oi,
		})
	}

	return points, nil
}

func (c *Client) fetchTakerVolume(ccy, instType, period string) ([]models.TakerVolume, error) {
	data, err := c.doGet("/api/v5/rubik/stat/taker-volume", map[string]string{
		"ccy":      ccy,
		"instType": instType,
		"period":   period,
	})
	if err != nil {
		return nil, err
	}

	var rows [][]string
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("unmarshal taker vol: %w", err)
	}

	result := make([]models.TakerVolume, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if len(row) < 3 {
			continue
		}
		ts, _ := strconv.ParseInt(row[0], 10, 64)
		sellVol, _ := strconv.ParseFloat(row[1], 64)
		buyVol, _ := strconv.ParseFloat(row[2], 64)

		ratio := 0.0
		if sellVol > 0 {
			ratio = buyVol / sellVol
		}

		result = append(result, models.TakerVolume{
			Timestamp:    ts,
			BuyVolume:    buyVol,
			SellVolume:   sellVol,
			BuySellRatio: ratio,
		})
	}

	return result, nil
}

func (c *Client) fetchOrderbook(instID string, depth int) (*models.OrderbookSnapshot, error) {
	data, err := c.doGet("/api/v5/market/books", map[string]string{
		"instId": instID,
		"sz":     strconv.Itoa(depth),
	})
	if err != nil {
		return nil, err
	}

	var books []struct {
		Asks [][]string `json:"asks"`
		Bids [][]string `json:"bids"`
		Ts   string     `json:"ts"`
	}
	if err := json.Unmarshal(data, &books); err != nil {
		return nil, fmt.Errorf("unmarshal orderbook: %w", err)
	}
	if len(books) == 0 {
		return nil, fmt.Errorf("empty orderbook response")
	}

	book := books[0]
	ts, _ := strconv.ParseInt(book.Ts, 10, 64)

	ob := &models.OrderbookSnapshot{
		Symbol:    instID,
		Timestamp: ts,
		Bids:      make([]models.OrderbookLevel, 0, len(book.Bids)),
		Asks:      make([]models.OrderbookLevel, 0, len(book.Asks)),
	}

	for _, b := range book.Bids {
		if len(b) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(b[0], 64)
		size, _ := strconv.ParseFloat(b[1], 64)
		ob.Bids = append(ob.Bids, models.OrderbookLevel{Price: price, Amount: size})
	}
	for _, a := range book.Asks {
		if len(a) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(a[0], 64)
		size, _ := strconv.ParseFloat(a[1], 64)
		ob.Asks = append(ob.Asks, models.OrderbookLevel{Price: price, Amount: size})
	}

	return ob, nil
}

// ── DataProvider Interface ──────────────────────────────────

func (c *Client) Name() string { return "okx" }

func (c *Client) SupportsSymbol(bybitSymbol string) bool {
	instID := bybitToSwapInstID(bybitSymbol)
	return c.hasSwapSymbol(instID)
}

func (c *Client) FetchOI(bybitSymbol, tfKey string, limit int) ([]models.OpenInterestPoint, error) {
	period, ok := okxPeriodMap[tfKey]
	if !ok {
		return nil, nil
	}
	return c.fetchOIHistory(bybitToCcy(bybitSymbol), period)
}

func (c *Client) FetchPerpTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error) {
	period, ok := okxPeriodMap[tfKey]
	if !ok {
		return nil, nil
	}
	return c.fetchTakerVolume(bybitToCcy(bybitSymbol), "SWAP", period)
}

func (c *Client) FetchSpotTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error) {
	period, ok := okxPeriodMap[tfKey]
	if !ok {
		return nil, nil
	}
	return c.fetchTakerVolume(bybitToCcy(bybitSymbol), "SPOT", period)
}

func (c *Client) FetchFuturesOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	instID := bybitToSwapInstID(bybitSymbol)
	if !c.hasSwapSymbol(instID) {
		return nil, nil
	}
	return c.fetchOrderbook(instID, depth)
}

func (c *Client) FetchSpotOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	instID := bybitToSpotInstID(bybitSymbol)
	c.mu.RLock()
	has := c.spotSymbols[instID]
	c.mu.RUnlock()
	if !has {
		return nil, nil
	}
	return c.fetchOrderbook(instID, depth)
}
