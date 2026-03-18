package analysis

import (
	"math"

	"bybit_trader/pkg/models"
)

// timeframes used for analysis
var timeframes = []string{"5", "15", "60", "240"}

// ── Helper Functions ────────────────────────────────────────

func classifyTrend(change, moderate, strong float64) models.Trend {
	abs := math.Abs(change)
	if abs < moderate {
		return models.TrendNeutral
	}
	if change > 0 {
		if abs >= strong {
			return models.TrendStrongUp
		}
		return models.TrendUp
	}
	if abs >= strong {
		return models.TrendStrongDown
	}
	return models.TrendDown
}

func classifyCVDTrend(cvd, volume24h float64) models.Trend {
	if volume24h == 0 {
		return models.TrendNeutral
	}
	// Normalize CVD against 24h volume
	ratio := cvd / volume24h
	if math.Abs(ratio) < 0.001 {
		return models.TrendNeutral
	}
	if ratio > 0 {
		if ratio > 0.005 {
			return models.TrendStrongUp
		}
		return models.TrendUp
	}
	if ratio < -0.005 {
		return models.TrendStrongDown
	}
	return models.TrendDown
}

func classifyOBBias(ratio float64) models.OrderbookBias {
	if ratio > 1.5 {
		return models.OBBidHeavy
	}
	if ratio > 1.15 {
		return models.OBBidWall
	}
	if ratio < 0.67 {
		return models.OBAskHeavy
	}
	if ratio < 0.87 {
		return models.OBAskWall
	}
	return models.OBBalanced
}

// findWall finds the largest order in the book (wall)
func findWall(levels []models.OrderbookLevel) (price, size float64) {
	maxVal := 0.0
	for _, l := range levels {
		val := l.Amount * l.Price
		if val > maxVal {
			maxVal = val
			price = l.Price
			size = l.Amount
		}
	}
	return
}

func aggregateOBVolume(orderbooks []*models.OrderbookSnapshot) (bidVol, askVol float64) {
	for _, ob := range orderbooks {
		for _, b := range ob.Bids {
			bidVol += b.Amount * b.Price
		}
		for _, a := range ob.Asks {
			askVol += a.Amount * a.Price
		}
	}
	return
}
