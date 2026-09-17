package core

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/tariff"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForecastSlotEnergy(t *testing.T) {
	slot := time.Unix(1735689600, 0).Truncate(tariff.SlotDuration)
	rate := func(i int, power float64) api.Rate {
		return api.Rate{
			Start: slot.Add(time.Duration(i) * tariff.SlotDuration),
			End:   slot.Add(time.Duration(i+1) * tariff.SlotDuration),
			Value: power,
		}
	}

	rr := api.Rates{rate(0, 4000), rate(1, 8000)}

	// trapezoidal like the published curve, (4kW + 8kW) / 2 * 15min,
	// and identical anywhere inside the slot
	assert.Equal(t, 1.5, forecastSlotEnergy(rr, slot))
	assert.Equal(t, 1.5, forecastSlotEnergy(rr, slot.Add(time.Minute)))

	// the last sample has no successor to integrate towards, as for the daily totals
	assert.Equal(t, 0.0, forecastSlotEnergy(rr, slot.Add(tariff.SlotDuration)))

	// beyond the forecast horizon
	assert.Equal(t, 0.0, forecastSlotEnergy(rr, slot.Add(2*tariff.SlotDuration)))
}

func TestForecastRates(t *testing.T) {
	start := time.Unix(1735689600, 0)

	for _, tc := range []struct {
		desc  string
		rates api.Rates
		want  string
	}{
		{desc: "nil", rates: nil, want: "null"},
		{desc: "empty", rates: api.Rates{}, want: "null"},
		{
			desc: "slots",
			rates: api.Rates{
				{Start: start, End: start.Add(time.Hour), Value: 0.25},
				{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour), Value: -0.1},
			},
			want: "[[1735689600,1735693200,0.25],[1735693200,1735696800,-0.1]]",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			b, err := json.Marshal(forecastRates(tc.rates))
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(b))
		})
	}
}

func TestTimeseriesMarshal(t *testing.T) {
	start := time.Unix(1735689600, 0)

	for _, tc := range []struct {
		desc string
		ts   timeseries
		want string
	}{
		{desc: "nil", ts: nil, want: "null"},
		{desc: "empty", ts: timeseries{}, want: "[]"},
		{
			desc: "entries",
			ts: timeseries{
				{Timestamp: start, Value: 1000},
				{Timestamp: start.Add(time.Hour), Value: 0},
			},
			want: "[[1735689600,1000],[1735693200,0]]",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			b, err := json.Marshal(tc.ts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(b))

			b, err = tc.ts.MarshalBytes()
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(b))
		})
	}
}

func TestPercentileOf(t *testing.T) {
	// n values of v
	fill := func(n int, v float64) []float64 {
		s := make([]float64, n)
		for i := range s {
			s[i] = v
		}
		return s
	}

	t.Run("too few samples returns false", func(t *testing.T) {
		_, ok := percentileOf(nil, 0.5, solarScaleMinSamples)
		assert.False(t, ok)

		_, ok = percentileOf(fill(solarScaleMinSamples-1, 0.9), 0.5, solarScaleMinSamples)
		assert.False(t, ok)
	})

	t.Run("stable cluster", func(t *testing.T) {
		v, ok := percentileOf(fill(20, 0.9), 0.5, solarScaleMinSamples)
		assert.True(t, ok)
		assert.InDelta(t, 0.9, v, 0.001)
	})

	// P50 rejects outlier days for free: a broken forecast feed (recent ratio
	// ~2.3) and a metering outage (ratio ~0.16) do not move the result as
	// long as they stay a minority of the window.
	t.Run("outlier days do not move P50", func(t *testing.T) {
		ratios := fill(20, 0.9)                   // healthy installation bias
		ratios = append(ratios, fill(4, 2.3)...)  // broken forecast feed
		ratios = append(ratios, fill(8, 0.16)...) // metering outage

		v, ok := percentileOf(ratios, 0.5, solarScaleMinSamples)
		assert.True(t, ok)
		assert.InDelta(t, 0.9, v, 0.001)
	})

	t.Run("higher percentile shifts toward the upper tail", func(t *testing.T) {
		ratios := append(fill(15, 0.8), fill(15, 1.2)...)

		p50, _ := percentileOf(ratios, 0.5, solarScaleMinSamples)
		p90, _ := percentileOf(ratios, 0.9, solarScaleMinSamples)
		assert.Less(t, p50, p90)
	})
}

func TestHoldChargeYieldSufficientFrom(t *testing.T) {
	bod := time.Date(2026, 8, 19, 0, 0, 0, 0, time.Local)
	eod := bod.AddDate(0, 0, 1)

	// solarEnergy trapezoidally integrates power between consecutive rate.Start samples,
	// so a flat curve over the whole day needs one sample at bod and one at eod.
	flatSolar := func(w float64) api.Rates {
		return api.Rates{{Start: bod, Value: w}, {Start: eod, Value: w}}
	}

	// 96 slots (15min each) averaging to the given daily total in kWh
	flatProfile := func(dailyKWh float64) [96]float64 {
		var p [96]float64
		for i := range p {
			p[i] = dailyKWh / 96
		}
		return p
	}

	t.Run("comfortably above the factor", func(t *testing.T) {
		assert.True(t, holdChargeYieldSufficientFrom(flatSolar(1000), 0, flatProfile(10), bod, bod)) // 24kWh > 1.5*10kWh
	})

	t.Run("below the factor", func(t *testing.T) {
		assert.False(t, holdChargeYieldSufficientFrom(flatSolar(1000), 0, flatProfile(20), bod, bod)) // 24kWh < 1.5*20kWh
	})

	t.Run("tomorrow's forecast does not count towards today", func(t *testing.T) {
		solar := api.Rates{
			{Start: bod, Value: 100},                    // 2.4kWh today
			{Start: eod, Value: 100},                    // still today's value at the midnight boundary sample
			{Start: eod.Add(time.Hour), Value: 100_000}, // huge, but strictly tomorrow
		}
		assert.False(t, holdChargeYieldSufficientFrom(solar, 0, flatProfile(2), bod, bod)) // 2.4kWh < 1.5*2kWh, despite tomorrow's huge value
	})

	t.Run("elapsed portion tips an otherwise-insufficient remaining forecast over the factor", func(t *testing.T) {
		// remaining forecast alone: 12kWh over the second half of the day, well below
		// 1.5*10kWh - but 12kWh already elapsed (logged history, not live-tariff-derived)
		// pushes the day total to 24kWh, same as the "comfortably above" case above
		noon := bod.Add(12 * time.Hour)
		solar := api.Rates{{Start: noon, Value: 1000}, {Start: eod, Value: 1000}}

		assert.False(t, holdChargeYieldSufficientFrom(solar, 0, flatProfile(10), bod, noon), "remaining forecast alone is insufficient")
		assert.True(t, holdChargeYieldSufficientFrom(solar, 12_000, flatProfile(10), bod, noon), "elapsed history tips it over")
	})
}
