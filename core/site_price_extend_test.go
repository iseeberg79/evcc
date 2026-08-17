package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics"
	"github.com/stretchr/testify/assert"
)

// mkSample builds a PriceSample for a given weekday-relative date/time, expressed as an
// offset in days from a fixed Monday reference so tests don't depend on wall-clock weekday.
func mkSample(daysFromMonday int, hour, minute int, price float64) metrics.PriceSample {
	monday := time.Date(2026, 8, 3, 0, 0, 0, 0, time.Local) // a known Monday
	ts := monday.AddDate(0, 0, daysFromMonday).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	return metrics.PriceSample{Timestamp: ts, Grid: price}
}

func TestWeekdayMatchedPrice(t *testing.T) {
	target := time.Date(2026, 8, 24, 14, 30, 0, 0, time.Local) // a Monday, 14:30

	t.Run("no matching sample returns false", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(1, 14, 30, 0.30), // Tuesday, wrong weekday
			mkSample(0, 15, 0, 0.30),  // Monday, wrong slot
		}
		_, ok := weekdayMatchedPrice(target, samples, 4)
		assert.False(t, ok)
	})

	t.Run("single matching sample is returned as-is", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(0, 14, 30, 0.25), // Monday 14:30
		}
		v, ok := weekdayMatchedPrice(target, samples, 4)
		assert.True(t, ok)
		assert.InDelta(t, 0.25, v, 1e-9)
	})

	t.Run("averages only the most recent `matches` samples, ascending input", func(t *testing.T) {
		// five Mondays at 14:30, ascending order (oldest first) as QueryGridPrices returns them
		samples := []metrics.PriceSample{
			mkSample(-4*7, 14, 30, 100.0), // oldest, must be excluded when matches=4
			mkSample(-3*7, 14, 30, 0.20),
			mkSample(-2*7, 14, 30, 0.30),
			mkSample(-1*7, 14, 30, 0.40),
			mkSample(0, 14, 30, 0.50), // most recent (== target's own week)
		}
		v, ok := weekdayMatchedPrice(target, samples, 4)
		assert.True(t, ok)
		assert.InDelta(t, (0.20+0.30+0.40+0.50)/4, v, 1e-9)
	})

	t.Run("fewer samples than `matches` averages what is available", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(-2*7, 14, 30, 0.20),
			mkSample(-1*7, 14, 30, 0.40),
		}
		v, ok := weekdayMatchedPrice(target, samples, 4)
		assert.True(t, ok)
		assert.InDelta(t, 0.30, v, 1e-9)
	})

	t.Run("ignores samples on the same weekday but a different time-of-day slot", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(-1*7, 14, 15, 0.10), // Monday, adjacent slot
			mkSample(-1*7, 14, 30, 0.40), // Monday, matching slot
		}
		v, ok := weekdayMatchedPrice(target, samples, 4)
		assert.True(t, ok)
		assert.InDelta(t, 0.40, v, 1e-9)
	})
}
