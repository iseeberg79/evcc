package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/stretchr/testify/require"
)

// buildHoldChargeSite creates a site with one home battery "home" and a synthetic
// optimizer result with the given per-slot charging plan (Wh per 15min slot).
// "now" sits inside slot 0.
func buildHoldChargeSite(enabled bool, minPower float64, charging []float32, withResult bool) (*Site, chan util.Param) {
	now := time.Now()
	const slots = 8
	dt := make([]int, slots)
	ts := make([]time.Time, slots)
	for i := range slots {
		dt[i] = 900 // 15 min
		ts[i] = now.Add(-time.Minute).Add(time.Duration(i) * 15 * time.Minute)
	}

	ch := make(chan util.Param, 16)
	s := &Site{
		log:           util.NewLogger("test"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "home"}, nil)},
	}
	s.valueChan = ch
	s.batteryAutoHoldCharge = enabled
	s.batteryAutoHoldChargeMinPower = minPower

	if withResult {
		s.lastOptimizerResult = &optimizerResult{
			Req: optimizer.OptimizationInput{TimeSeries: optimizer.TimeSeries{Dt: dt}},
			Res: optimizer.OptimizationResult{
				Batteries: []optimizer.BatteryResult{{ChargingPower: charging}},
			},
			Details: requestDetails{
				Timestamps:     ts,
				BatteryDetails: []batteryDetail{{Type: batteryTypeBattery, Name: "home"}},
			},
		}
	}

	return s, ch
}

// lastHoldChargePower drains the channel and returns the last published power array.
func lastHoldChargePower(ch chan util.Param) []int64 {
	var res []int64
	for {
		select {
		case p := <-ch:
			if p.Key == keys.BatteryHoldChargePower {
				res = p.Val.([]int64)
			}
		default:
			return res
		}
	}
}

func TestApplyHoldChargePower(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		minPower   float64
		charging   []float32 // Wh per slot; nil means no optimizer result
		wantActive bool
		wantPowers []int64
	}{
		{
			// feature off: never active, publishes zeros
			name: "disabled", enabled: false, minPower: 200,
			charging:   []float32{1000, 1000, 1000, 1000, 1000, 1000, 1000, 1000},
			wantActive: false, wantPowers: []int64{0},
		},
		{
			// no optimizer result yet: never active, publishes zeros
			name: "no result", enabled: true, minPower: 200, charging: nil,
			wantActive: false, wantPowers: []int64{0},
		},
		{
			// charging now: 1000 Wh / 0.25 h = 4000 W limit, active
			name: "charging now", enabled: true, minPower: 200,
			charging:   []float32{1000, 0, 0, 0, 0, 0, 0, 0},
			wantActive: true, wantPowers: []int64{4000},
		},
		{
			// morning delay: 0 now but midday plan >= threshold -> limit 0, still active
			name: "morning delay", enabled: true, minPower: 200,
			charging:   []float32{0, 0, 0, 1000, 1000, 0, 0, 0},
			wantActive: true, wantPowers: []int64{0},
		},
		{
			// evening / bad weather: nothing planned -> inactive (free run)
			name: "free run", enabled: true, minPower: 200,
			charging:   []float32{0, 0, 0, 0, 0, 0, 0, 0},
			wantActive: false, wantPowers: []int64{0},
		},
		{
			// below threshold: 40 Wh -> 160 W < 200 -> zeroed, inactive
			name: "below threshold", enabled: true, minPower: 200,
			charging:   []float32{40, 40, 40, 40, 40, 40, 40, 40},
			wantActive: false, wantPowers: []int64{0},
		},
		{
			// minPower 0 falls back to default 200: 30 Wh -> 120 W < 200 -> inactive
			name: "default threshold applied", enabled: true, minPower: 0,
			charging:   []float32{30, 30, 30, 30, 30, 30, 30, 30},
			wantActive: false, wantPowers: []int64{0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ch := buildHoldChargeSite(tc.enabled, tc.minPower, tc.charging, tc.charging != nil)

			active := s.applyHoldChargePower()
			require.Equal(t, tc.wantActive, active, "active")
			require.Equal(t, tc.wantPowers, lastHoldChargePower(ch), "published powers")
		})
	}
}
