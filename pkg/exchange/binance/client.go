package binance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"bybit_trader/pkg/models"
)

const (
	futuresBaseURL = "https://fapi.binance.com"
	spotBaseURL    = "https://api.binance.com"
	rateLimit      = 10
)

type Client struct {
	http    *http.Client
	limiter chan struct{}
}

func NewClient() *Client {
	c := &Client{
		http: &http.Client{
			Timeout: 15 * time.Second,
		},
		limiter: make(chan struct{}, rateLimit),
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
	return c
}

func (c *Client) doGet(baseURL, endpoint string, params map[string]string) ([]byte, error) {
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

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}

	return body, nil
}

// FetchTakerBuySellVolume derives taker buy/sell volume from futures klines.
// Binance futures kline fields:
//   [0] openTime, [1] open, [2] high, [3] low, [4] close,
//   [5] volume (base), [6] closeTime, [7] quoteAssetVolume,
//   [8] numTrades, [9] takerBuyBaseVol, [10] takerBuyQuoteVol
// interval: 1m, 5m, 15m, 30m, 1h, 4h, 1d
func (c *Client) FetchTakerBuySellVolume(symbol, interval string, limit int) ([]models.TakerVolume, error) {
	body, err := c.doGet(futuresBaseURL, "/fapi/v1/klines", map[string]string{
		"symbol":   symbol,
		"interval": interval,
		"limit":    strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}

	var resp [][]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal futures kline: %w", err)
	}

	result := make([]models.TakerVolume, 0, len(resp))
	for _, row := range resp {
		if len(row) < 11 {
			continue
		}

		var ts int64
		json.Unmarshal(row[0], &ts)

		parseStr := func(raw json.RawMessage) float64 {
			var s string
			json.Unmarshal(raw, &s)
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}

		totalVol := parseStr(row[5])    // total base asset volume
		takerBuyVol := parseStr(row[9]) // taker buy base asset volume
		takerSellVol := totalVol - takerBuyVol

		ratio := 0.0
		if takerSellVol > 0 {
			ratio = takerBuyVol / takerSellVol
		}

		result = append(result, models.TakerVolume{
			Timestamp:    ts,
			BuyVolume:    takerBuyVol,
			SellVolume:   takerSellVol,
			BuySellRatio: ratio,
		})
	}

	return result, nil
}

// FetchOpenInterest returns current open interest for a symbol
func (c *Client) FetchOpenInterest(symbol string) (*models.OpenInterestPoint, error) {
	body, err := c.doGet(futuresBaseURL, "/fapi/v1/openInterest", map[string]string{
		"symbol": symbol,
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		OpenInterest string `json:"openInterest"`
		Symbol       string `json:"symbol"`
		Time         int64  `json:"time"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal OI: %w", err)
	}

	oi, _ := strconv.ParseFloat(resp.OpenInterest, 64)
	return &models.OpenInterestPoint{
		Timestamp:    resp.Time,
		OpenInterest: oi,
	}, nil
}

// FetchSpotKline returns spot kline data for spot CVD calculation
// Spot kline also contains taker buy vol in field[9]
// interval: 1m, 3m, 5m, 15m, 30m, 1h, 2h, 4h, 1d
func (c *Client) FetchSpotKline(symbol, interval string, limit int) ([]models.OHLCV, error) {
	body, err := c.doGet(spotBaseURL, "/api/v3/klines", map[string]string{
		"symbol":   symbol,
		"interval": interval,
		"limit":    strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}

	var resp [][]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal kline: %w", err)
	}

	candles := make([]models.OHLCV, 0, len(resp))
	for _, row := range resp {
		if len(row) < 11 {
			continue
		}
		var ts int64
		json.Unmarshal(row[0], &ts)

		parseStr := func(raw json.RawMessage) float64 {
			var s string
			json.Unmarshal(raw, &s)
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}

		candles = append(candles, models.OHLCV{
			Timestamp: ts,
			Open:      parseStr(row[1]),
			High:      parseStr(row[2]),
			Low:       parseStr(row[3]),
			Close:     parseStr(row[4]),
			Volume:    parseStr(row[5]),
			Turnover:  parseStr(row[7]), // quote asset volume
		})
	}

	return candles, nil
}

// FetchSpotTakerVolume derives taker buy/sell from spot klines
// Spot kline field[9] = taker buy base vol
func (c *Client) FetchSpotTakerVolume(symbol, interval string, limit int) ([]models.TakerVolume, error) {
	body, err := c.doGet(spotBaseURL, "/api/v3/klines", map[string]string{
		"symbol":   symbol,
		"interval": interval,
		"limit":    strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}

	var resp [][]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal spot kline: %w", err)
	}

	result := make([]models.TakerVolume, 0, len(resp))
	for _, row := range resp {
		if len(row) < 11 {
			continue
		}
		var ts int64
		json.Unmarshal(row[0], &ts)

		parseStr := func(raw json.RawMessage) float64 {
			var s string
			json.Unmarshal(raw, &s)
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}

		totalVol := parseStr(row[5])
		takerBuyVol := parseStr(row[9])
		takerSellVol := totalVol - takerBuyVol

		ratio := 0.0
		if takerSellVol > 0 {
			ratio = takerBuyVol / takerSellVol
		}

		result = append(result, models.TakerVolume{
			Timestamp:    ts,
			BuyVolume:    takerBuyVol,
			SellVolume:   takerSellVol,
			BuySellRatio: ratio,
		})
	}

	return result, nil
}

// BybitToFuturesSymbol converts a Bybit symbol to Binance futures symbol.
// Most symbols are the same (BTCUSDT -> BTCUSDT).
// Bybit and Binance both use 1000X notation for futures.
func BybitToFuturesSymbol(bybitSymbol string) string {
	return bybitSymbol // Usually identical for futures
}

// BybitToSpotSymbol converts a Bybit perpetual symbol to Binance spot symbol.
// Key differences:
//   - "1000PEPEUSDT" (futures) -> "PEPEUSDT" (spot)
//   - "1000SHIBUSDT" -> "SHIBUSDT"
//   - "1000FLOKIUSDT" -> "FLOKIUSDT"
//   - "1000BONKUSDT" -> "BONKUSDT"
//   - "1000LUNCUSDT" -> "LUNCUSDT"
//   - "1000RATSUSDT" -> "RATSUSDT"
//   - "1000XECUSDT" -> "XECUSDT"
//   - "10000SATSUSDT" -> "1000SATSUSDT"
//   - "10000WENUSDT" -> no direct mapping
func BybitToSpotSymbol(bybitSymbol string) string {
	base := SymbolToBaseCoin(bybitSymbol)

	// Handle "10000X" prefix -> keep "1000X" for spot in special cases
	if len(base) > 5 && base[:5] == "10000" {
		// 10000SATS -> 1000SATS (Binance spot), others probably don't exist
		return "1000" + base[5:] + "USDT"
	}

	// Handle "1000X" prefix -> strip for spot
	if len(base) > 4 && base[:4] == "1000" {
		return base[4:] + "USDT"
	}

	return bybitSymbol
}

// SymbolToBaseCoin extracts base coin from Bybit symbol (e.g., "BTCUSDT" -> "BTC")
func SymbolToBaseCoin(symbol string) string {
	if len(symbol) > 4 && symbol[len(symbol)-4:] == "USDT" {
		return symbol[:len(symbol)-4]
	}
	return symbol
}

// InvalidSymbolCache tracks symbols that returned 400 from Binance
// so we don't keep retrying them every scan cycle
var (
	invalidFuturesSymbols = make(map[string]bool)
	invalidSpotSymbols    = make(map[string]bool)
)

// IsFuturesSymbolValid checks if a symbol is known to be invalid on Binance futures
func IsFuturesSymbolValid(symbol string) bool {
	return !invalidFuturesSymbols[symbol]
}

// IsSpotSymbolValid checks if a symbol is known to be invalid on Binance spot
func IsSpotSymbolValid(symbol string) bool {
	return !invalidSpotSymbols[symbol]
}

// MarkFuturesInvalid marks a symbol as invalid on Binance futures
func MarkFuturesInvalid(symbol string) {
	invalidFuturesSymbols[symbol] = true
}

// MarkSpotInvalid marks a symbol as invalid on Binance spot
func MarkSpotInvalid(symbol string) {
	invalidSpotSymbols[symbol] = true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
