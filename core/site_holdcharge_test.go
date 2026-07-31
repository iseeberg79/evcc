package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/require"
)

// chargePowerLimiterMeter is a minimal api.Meter that also implements
// BatteryChargePowerLimiter, so hasBatteryChargeControl() finds it - the capability is the
// opt-in for following the optimizer's plan automatically (see requiredBatteryMode()).
type chargePowerLimiterMeter struct{ api.Meter }

func (chargePowerLimiterMeter) SetMaxChargePower(float64) error { return nil }

func TestHoldChargeMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		suggestions map[string]types.Suggestion
		want        api.BatteryMode
	}{
		{"holdcharge action", map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}}, api.BatteryHoldCharge},
		{"hold action", map[string]types.Suggestion{"b": {Action: api.BatteryHold.String()}}, api.BatteryHold},
		{"charge action", map[string]types.Suggestion{"b": {Action: api.BatteryCharge.String()}}, api.BatteryCharge},
		{"normal action", map[string]types.Suggestion{"b": {Action: api.BatteryNormal.String()}}, api.BatteryNormal},
		{"empty action (default)", map[string]types.Suggestion{"b": {}}, api.BatteryNormal},
		{"holdcharge wins over charge", map[string]types.Suggestion{"a": {Action: api.BatteryCharge.String()}, "b": {Action: api.BatteryHoldCharge.String()}}, api.BatteryHoldCharge},
		{"hold wins over charge", map[string]types.Suggestion{"a": {Action: api.BatteryCharge.String()}, "b": {Action: api.BatteryHold.String()}}, api.BatteryHold},
	} {
		site := &Site{holdChargeSuggestions: tc.suggestions}
		require.Equal(t, tc.want, site.holdChargeMode(), tc.name)
	}
}

// TestHoldChargeYieldsToSmartChargeSession guards that Hold wins over the fork's
// HoldCharge for the whole duration of a smart-cost/fast charge session, even when the
// loadpoint status briefly drops from C to B (PWM pause, phase switch, handshake retry).
// Without the blip-tolerant session signal the switch would flicker to HoldCharge (which
// also blocks the vehicle's charging) for that single update cycle.
func TestHoldChargeYieldsToSmartChargeSession(t *testing.T) {
	limit := 0.20
	cheap := api.Rate{Value: 0.10}  // <= limit -> smart cost active
	pricey := api.Rate{Value: 0.50} // > limit  -> smart cost inactive

	newLoadpoint := func(status api.ChargeStatus, smartCostLimit *float64) *Loadpoint {
		return &Loadpoint{
			log:            util.NewLogger("lp"),
			mode:           api.ModePV,
			status:         status,
			smartCostLimit: smartCostLimit,
		}
	}

	for _, tc := range []struct {
		name    string
		status  api.ChargeStatus
		rate    api.Rate
		batMode api.BatteryMode
		want    api.BatteryMode
	}{
		{"charging: dischargeControlActive yields Hold", api.StatusC, cheap, api.BatteryUnknown, api.BatteryHold},
		{"blip to StatusB during session: Hold still wins over HoldCharge", api.StatusB, cheap, api.BatteryUnknown, api.BatteryHold},
		{"blip to StatusB while already holding: keep Hold, no flicker", api.StatusB, cheap, api.BatteryHold, api.BatteryUnknown},
		{"unplugged: session over, follow HoldCharge plan", api.StatusA, cheap, api.BatteryUnknown, api.BatteryHoldCharge},
		{"unplugged while holding: transition to HoldCharge", api.StatusA, cheap, api.BatteryHold, api.BatteryHoldCharge},
		{"connected but no smart-cost window: follow HoldCharge plan", api.StatusB, pricey, api.BatteryUnknown, api.BatteryHoldCharge},
	} {
		site := &Site{
			log:                     util.NewLogger("site"),
			batteryMeters:           []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{}, chargePowerLimiterMeter{})},
			batteryDischargeControl: true,
			holdChargeSuggestions:   map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}},
			holdChargeUpdated:       time.Now(),
			loadpoints:              []*Loadpoint{newLoadpoint(tc.status, &limit)},
		}
		site.batteryMode = tc.batMode

		got := site.requiredBatteryMode(false, tc.rate)
		require.Equal(t, tc.want.String(), got.String(), tc.name)
	}
}

func TestHoldChargePlanAvailable(t *testing.T) {
	site := &Site{}
	require.False(t, site.holdChargePlanAvailable(), "no plan")

	site.holdChargeSuggestions = map[string]types.Suggestion{"b": {Charge: 1000}}
	site.holdChargeUpdated = time.Now()
	require.True(t, site.holdChargePlanAvailable(), "fresh plan")

	site.holdChargeUpdated = time.Now().Add(-2 * holdChargeStale)
	require.False(t, site.holdChargePlanAvailable(), "stale plan")
}
