package core

import (
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/tariff"
)

// priceHorizonWindow is the trailing calendar window averaged by calendarTrailingPrice.
// Validated by backtest against 18 months of real day-ahead prices: a plain trailing
// calendar window consistently beats matching by weekday (rank correlation 0.71 vs 0.66
// over the full period, in both the hourly and 15min data eras) - the larger sample size
// per slot (~14 history points vs. ~4 for a 4-week weekday match) outweighs the
// conceptual appeal of matching by weekday, since prices are driven more by weather
// (solar/wind) than by weekday/weekend patterns. 14 days is close to the backtest
// optimum: 7 days ties on average but is materially less stable (worst-case monthly rank
// correlation 0.19 vs 0.32 for 14d+); shorter windows trade robustness for recency,
// longer windows dilute the average further. See project notes.
const priceHorizonWindow = 14 * 24 * time.Hour

// calendarTrailingPrice estimates the price for target as the mean of all samples at the
// same time-of-day slot within the trailing window before target. samples must be sorted
// ascending by Timestamp (as returned by metrics.QueryGridPrices). Returns false if no
// matching sample exists at all.
func calendarTrailingPrice(target time.Time, samples []metrics.PriceSample, window time.Duration) (float64, bool) {
	slot := target.Hour()*4 + target.Minute()/15
	cutoff := target.Add(-window)

	var sum float64
	var n int
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if s.Timestamp.Before(cutoff) {
			break
		}
		if !s.Timestamp.Before(target) {
			continue
		}
		if sSlot := s.Timestamp.Hour()*4 + s.Timestamp.Minute()/15; sSlot != slot {
			continue
		}
		sum += s.Grid
		n++
	}

	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// extendGridRates appends synthetic slots derived from calendarTrailingPrice to rates so the
// series reaches horizon, when the real (day-ahead) data does not extend that far. The
// synthetic tail is self-correcting: the optimizer re-runs every cycle, so a slot is
// replaced by the real day-ahead price as soon as it becomes available - see project notes
// for the backtest that established priceHorizonWindow. Falls back to the unmodified
// rates on any history query error or once no matching sample is found for a slot, rather
// than extending on thinner and thinner history.
//
// Gated on GetPriceHorizonExtended() at the call sites in optimizerRequest() and
// publishTariffs(), off by default.
func (site *Site) extendGridRates(rates api.Rates, horizon time.Time) api.Rates {
	if len(rates) == 0 || !rates[len(rates)-1].End.Before(horizon) {
		return rates
	}

	from := time.Now().Add(-priceHorizonWindow)
	samples, err := metrics.QueryGridPrices(from, time.Now())
	if err != nil || len(samples) == 0 {
		return rates
	}

	res := slices.Clone(rates)
	for start := rates[len(rates)-1].End; start.Before(horizon); start = start.Add(tariff.SlotDuration) {
		v, ok := calendarTrailingPrice(start, samples, priceHorizonWindow)
		if !ok {
			break
		}
		res = append(res, api.Rate{Start: start, End: start.Add(tariff.SlotDuration), Value: v})
	}

	return res
}
