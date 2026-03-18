package bybit

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"bybit_trader/pkg/models"
)

const (
	baseURL    = "https://api.bybit.com"
	rateLimit  = 5 // requests per second (conservative to avoid 10006 errors)
)

type Client struct {
	http    *http.Client
	limiter chan struct{}
	mu      sync.Mutex
}

func NewClient() *Client {
	c := &Client{
		http: &http.Client{
			Timeout: 15 * time.Second,
		},
		limiter: make(chan struct{}, rateLimit),
	}
	// Fill limiter tokens
	for i := 0; i < rateLimit; i++ {
		c.limiter <- struct{}{}
	}
	// Replenish tokens
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
	return c
}

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
		return nil, fmt.Errorf("read body failed: %w", err)
	}

	var result struct {
		RetCode int             `json:"retCode"`
		RetMsg  string          `json:"retMsg"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal failed: %w", err)
	}
	if result.RetCode != 0 {
		return nil, fmt.Errorf("API error %d: %s", result.RetCode, result.RetMsg)
	}

	return result.Result, nil
}

// FetchInstruments returns all active USDT perpetual contracts
func (c *Client) FetchInstruments() ([]models.Coin, error) {
	var allCoins []models.Coin
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

		raw, err := c.doGet("/v5/market/instruments-info", params)
		if err != nil {
			return nil, err
		}

		var resp struct {
			List []struct {
				Symbol     string `json:"symbol"`
				BaseCoin   string `json:"baseCoin"`
				QuoteCoin  string `json:"quoteCoin"`
				LaunchTime string `json:"launchTime"`
				Status     string `json:"status"`
			} `json:"list"`
			NextPageCursor string `json:"nextPageCursor"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}

		for _, item := range resp.List {
			if item.QuoteCoin != "USDT" {
				continue
			}
			lt, _ := strconv.ParseInt(item.LaunchTime, 10, 64)
			allCoins = append(allCoins, models.Coin{
				Symbol:    item.Symbol,
				BaseCoin:  item.BaseCoin,
				QuoteCoin: item.QuoteCoin,
				LaunchTime: lt,
				Status:    item.Status,
			})
		}

		if resp.NextPageCursor == "" {
			break
		}
		cursor = resp.NextPageCursor
	}

	return allCoins, nil
}

// FetchTickers returns all linear USDT perpetual tickers (price, volume, OI, funding)
func (c *Client) FetchTickers() (map[string]*models.Coin, error) {
	raw, err := c.doGet("/v5/market/tickers", map[string]string{
		"category": "linear",
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		List []struct {
			Symbol             string `json:"symbol"`
			LastPrice          string `json:"lastPrice"`
			Volume24h          string `json:"volume24h"`
			Turnover24h        string `json:"turnover24h"`
			FundingRate        string `json:"fundingRate"`
			OpenInterest       string `json:"openInterest"`
			OpenInterestValue  string `json:"openInterestValue"`
			NextFundingTime    string `json:"nextFundingTime"`
			FundingIntervalHour string `json:"fundingIntervalHour"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	result := make(map[string]*models.Coin)
	for _, t := range resp.List {
		price, _ := strconv.ParseFloat(t.LastPrice, 64)
		vol, _ := strconv.ParseFloat(t.Volume24h, 64)
		turn, _ := strconv.ParseFloat(t.Turnover24h, 64)
		fund, _ := strconv.ParseFloat(t.FundingRate, 64)
		oi, _ := strconv.ParseFloat(t.OpenInterest, 64)
		nextFund, _ := strconv.ParseInt(t.NextFundingTime, 10, 64)
		fundInterval, _ := strconv.Atoi(t.FundingIntervalHour)

		result[t.Symbol] = &models.Coin{
			Symbol:          t.Symbol,
			LastPrice:       price,
			Volume24h:       vol,
			Turnover24h:     turn,
			FundingRate:     fund,
			OpenInterest:    oi,
			NextFundingTime: nextFund,
			FundingInterval: fundInterval,
		}
	}

	return result, nil
}

// FetchOpenInterest returns historical OI data
// intervalTime: 5min, 15min, 30min, 1h, 4h, 1d
func (c *Client) FetchOpenInterest(symbol, intervalTime string, limit int) ([]models.OpenInterestPoint, error) {
	raw, err := c.doGet("/v5/market/open-interest", map[string]string{
		"category":     "linear",
		"symbol":       symbol,
		"intervalTime": intervalTime,
		"limit":        strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		List []struct {
			OpenInterest string `json:"openInterest"`
			Timestamp    string `json:"timestamp"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	points := make([]models.OpenInterestPoint, 0, len(resp.List))
	for i := len(resp.List) - 1; i >= 0; i-- { // reverse to ascending
		item := resp.List[i]
		oi, _ := strconv.ParseFloat(item.OpenInterest, 64)
		ts, _ := strconv.ParseInt(item.Timestamp, 10, 64)
		points = append(points, models.OpenInterestPoint{
			Timestamp:    ts,
			OpenInterest: oi,
		})
	}

	return points, nil
}

// FetchOrderbook returns orderbook depth
func (c *Client) FetchOrderbook(symbol string, depth int) (*models.OrderbookSnapshot, error) {
	raw, err := c.doGet("/v5/market/orderbook", map[string]string{
		"category": "linear",
		"symbol":   symbol,
		"limit":    strconv.Itoa(depth),
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		S  string     `json:"s"`
		B  [][]string `json:"b"` // bids: [price, size]
		A  [][]string `json:"a"` // asks: [price, size]
		Ts int64      `json:"ts"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	ob := &models.OrderbookSnapshot{
		Symbol:    symbol,
		Timestamp: resp.Ts,
		Bids:      make([]models.OrderbookLevel, 0, len(resp.B)),
		Asks:      make([]models.OrderbookLevel, 0, len(resp.A)),
	}

	for _, b := range resp.B {
		if len(b) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(b[0], 64)
		size, _ := strconv.ParseFloat(b[1], 64)
		ob.Bids = append(ob.Bids, models.OrderbookLevel{Price: price, Amount: size})
	}
	for _, a := range resp.A {
		if len(a) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(a[0], 64)
		size, _ := strconv.ParseFloat(a[1], 64)
		ob.Asks = append(ob.Asks, models.OrderbookLevel{Price: price, Amount: size})
	}

	return ob, nil
}

// FetchKline returns OHLCV kline data
// interval: 1, 3, 5, 15, 30, 60, 120, 240, 360, 720, D, W, M
func (c *Client) FetchKline(symbol, interval string, limit int) ([]models.OHLCV, error) {
	raw, err := c.doGet("/v5/market/kline", map[string]string{
		"category": "linear",
		"symbol":   symbol,
		"interval": interval,
		"limit":    strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		List [][]string `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	candles := make([]models.OHLCV, 0, len(resp.List))
	for i := len(resp.List) - 1; i >= 0; i-- { // reverse to ascending
		row := resp.List[i]
		if len(row) < 7 {
			continue
		}
		ts, _ := strconv.ParseInt(row[0], 10, 64)
		o, _ := strconv.ParseFloat(row[1], 64)
		h, _ := strconv.ParseFloat(row[2], 64)
		l, _ := strconv.ParseFloat(row[3], 64)
		cl, _ := strconv.ParseFloat(row[4], 64)
		v, _ := strconv.ParseFloat(row[5], 64)
		t, _ := strconv.ParseFloat(row[6], 64)

		candles = append(candles, models.OHLCV{
			Timestamp: ts,
			Open:      o,
			High:      h,
			Low:       l,
			Close:     cl,
			Volume:    v,
			Turnover:  t,
		})
	}

	return candles, nil
}

// FetchLongShortRatio returns account long/short ratio
// period: 5min, 15min, 30min, 1h, 4h, 1d
func (c *Client) FetchLongShortRatio(symbol, period string, limit int) ([]models.LongShortRatio, error) {
	raw, err := c.doGet("/v5/market/account-ratio", map[string]string{
		"category": "linear",
		"symbol":   symbol,
		"period":   period,
		"limit":    strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		List []struct {
			Symbol    string `json:"symbol"`
			BuyRatio  string `json:"buyRatio"`
			SellRatio string `json:"sellRatio"`
			Timestamp string `json:"timestamp"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	result := make([]models.LongShortRatio, 0, len(resp.List))
	for i := len(resp.List) - 1; i >= 0; i-- {
		item := resp.List[i]
		buy, _ := strconv.ParseFloat(item.BuyRatio, 64)
		sell, _ := strconv.ParseFloat(item.SellRatio, 64)
		ts, _ := strconv.ParseInt(item.Timestamp, 10, 64)

		ratio := 0.0
		if sell > 0 {
			ratio = buy / sell
		}

		result = append(result, models.LongShortRatio{
			Timestamp: ts,
			BuyRatio:  buy,
			SellRatio: sell,
			Ratio:     ratio,
		})
	}

	return result, nil
}
