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
	enableAutomatic(t)

	site := &Site{
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{})},
	}
	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}})

	got := site.requiredBatteryMode(false, false, api.Rate{})
	require.Equal(t, api.BatteryHoldCharge.String(), got.String())
}

// TestRequiredBatteryModeIgnoresPlanWithoutAutomatic guards that suggestions stay advisory
// while the optimizer is not automatic: the plan must not drive the battery mode.
func TestRequiredBatteryModeIgnoresPlanWithoutAutomatic(t *testing.T) {
	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{})},
	}
	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}})

	require.Equal(t, api.BatteryUnknown.String(), site.requiredBatteryMode(false, false, api.Rate{}).String())
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
		devs := make([]config.Device[api.Meter], 0, len(tc.suggestions))
		for name := range tc.suggestions {
			devs = append(devs, config.NewStaticDevice[api.Meter](config.Named{Name: name}, &struct{ api.Meter }{}))
		}
		site := &Site{batteryMeters: devs}
		setBatterySuggestions(site, tc.suggestions)
		require.Equal(t, tc.want, site.holdChargeMode(), tc.name)
	}
}

// TestHoldChargeModeDebounce guards the fix for a degenerate solve near a very
// short slot briefly suggesting the wrong mode (previously only caught by an
// ad-hoc trace-logging patch, see evcc_slot0_findings.md): a single flip must
// not surface, a change that persists past batterySuggestionDebounce must.
func TestHoldChargeModeDebounce(t *testing.T) {
	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{})},
	}

	setPlan := func(action string) {
		setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: action}})
	}

	setPlan(api.BatteryHold.String())
	require.Equal(t, api.BatteryHold, site.holdChargeMode(), "first suggestion is adopted immediately")

	setPlan(api.BatteryNormal.String())
	require.Equal(t, api.BatteryHold, site.holdChargeMode(), "single flip is filtered")

	setPlan(api.BatteryHold.String())
	require.Equal(t, api.BatteryHold, site.holdChargeMode(), "reverting before the debounce elapses leaves no trace")

	setPlan(api.BatteryCharge.String())
	require.Equal(t, api.BatteryHold, site.holdChargeMode(), "not yet debounced")

	site.batterySuggestionSince = time.Now().Add(-batterySuggestionDebounce - time.Second)
	require.Equal(t, api.BatteryCharge, site.holdChargeMode(), "adopted once it has held long enough")
}

// TestHoldChargeModeDebounceResetsWhenPlanUnavailable guards that leaving the
// plan (optimizer stalled, or requiredBatteryMode takes a different branch)
// does not let a stale debounce window delay the next real suggestion.
func TestHoldChargeModeDebounceResetsWhenPlanUnavailable(t *testing.T) {
	enableAutomatic(t)

	site := &Site{log: util.NewLogger("foo"), batteryMeters: []config.Device[api.Meter]{
		config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{}),
	}}

	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryCharge.String()}})
	require.Equal(t, api.BatteryCharge, site.requiredBatteryMode(false, false, api.Rate{}))
	site.batteryMode = api.BatteryCharge // simulate updateBatteryMode having applied it

	// plan gone (stalled optimizer): requiredBatteryMode releases the battery
	// and must reset the debounce, not just leave it unfed
	site.setSuggestions(nil)
	require.Equal(t, api.BatteryNormal, site.requiredBatteryMode(false, false, api.Rate{}))
	site.batteryMode = api.BatteryNormal

	// a fresh, different suggestion is adopted immediately, not held to the
	// debounce window left over from before the stall
	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHold.String()}})
	require.Equal(t, api.BatteryHold, site.requiredBatteryMode(false, false, api.Rate{}))
}

// TestHoldChargeModeDebounceFiltersRealDegenerateSolve replays the exact
// mode transition of a captured optimizer response (dt[0]=1s, right at a
// 15min boundary): slotSuggestion on that slot alone does flip to normal
// (charging_power=0.0068433, no grid flow), the very next slot's suggestion
// was hold (matches what was actually observed live).
func TestHoldChargeModeDebounceFiltersRealDegenerateSolve(t *testing.T) {
	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{})},
	}

	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHold.String()}})
	require.Equal(t, api.BatteryHold, site.holdChargeMode())

	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryNormal.String()}})
	require.Equal(t, api.BatteryHold, site.holdChargeMode(), "the one-off normal must not surface")

	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHold.String()}})
	require.Equal(t, api.BatteryHold, site.holdChargeMode())
}

func TestHoldChargePlanAvailable(t *testing.T) {
	site := &Site{batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{})}}
	require.False(t, site.holdChargePlanAvailable(), "no plan")

	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Charge: 1000}})
	require.True(t, site.holdChargePlanAvailable(), "fresh plan")

	site.clearSuggestions()
	require.False(t, site.holdChargePlanAvailable(), "stale plan")
}

// the slot-boundary lookup itself is covered by TestOptimizerSchedule
// (core/optimizer_schedule_test.go) - optimizerSchedule.activeSlot is the
// production mechanism now, this file only exercises the site/battery layer
// built on top of it.
