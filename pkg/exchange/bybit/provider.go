package bybit

import "bybit_trader/pkg/models"

// Provider wraps the Bybit Client as a DataProvider.
type Provider struct {
	client *Client
}

func NewProvider(c *Client) *Provider {
	return &Provider{client: c}
}

func (p *Provider) Name() string { return "bybit" }

// SupportsSymbol always returns true since we only scan Bybit-listed coins.
func (p *Provider) SupportsSymbol(_ string) bool { return true }

var tfToBybitOIInterval = map[string]string{
	"5": "5min", "15": "15min", "60": "1h", "240": "4h",
}

var tfToBybitLSPeriod = map[string]string{
	"5": "5min", "15": "15min", "60": "1h", "240": "4h",
}

func (p *Provider) FetchOI(bybitSymbol, tfKey string, limit int) ([]models.OpenInterestPoint, error) {
	interval, ok := tfToBybitOIInterval[tfKey]
	if !ok {
		return nil, nil
	}
	return p.client.FetchOpenInterest(bybitSymbol, interval, limit)
}

// FetchPerpTakerVolume returns nil — Bybit klines don't include taker buy/sell split.
func (p *Provider) FetchPerpTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

// FetchSpotTakerVolume returns nil — Bybit has no spot taker volume data.
func (p *Provider) FetchSpotTakerVolume(_ string, _ string, _ int) ([]models.TakerVolume, error) {
	return nil, nil
}

func (p *Provider) FetchFuturesOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error) {
	return p.client.FetchOrderbook(bybitSymbol, depth)
}

// FetchSpotOrderbook returns nil — Bybit spot not available via this client.
func (p *Provider) FetchSpotOrderbook(_ string, _ int) (*models.OrderbookSnapshot, error) {
	return nil, nil
}
