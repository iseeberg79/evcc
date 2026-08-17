package core

import (
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/tariff"
)

// priceHorizonMatches is how many past occurrences of the same weekday/time-of-day slot
// are averaged for weekdayMatchedPrice. Validated by backtest against 18 months of real
// day-ahead prices: matching by weekday instead of a plain trailing calendar window avoids
// mixing weekday/weekend price shape, and 4 weeks balances responsiveness against noise.
// Accuracy is materially weaker in winter/transition months than in summer (rank
// correlation ~0.75-0.78 vs ~0.85-0.92), but never degenerates - see project notes.
const priceHorizonMatches = 4

// weekdayMatchedPrice estimates the price for target from the last priceHorizonMatches
// occurrences of the same weekday and time-of-day in samples. samples must be sorted
// ascending by Timestamp (as returned by metrics.QueryGridPrices). Returns false if no
// matching sample exists at all.
func weekdayMatchedPrice(target time.Time, samples []metrics.PriceSample, matches int) (float64, bool) {
	slot := target.Hour()*4 + target.Minute()/15
	weekday := target.Weekday()

	var sum float64
	var n int
	for i := len(samples) - 1; i >= 0 && n < matches; i-- {
		s := samples[i]
		if s.Timestamp.Weekday() != weekday {
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

// extendGridRates appends synthetic slots derived from weekdayMatchedPrice to rates so the
// series reaches horizon, when the real (day-ahead) data does not extend that far. The
// synthetic tail is self-correcting: the optimizer re-runs every cycle, so a slot is
// replaced by the real day-ahead price as soon as it becomes available - see project notes
// for the backtest that established priceHorizonMatches. Falls back to the unmodified
// rates on any history query error or once no matching sample is found for a slot, rather
// than extending on thinner and thinner history.
//
// Gated on GetPriceHorizonExtended() at the call site in optimizerRequest(), off by
// default. The synthetic tail is not yet marked as such in the published forecast - see
// project notes for that follow-up.
func (site *Site) extendGridRates(rates api.Rates, horizon time.Time) api.Rates {
	if len(rates) == 0 || !rates[len(rates)-1].End.Before(horizon) {
		return rates
	}

	from := time.Now().AddDate(0, 0, -7*priceHorizonMatches)
	samples, err := metrics.QueryGridPrices(from, time.Now())
	if err != nil || len(samples) == 0 {
		return rates
	}

	res := slices.Clone(rates)
	for start := rates[len(rates)-1].End; start.Before(horizon); start = start.Add(tariff.SlotDuration) {
		v, ok := weekdayMatchedPrice(start, samples, priceHorizonMatches)
		if !ok {
			break
		}
		res = append(res, api.Rate{Start: start, End: start.Add(tariff.SlotDuration), Value: v})
	}

	return res
}
