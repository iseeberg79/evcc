package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics"
	"github.com/stretchr/testify/assert"
)

// mkSample builds a PriceSample at a given offset (in days) before a fixed reference date,
// so tests don't depend on wall-clock time.
func mkSample(daysAgo int, hour, minute int, price float64) metrics.PriceSample {
	ref := time.Date(2026, 8, 24, 0, 0, 0, 0, time.Local)
	ts := ref.AddDate(0, 0, -daysAgo).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	return metrics.PriceSample{Timestamp: ts, Grid: price}
}

func TestCalendarTrailingPrice(t *testing.T) {
	target := time.Date(2026, 8, 24, 14, 30, 0, 0, time.Local)
	window := 14 * 24 * time.Hour

	t.Run("no matching sample returns false", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(20, 14, 30, 0.30), // outside the window
			mkSample(1, 15, 0, 0.30),   // wrong slot
		}
		_, ok := calendarTrailingPrice(target, samples, window)
		assert.False(t, ok)
	})

	t.Run("single matching sample is returned as-is", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(1, 14, 30, 0.25),
		}
		v, ok := calendarTrailingPrice(target, samples, window)
		assert.True(t, ok)
		assert.InDelta(t, 0.25, v, 1e-9)
	})

	t.Run("averages all samples within the window, ascending input", func(t *testing.T) {
		// ascending order (oldest first) as QueryGridPrices returns them
		samples := []metrics.PriceSample{
			mkSample(20, 14, 30, 100.0), // outside the window, must be excluded
			mkSample(10, 14, 30, 0.20),
			mkSample(7, 14, 30, 0.30),
			mkSample(3, 14, 30, 0.40),
			mkSample(1, 14, 30, 0.50),
		}
		v, ok := calendarTrailingPrice(target, samples, window)
		assert.True(t, ok)
		assert.InDelta(t, (0.20+0.30+0.40+0.50)/4, v, 1e-9)
	})

	t.Run("ignores samples at a different time-of-day slot", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(1, 14, 15, 0.10), // adjacent slot
			mkSample(1, 14, 30, 0.40), // matching slot
		}
		v, ok := calendarTrailingPrice(target, samples, window)
		assert.True(t, ok)
		assert.InDelta(t, 0.40, v, 1e-9)
	})

	t.Run("ignores samples at or after target", func(t *testing.T) {
		samples := []metrics.PriceSample{
			mkSample(1, 14, 30, 0.40),
			{Timestamp: target, Grid: 100.0},
			{Timestamp: target.Add(time.Hour), Grid: 100.0},
		}
		v, ok := calendarTrailingPrice(target, samples, window)
		assert.True(t, ok)
		assert.InDelta(t, 0.40, v, 1e-9)
	})
}
