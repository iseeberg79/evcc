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
// BatteryChargePowerLimiter.
type chargePowerLimiterMeter struct{ api.Meter }

func (chargePowerLimiterMeter) SetMaxChargePower(float64) error { return nil }

// TestAllBatteriesHaveChargeCap guards that allBatteriesHaveChargeCap() requires every
// configured battery to have BatteryChargePowerLimiter, not just one: the self-consumption
// holdcharge suggestion it gates is dispatched as one site-wide mode (applyBatteryMode has
// no per-battery mode concept), so a single battery lacking the cap would receive the same
// HoldCharge mode without a value push of its own and fall back to a hard 0 W block.
func TestAllBatteriesHaveChargeCap(t *testing.T) {
	newSite := func(bats ...api.Meter) *Site {
		devs := make([]config.Device[api.Meter], len(bats))
		for i, bat := range bats {
			devs[i] = config.NewStaticDevice[api.Meter](config.Named{}, bat)
		}
		return &Site{batteryMeters: devs}
	}

	require.True(t, newSite().allBatteriesHaveChargeCap(), "no batteries: vacuously true")
	require.True(t, newSite(chargePowerLimiterMeter{}).allBatteriesHaveChargeCap())
	require.False(t, newSite(&struct{ api.Meter }{}).allBatteriesHaveChargeCap(), "no cap capability")
	require.False(t, newSite(chargePowerLimiterMeter{}, &struct{ api.Meter }{}).allBatteriesHaveChargeCap(),
		"one battery without the cap must fail the check for all")
}

// TestRequiredBatteryModeIndependentOfCapability guards that the optimizer-follows-the-
// plan automation activates regardless of BatteryChargePowerLimiter/
// BatteryPowerSetpointController: hold/charge/holdcharge already work via
// api.BatteryController alone on upstream device templates (holdcharge as an
// unconditional 0 W block), so requiring either push capability here would silently
// disable the automation for every battery template that doesn't implement them.
func TestRequiredBatteryModeIndependentOfCapability(t *testing.T) {
	site := &Site{
		batteryMeters:  []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{}, &struct{ api.Meter }{})},
		holdChargePlan: singleSlotHoldChargePlan(time.Now(), map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}}),
	}

	got := site.requiredBatteryMode(false, api.Rate{})
	require.Equal(t, api.BatteryHoldCharge.String(), got.String())
}

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
		site := &Site{holdChargePlan: singleSlotHoldChargePlan(time.Now(), tc.suggestions)}
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
			holdChargePlan:          singleSlotHoldChargePlan(time.Now(), map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}}),
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

	site.holdChargePlan = singleSlotHoldChargePlan(time.Now(), map[string]types.Suggestion{"b": {Charge: 1000}})
	require.True(t, site.holdChargePlanAvailable(), "fresh plan")

	site.holdChargePlan.updated = time.Now().Add(-2 * holdChargeStale)
	require.False(t, site.holdChargePlanAvailable(), "stale plan")
}

// TestHoldChargePlanSlotLookup guards the actual bug this design fixes: a multi-slot
// plan retains a suggestion per slot, so looking it up by wall-clock time resolves to
// the slot that actually covers "now" instead of the frozen slot 0 it was built with -
// even though the plan itself (its "updated" timestamp) may be many minutes old.
func TestHoldChargePlanSlotLookup(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 42, 0, 0, time.UTC)
	plan := &holdChargePlan{
		updated: base,
		starts:  []time.Time{base, base.Add(3 * time.Minute), base.Add(18 * time.Minute)},
		ends:    []time.Time{base.Add(3 * time.Minute), base.Add(18 * time.Minute), base.Add(33 * time.Minute)},
		slots: []map[string]types.Suggestion{
			{"b": {Action: "normal"}},
			{"b": {Action: "holdcharge"}},
			{"b": {Action: "hold"}},
		},
	}

	require.Equal(t, "normal", plan.suggestions(base)["b"].Action, "at run time: slot 0")
	require.Equal(t, "holdcharge", plan.suggestions(base.Add(10 * time.Minute))["b"].Action, "10:52, within slot 1's window: slot 1, not the frozen slot 0")
	require.Equal(t, "hold", plan.suggestions(base.Add(20 * time.Minute))["b"].Action, "11:02, within slot 2's window: slot 2")
	require.Nil(t, plan.suggestions(base.Add(40*time.Minute)), "beyond the plan's horizon: no slot matches")
}
