package exchange

import "bybit_trader/pkg/models"

// DataProvider is an interface for fetching market data from an exchange.
// Methods accept Bybit-format symbols (e.g., "BTCUSDT") and timeframe keys ("5","15","60","240").
// Implementations handle symbol conversion and interval mapping internally.
type DataProvider interface {
	Name() string
	SupportsSymbol(bybitSymbol string) bool
	FetchOI(bybitSymbol, tfKey string, limit int) ([]models.OpenInterestPoint, error)
	FetchPerpTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error)
	FetchSpotTakerVolume(bybitSymbol, tfKey string, limit int) ([]models.TakerVolume, error)
	FetchFuturesOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error)
	FetchSpotOrderbook(bybitSymbol string, depth int) (*models.OrderbookSnapshot, error)
	FetchLSRatio(bybitSymbol, tfKey string, limit int) ([]models.LongShortRatio, error)
}

// AggregateOIChange calculates aggregated OI % change across multiple exchange series.
// It sums the first and last OI values from all series, then computes the total % change.
func AggregateOIChange(allSeries [][]models.OpenInterestPoint) float64 {
	if len(allSeries) == 0 {
		return 0
	}

	var totalFirst, totalLast float64
	valid := false
	for _, series := range allSeries {
		if len(series) < 2 {
			continue
		}
		totalFirst += series[0].OpenInterest
		totalLast += series[len(series)-1].OpenInterest
		valid = true
	}

	if !valid || totalFirst == 0 {
		return 0
	}
	return ((totalLast - totalFirst) / totalFirst) * 100
}
