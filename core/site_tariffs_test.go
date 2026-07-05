package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSolarScaleFactor(t *testing.T) {
	// n ratios of value r
	fill := func(n int, r float64) []float64 {
		s := make([]float64, n)
		for i := range s {
			s[i] = r
		}
		return s
	}

	t.Run("too few days returns 1", func(t *testing.T) {
		assert.Equal(t, 1.0, solarScaleFactor(nil))
		assert.Equal(t, 1.0, solarScaleFactor(fill(solarScaleMinDays-1, 0.9)))
	})

	t.Run("stable cluster", func(t *testing.T) {
		assert.InDelta(t, 0.9, solarScaleFactor(fill(20, 0.9)), 0.001)
	})

	// the median rejects outlier days for free: a broken forecast feed (recent
	// ratio ~2.3) and a metering outage (ratio ~0.16) do not move the result as
	// long as they stay a minority of the window.
	t.Run("outlier days do not move the median", func(t *testing.T) {
		ratios := fill(20, 0.9)                   // healthy installation bias
		ratios = append(ratios, fill(4, 2.3)...)  // broken forecast feed
		ratios = append(ratios, fill(8, 0.16)...) // metering outage
		assert.InDelta(t, 0.9, solarScaleFactor(ratios), 0.001)
	})
}
