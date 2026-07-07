package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConsumptionReserveMargin(t *testing.T) {
	rep := func(v float64, n int) []float64 {
		s := make([]float64, n)
		for i := range s {
			s[i] = v
		}
		return s
	}

	t.Run("insufficient history yields no margin", func(t *testing.T) {
		margin, samples := consumptionReserveMargin(rep(10, consumptionMarginBaseline+consumptionMarginMinSamples-1))
		assert.Equal(t, 1.0, margin)
		assert.Equal(t, 0, samples)
	})

	t.Run("constant consumption gives margin 1", func(t *testing.T) {
		// every day equals its trailing mean, so all ratios are 1
		margin, samples := consumptionReserveMargin(rep(10, consumptionMarginBaseline+consumptionMarginMinSamples))
		assert.Equal(t, 1.0, margin)
		assert.Equal(t, consumptionMarginMinSamples, samples)
	})

	t.Run("declining consumption is floored at 1", func(t *testing.T) {
		// each day below its trailing mean -> ratios < 1 -> floored
		daily := make([]float64, 60)
		for i := range daily {
			daily[i] = float64(200 - i)
		}
		margin, samples := consumptionReserveMargin(daily)
		assert.Equal(t, 1.0, margin)
		assert.Positive(t, samples)
	})

	t.Run("higher recent consumption yields margin > 1", func(t *testing.T) {
		// 30 baseline days low, then a sustained higher level -> ratios > 1
		daily := append(rep(10, consumptionMarginBaseline), rep(15, 30)...)
		margin, samples := consumptionReserveMargin(daily)
		assert.Greater(t, margin, 1.0)
		assert.Equal(t, 30, samples)
	})
}
