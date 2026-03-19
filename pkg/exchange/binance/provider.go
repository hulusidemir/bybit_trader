package binance

import (
	"strings"

	"bybit_trader/pkg/models"
)

// Provider wraps the Binance Client as a DataProvider.
type Provider struct {
	client *Client
}

func NewProvider(c *Client) *Provider {
	return &Provider{client: c}
}

func (p *Provider) Name() string { return "binance" }

func (p *Provider) SupportsSymbol(bybitSymbol string) bool {
	return IsFuturesSymbolValid(BybitToFuturesSymbol(bybitSymbol))
}

var tfToBinancePeriod = map[string]string{
	"5": "5m", "15": "15m", "60": "1h", "240": "4h",
}

func isInvalidSymbolErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "-1121") || strings.Contains(s, "Invalid symbol")
}

func (p *Provider) FetchOI(bybitSymbol, tfKey string, limit int) ([]models.OpenInterestPoint, error) {
	period, ok := tfToBinancePeriod[tfKey]
	if !ok {
		return nil, nil
	}
	sym := BybitToFuturesSymbol(bybitSymbol)
	if !IsFuturesSymbolValid(sym) {
		return nil, nil
	}
	data, err := p.client.FetchOpenInterestHistory(sym, period, limit)
	if err != nil {
		if isInvalidSymbolErr(err) {
			MarkFuturesInvalid(sym)
		}
		return nil, err
	}
	return data, nil
}

func (p *Provider) FetchPerpTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error) {
	period, ok := tfToBinancePeriod[tfKey]
	if !ok {
		return nil, nil
	}
	sym := BybitToFuturesSymbol(bybitSymbol)
	if !IsFuturesSymbolValid(sym) {
		return nil, nil
	}
	data, err := p.client.FetchTakerBuySellVolume(sym, period, limit)
	if err != nil {
		if isInvalidSymbolErr(err) {
			MarkFuturesInvalid(sym)
		}
		return nil, err
	}
	return data, nil
}

func (p *Provider) FetchSpotTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error) {
	period, ok := tfToBinancePeriod[tfKey]
	if !ok {
		return nil, nil
	}
	sym := BybitToSpotSymbol(bybitSymbol)
	if !IsSpotSymbolValid(sym) {
		return nil, nil
	}
	data, err := p.client.FetchSpotTakerVolume(sym, period, limit)
	if err != nil {
		if isInvalidSymbolErr(err) {
			MarkSpotInvalid(sym)
		}
		return nil, err
	}
	return data, nil
}

func (p *Provider) FetchFuturesOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	sym := BybitToFuturesSymbol(bybitSymbol)
	if !IsFuturesSymbolValid(sym) {
		return nil, nil
	}
	data, err := p.client.FetchFuturesOrderbook(sym, depth)
	if err != nil {
		if isInvalidSymbolErr(err) {
			MarkFuturesInvalid(sym)
		}
		return nil, err
	}
	return data, nil
}

func (p *Provider) FetchSpotOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	sym := BybitToSpotSymbol(bybitSymbol)
	if !IsSpotSymbolValid(sym) {
		return nil, nil
	}
	data, err := p.client.FetchSpotOrderbook(sym, depth)
	if err != nil {
		if isInvalidSymbolErr(err) {
			MarkSpotInvalid(sym)
		}
		return nil, err
	}
	return data, nil
}
