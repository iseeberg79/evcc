package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
)

func TestSpreadChargePower(t *testing.T) {
	for _, tc := range []struct {
		name                                                             string
		deficitWh, availableHours, maxACPower, maxChargePower, pvSurplus float64
		want                                                             float64
	}{
		{"no deficit", 0, 4, 6000, 6000, 3000, 0},
		{"no time left", 3000, 0, 6000, 6000, 3000, 0},
		{"no pv surplus never forces grid import", 3000, 4, 6000, 6000, 0, 0},
		{
			// 3kWh over 4h, minus 1h buffer (25%) = 3h safe -> 1000W, within pv surplus
			"spread evenly outside buffer", 3000, 4, 6000, 6000, 3000, 1000,
		},
		{
			// capped by pv surplus even though the spread power would be higher
			"capped to pv surplus", 3000, 4, 6000, 6000, 500, 500,
		},
		{
			// below 500W threshold is not worth switching into hold charge
			"below minimum power drops to zero", 200, 4, 6000, 6000, 3000, 0,
		},
		{
			// short remaining time -> small safe-time divisor -> power ramps up naturally
			// (3kWh over 1h, minus 0.25h buffer = 0.75h safe -> 4000W, within pv surplus)
			"short window ramps power up", 3000, 1, 6000, 6000, 5000, 4000,
		},
		{
			// buffer capped at 2h even for a long remaining window
			"buffer capped at 2h for long windows", 6000, 10, 6000, 6000, 6000, 750,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := spreadChargePower(tc.deficitWh, tc.availableHours, tc.maxACPower, tc.maxChargePower, tc.pvSurplus)
			assert.InDelta(t, tc.want, got, 1e-6)
		})
	}
}

func TestSolarCutoffTime(t *testing.T) {
	now := time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC)

	t.Run("finds the first low-solar slot after now", func(t *testing.T) {
		rates := api.Rates{
			{Start: now.Add(-15 * time.Minute), End: now.Add(15 * time.Minute), Value: 2000},
			{Start: now.Add(15 * time.Minute), End: now.Add(30 * time.Minute), Value: 40},
			{Start: now.Add(30 * time.Minute), End: now.Add(45 * time.Minute), Value: 0},
		}
		got := solarCutoffTime(rates, now)
		assert.Equal(t, now.Add(15*time.Minute), got)
	})

	t.Run("already-low current slot cuts off now", func(t *testing.T) {
		rates := api.Rates{
			{Start: now.Add(-15 * time.Minute), End: now.Add(15 * time.Minute), Value: 10},
		}
		got := solarCutoffTime(rates, now)
		assert.Equal(t, now, got)
	})

	t.Run("no low slot falls back to the end of the last rate", func(t *testing.T) {
		rates := api.Rates{
			{Start: now, End: now.Add(15 * time.Minute), Value: 2000},
			{Start: now.Add(15 * time.Minute), End: now.Add(30 * time.Minute), Value: 1800},
		}
		got := solarCutoffTime(rates, now)
		assert.Equal(t, rates[len(rates)-1].End, got)
	})
}

// fakeBattery implements api.Battery, api.BatteryCapacity and api.BatterySocLimiter for
// calculateBatteryDeficit tests without pulling in gomock (these caps aren't in the
// generated mock set).
type fakeBattery struct {
	api.Meter
	soc, capacity, minSoc, maxSoc float64
}

func (f *fakeBattery) Soc() (float64, error)            { return f.soc, nil }
func (f *fakeBattery) Capacity() float64                { return f.capacity }
func (f *fakeBattery) GetSocLimits() (min, max float64) { return f.minSoc, f.maxSoc }

func TestCalculateBatteryDeficit(t *testing.T) {
	site := &Site{}

	for _, tc := range []struct {
		name string
		bat  *fakeBattery
		want float64
	}{
		{"half full, default max", &fakeBattery{soc: 50, capacity: 10, maxSoc: 100}, 5},
		{"near a configured max", &fakeBattery{soc: 80, capacity: 10, maxSoc: 90}, 1},
		{"already at max", &fakeBattery{soc: 100, capacity: 10, maxSoc: 100}, 0},
		{"above max clamps to zero, not negative", &fakeBattery{soc: 100, capacity: 10, maxSoc: 90}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := site.calculateBatteryDeficit(config.NewStaticDevice(config.Named{}, api.Meter(tc.bat)))
			assert.InDelta(t, tc.want, got, 1e-6)
		})
	}
}
