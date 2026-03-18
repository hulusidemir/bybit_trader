package bybit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ════════════════════════════════════════════════════════════
// TradeClient — Authenticated Bybit V5 Trade API
// Handles order placement, position management, leverage
// ════════════════════════════════════════════════════════════

type TradeClient struct {
	httpClient *http.Client
	apiKey     string
	apiSecret  string
	baseURL    string
	limiter    chan struct{}

	// Instrument precision cache
	instruments   map[string]*InstrumentDetail
	instrumentsMu sync.RWMutex

	// Position mode: always hedge (two-way)
	hedgeMode bool
}

type InstrumentDetail struct {
	Symbol      string
	MinOrderQty float64
	MaxOrderQty float64
	QtyStep     float64
	TickSize    float64
}

type PlaceOrderReq struct {
	Symbol      string
	Side        string // "Buy" or "Sell"
	OrderType   string // "Limit" or "Market"
	Qty         string
	Price       string // required for Limit
	TimeInForce string // "GTC", "IOC", "PostOnly"
	ReduceOnly  bool
}

type OrderResult struct {
	OrderID     string
	OrderLinkID string
}

type OrderDetail struct {
	OrderID      string
	Symbol       string
	Side         string
	OrderType    string
	Price        string
	Qty          string
	CumExecQty   string
	CumExecValue string
	AvgPrice     string
	OrderStatus  string // "New", "PartiallyFilled", "Filled", "Cancelled", "Rejected"
	CreatedTime  string
}

type PositionDetail struct {
	Symbol        string
	Side          string // "Buy", "Sell", "None"
	Size          string
	AvgPrice      string
	PositionValue string
	UnrealisedPnl string
	Leverage      string
	LiqPrice      string
}

func NewTradeClient(apiKey, apiSecret string, testnet bool) *TradeClient {
	base := "https://api.bybit.com"
	if testnet {
		base = "https://api-testnet.bybit.com"
	}

	tc := &TradeClient{
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		apiKey:      apiKey,
		apiSecret:   apiSecret,
		baseURL:     base,
		limiter:     make(chan struct{}, 5),
		instruments: make(map[string]*InstrumentDetail),
		hedgeMode:   true,
	}

	for i := 0; i < 5; i++ {
		tc.limiter <- struct{}{}
	}
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			select {
			case tc.limiter <- struct{}{}:
			default:
			}
		}
	}()

	return tc
}

// ── Authentication ──────────────────────────────────────────

func (tc *TradeClient) sign(timestamp, payload string) string {
	h := hmac.New(sha256.New, []byte(tc.apiSecret))
	h.Write([]byte(timestamp + tc.apiKey + "5000" + payload))
	return hex.EncodeToString(h.Sum(nil))
}

// ErrRateLimit is returned when Bybit responds with HTTP 429 Too Many Requests.
var ErrRateLimit = fmt.Errorf("rate limited (HTTP 429)")

func (tc *TradeClient) doPost(endpoint string, body map[string]interface{}) (json.RawMessage, error) {
	<-tc.limiter

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig := tc.sign(ts, bodyStr)

	req, err := http.NewRequest("POST", tc.baseURL+endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BAPI-API-KEY", tc.apiKey)
	req.Header.Set("X-BAPI-TIMESTAMP", ts)
	req.Header.Set("X-BAPI-SIGN", sig)
	req.Header.Set("X-BAPI-RECV-WINDOW", "5000")

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// HTTP 429 — rate limited, signal caller to retry
	if resp.StatusCode == http.StatusTooManyRequests {
		io.ReadAll(resp.Body) // drain body
		return nil, ErrRateLimit
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if len(respBody) == 0 {
		return nil, fmt.Errorf("empty response body (HTTP %d) for %s", resp.StatusCode, endpoint)
	}

	var result struct {
		RetCode int             `json:"retCode"`
		RetMsg  string          `json:"retMsg"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w (body: %s)", err, string(respBody[:min(len(respBody), 200)]))
	}
	if result.RetCode != 0 {
		return nil, fmt.Errorf("API error %d: %s (endpoint: %s)", result.RetCode, result.RetMsg, endpoint)
	}

	return result.Result, nil
}

// doPostWithRetry wraps doPost with exponential backoff retry.
// Retries on network errors and HTTP 429 (rate limit). Max 3 attempts.
// Backoff: 500ms → 1s → 2s.
func (tc *TradeClient) doPostWithRetry(endpoint string, body map[string]interface{}) (json.RawMessage, error) {
	backoff := [3]time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := tc.doPost(endpoint, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		// Don't retry on deterministic API errors (retCode != 0)
		// Only retry on rate limits, network failures, empty/corrupt responses
		if err != ErrRateLimit &&
			!strings.Contains(err.Error(), "request failed:") &&
			!strings.Contains(err.Error(), "empty response body") {
			return nil, err
		}
		log.Printf("[Trade] ⚠️ %s attempt %d/3 failed: %v — retrying in %v",
			endpoint, attempt+1, err, backoff[attempt])
		time.Sleep(backoff[attempt])
	}
	return nil, fmt.Errorf("%s after 3 attempts: %w", endpoint, lastErr)
}

func (tc *TradeClient) doAuthGet(endpoint string, params map[string]string) (json.RawMessage, error) {
	<-tc.limiter

	vals := url.Values{}
	for k, v := range params {
		vals.Set(k, v)
	}
	queryStr := vals.Encode()

	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig := tc.sign(ts, queryStr)

	reqURL := tc.baseURL + endpoint
	if queryStr != "" {
		reqURL += "?" + queryStr
	}

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-BAPI-API-KEY", tc.apiKey)
	req.Header.Set("X-BAPI-TIMESTAMP", ts)
	req.Header.Set("X-BAPI-SIGN", sig)
	req.Header.Set("X-BAPI-RECV-WINDOW", "5000")

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result struct {
		RetCode int             `json:"retCode"`
		RetMsg  string          `json:"retMsg"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if result.RetCode != 0 {
		return nil, fmt.Errorf("API error %d: %s", result.RetCode, result.RetMsg)
	}

	return result.Result, nil
}

// ── Instrument Info (Precision Cache) ──────────────────────

func (tc *TradeClient) LoadInstruments() error {
	log.Println("[Trade] Loading instrument precision data...")
	cursor := ""

	for {
		params := map[string]string{
			"category": "linear",
			"limit":    "1000",
			"status":   "Trading",
		}
		if cursor != "" {
			params["cursor"] = cursor
		}

		reqURL := tc.baseURL + "/v5/market/instruments-info"
		req, _ := http.NewRequest("GET", reqURL, nil)
		q := req.URL.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()

		resp, err := tc.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("fetch instruments: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}

		var result struct {
			RetCode int    `json:"retCode"`
			RetMsg  string `json:"retMsg"`
			Result  struct {
				List []struct {
					Symbol        string `json:"symbol"`
					LotSizeFilter struct {
						MinOrderQty string `json:"minOrderQty"`
						MaxOrderQty string `json:"maxOrderQty"`
						QtyStep     string `json:"qtyStep"`
					} `json:"lotSizeFilter"`
					PriceFilter struct {
						TickSize string `json:"tickSize"`
					} `json:"priceFilter"`
				} `json:"list"`
				NextPageCursor string `json:"nextPageCursor"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("unmarshal instruments: %w", err)
		}

		tc.instrumentsMu.Lock()
		for _, item := range result.Result.List {
			minQty, _ := strconv.ParseFloat(item.LotSizeFilter.MinOrderQty, 64)
			maxQty, _ := strconv.ParseFloat(item.LotSizeFilter.MaxOrderQty, 64)
			qtyStep, _ := strconv.ParseFloat(item.LotSizeFilter.QtyStep, 64)
			tickSize, _ := strconv.ParseFloat(item.PriceFilter.TickSize, 64)
			tc.instruments[item.Symbol] = &InstrumentDetail{
				Symbol:      item.Symbol,
				MinOrderQty: minQty,
				MaxOrderQty: maxQty,
				QtyStep:     qtyStep,
				TickSize:    tickSize,
			}
		}
		tc.instrumentsMu.Unlock()

		if result.Result.NextPageCursor == "" {
			break
		}
		cursor = result.Result.NextPageCursor
	}

	tc.instrumentsMu.RLock()
	count := len(tc.instruments)
	tc.instrumentsMu.RUnlock()
	log.Printf("[Trade] Loaded precision data for %d instruments", count)

	return nil
}

func (tc *TradeClient) GetInstrument(symbol string) (*InstrumentDetail, bool) {
	tc.instrumentsMu.RLock()
	defer tc.instrumentsMu.RUnlock()
	info, ok := tc.instruments[symbol]
	return info, ok
}

// ── Precision Helpers ──────────────────────────────────────

// FormatQty rounds down qty to symbol's step and returns as string
func (tc *TradeClient) FormatQty(symbol string, qty float64) (string, error) {
	info, ok := tc.GetInstrument(symbol)
	if !ok {
		return "", fmt.Errorf("instrument %s not found in cache", symbol)
	}

	stepped := math.Floor(qty/info.QtyStep) * info.QtyStep
	if stepped < info.MinOrderQty {
		return "", fmt.Errorf("qty %.8f (stepped %.8f) below min %.8f for %s",
			qty, stepped, info.MinOrderQty, symbol)
	}
	if stepped > info.MaxOrderQty {
		stepped = info.MaxOrderQty
	}

	return floatToStr(stepped, info.QtyStep), nil
}

// FormatPrice rounds price to symbol's tick size and returns as string
func (tc *TradeClient) FormatPrice(symbol string, price float64) (string, error) {
	info, ok := tc.GetInstrument(symbol)
	if !ok {
		return "", fmt.Errorf("instrument %s not found in cache", symbol)
	}
	stepped := math.Round(price/info.TickSize) * info.TickSize
	return floatToStr(stepped, info.TickSize), nil
}

func floatToStr(val, step float64) string {
	if step >= 1 {
		return strconv.FormatFloat(val, 'f', 0, 64)
	}
	s := strconv.FormatFloat(step, 'f', -1, 64)
	parts := strings.Split(s, ".")
	decimals := 0
	if len(parts) > 1 {
		decimals = len(parts[1])
	}
	return strconv.FormatFloat(val, 'f', decimals, 64)
}

// ── Trading Operations ─────────────────────────────────────

// SetLeverage sets leverage for a symbol (both buy and sell directions)
func (tc *TradeClient) SetLeverage(symbol string, leverage int) error {
	lev := strconv.Itoa(leverage)
	_, err := tc.doPost("/v5/position/set-leverage", map[string]interface{}{
		"category":    "linear",
		"symbol":      symbol,
		"buyLeverage": lev,
		"sellLeverage": lev,
	})
	if err != nil {
		// 110043 = leverage not modified (already at target)
		if strings.Contains(err.Error(), "110043") {
			return nil
		}
		return fmt.Errorf("set leverage: %w", err)
	}
	return nil
}

// PlaceOrder creates a new order on Bybit
func (tc *TradeClient) PlaceOrder(req PlaceOrderReq) (*OrderResult, error) {
	// Determine positionIdx based on position mode
	posIdx := 0 // one-way mode
	if tc.hedgeMode {
		if req.ReduceOnly {
			// Reduce-only: close the opposite side
			if req.Side == "Buy" {
				posIdx = 2 // closing a Short
			} else {
				posIdx = 1 // closing a Long
			}
		} else {
			// Opening: Buy = Long side (1), Sell = Short side (2)
			if req.Side == "Buy" {
				posIdx = 1
			} else {
				posIdx = 2
			}
		}
	}

	body := map[string]interface{}{
		"category":    "linear",
		"symbol":      req.Symbol,
		"side":        req.Side,
		"orderType":   req.OrderType,
		"qty":         req.Qty,
		"positionIdx": posIdx,
	}

	if req.OrderType == "Limit" {
		body["price"] = req.Price
		tif := req.TimeInForce
		if tif == "" {
			tif = "GTC"
		}
		body["timeInForce"] = tif
	}
	// Market orders: don't set timeInForce — Bybit defaults to IOC

	if req.ReduceOnly {
		body["reduceOnly"] = true
	}

	raw, err := tc.doPostWithRetry("/v5/order/create", body)
	if err != nil {
		return nil, fmt.Errorf("place order: %w", err)
	}

	var resp struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse order response: %w", err)
	}

	return &OrderResult{
		OrderID:     resp.OrderID,
		OrderLinkID: resp.OrderLinkID,
	}, nil
}

// CancelOrder cancels an open order
func (tc *TradeClient) CancelOrder(symbol, orderID string) error {
	_, err := tc.doPostWithRetry("/v5/order/cancel", map[string]interface{}{
		"category": "linear",
		"symbol":   symbol,
		"orderId":  orderID,
	})
	if err != nil {
		// 110001 = order not exists (already filled/cancelled)
		if strings.Contains(err.Error(), "110001") {
			return nil
		}
		return err
	}
	return nil
}

// GetOrder fetches order details — tries realtime first, then history
func (tc *TradeClient) GetOrder(symbol, orderID string) (*OrderDetail, error) {
	// Try realtime (active/recent orders)
	detail, err := tc.getOrderFromEndpoint("/v5/order/realtime", symbol, orderID)
	if err == nil {
		return detail, nil
	}

	// Fallback to order history (filled/cancelled orders)
	detail, err = tc.getOrderFromEndpoint("/v5/order/history", symbol, orderID)
	if err != nil {
		return nil, fmt.Errorf("order %s not found in realtime or history: %w", orderID, err)
	}
	return detail, nil
}

func (tc *TradeClient) getOrderFromEndpoint(endpoint, symbol, orderID string) (*OrderDetail, error) {
	raw, err := tc.doAuthGet(endpoint, map[string]string{
		"category": "linear",
		"symbol":   symbol,
		"orderId":  orderID,
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		List []struct {
			OrderID      string `json:"orderId"`
			Symbol       string `json:"symbol"`
			Side         string `json:"side"`
			OrderType    string `json:"orderType"`
			Price        string `json:"price"`
			Qty          string `json:"qty"`
			CumExecQty   string `json:"cumExecQty"`
			CumExecValue string `json:"cumExecValue"`
			AvgPrice     string `json:"avgPrice"`
			OrderStatus  string `json:"orderStatus"`
			CreatedTime  string `json:"createdTime"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	if len(resp.List) == 0 {
		return nil, fmt.Errorf("order %s not found", orderID)
	}

	o := resp.List[0]
	return &OrderDetail{
		OrderID:      o.OrderID,
		Symbol:       o.Symbol,
		Side:         o.Side,
		OrderType:    o.OrderType,
		Price:        o.Price,
		Qty:          o.Qty,
		CumExecQty:   o.CumExecQty,
		CumExecValue: o.CumExecValue,
		AvgPrice:     o.AvgPrice,
		OrderStatus:  o.OrderStatus,
		CreatedTime:  o.CreatedTime,
	}, nil
}

// GetPosition gets the current position for a symbol
func (tc *TradeClient) GetPosition(symbol string) (*PositionDetail, error) {
	raw, err := tc.doAuthGet("/v5/position/list", map[string]string{
		"category": "linear",
		"symbol":   symbol,
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		List []struct {
			Symbol        string `json:"symbol"`
			Side          string `json:"side"`
			Size          string `json:"size"`
			AvgPrice      string `json:"avgPrice"`
			PositionValue string `json:"positionValue"`
			UnrealisedPnl string `json:"unrealisedPnl"`
			Leverage      string `json:"leverage"`
			LiqPrice      string `json:"liqPrice"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	if len(resp.List) == 0 {
		return &PositionDetail{Symbol: symbol, Side: "None", Size: "0"}, nil
	}

	p := resp.List[0]
	return &PositionDetail{
		Symbol:        p.Symbol,
		Side:          p.Side,
		Size:          p.Size,
		AvgPrice:      p.AvgPrice,
		PositionValue: p.PositionValue,
		UnrealisedPnl: p.UnrealisedPnl,
		Leverage:      p.Leverage,
		LiqPrice:      p.LiqPrice,
	}, nil
}

// GetWalletBalance returns available USDT balance
func (tc *TradeClient) GetWalletBalance() (float64, error) {
	raw, err := tc.doAuthGet("/v5/account/wallet-balance", map[string]string{
		"accountType": "UNIFIED",
		"coin":        "USDT",
	})
	if err != nil {
		return 0, err
	}

	var resp struct {
		List []struct {
			Coin []struct {
				Coin            string `json:"coin"`
				AvailableToWithdraw string `json:"availableToWithdraw"`
				WalletBalance   string `json:"walletBalance"`
			} `json:"coin"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, err
	}

	for _, acct := range resp.List {
		for _, c := range acct.Coin {
			if c.Coin == "USDT" {
				bal, _ := strconv.ParseFloat(c.WalletBalance, 64)
				return bal, nil
			}
		}
	}

	return 0, nil
}

// SetTradingStop sets a stop-loss on an existing position using Bybit's set-trading-stop API.
// positionIdx: 1=Long, 2=Short (hedge mode)
// Retries up to 3 times on transient failures (empty response, network errors).
func (tc *TradeClient) SetTradingStop(symbol string, positionIdx int, stopLoss float64) error {
	slStr, err := tc.FormatPrice(symbol, stopLoss)
	if err != nil {
		return fmt.Errorf("format stop loss price: %w", err)
	}

	payload := map[string]interface{}{
		"category":    "linear",
		"symbol":      symbol,
		"stopLoss":    slStr,
		"slTriggerBy": "LastPrice",
		"positionIdx": positionIdx,
	}

	_, err = tc.doPostWithRetry("/v5/position/trading-stop", payload)
	if err != nil {
		return fmt.Errorf("set trading stop: %w", err)
	}

	log.Printf("[Trade] 🛡️ Stop loss set: %s posIdx=%d SL=%s", symbol, positionIdx, slStr)
	return nil
}

// CancelAllOrders cancels all open orders for a symbol
func (tc *TradeClient) CancelAllOrders(symbol string) error {
	_, err := tc.doPostWithRetry("/v5/order/cancel-all", map[string]interface{}{
		"category": "linear",
		"symbol":   symbol,
	})
	if err != nil {
		return fmt.Errorf("cancel all orders: %w", err)
	}
	return nil
}
