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
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{})},
	}
	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}})

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
	site := &Site{log: util.NewLogger("foo"), batteryMeters: []config.Device[api.Meter]{
		config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{}),
	}}

	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryCharge.String()}})
	require.Equal(t, api.BatteryCharge, site.requiredBatteryMode(false, api.Rate{}))
	site.batteryMode = api.BatteryCharge // simulate updateBatteryMode having applied it

	// plan gone (stalled optimizer): requiredBatteryMode releases the battery
	// and must reset the debounce, not just leave it unfed
	site.setSuggestions(nil)
	require.Equal(t, api.BatteryNormal, site.requiredBatteryMode(false, api.Rate{}))
	site.batteryMode = api.BatteryNormal

	// a fresh, different suggestion is adopted immediately, not held to the
	// debounce window left over from before the stall
	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHold.String()}})
	require.Equal(t, api.BatteryHold, site.requiredBatteryMode(false, api.Rate{}))
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
			batteryMeters:           []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, chargePowerLimiterMeter{})},
			batteryDischargeControl: true,
			loadpoints:              []*Loadpoint{newLoadpoint(tc.status, &limit)},
		}
		setBatterySuggestions(site, map[string]types.Suggestion{"b": {Action: api.BatteryHoldCharge.String()}})
		site.batteryMode = tc.batMode

		got := site.requiredBatteryMode(false, tc.rate)
		require.Equal(t, tc.want.String(), got.String(), tc.name)
	}
}

func TestHoldChargePlanAvailable(t *testing.T) {
	site := &Site{batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice[api.Meter](config.Named{Name: "b"}, &struct{ api.Meter }{})}}
	require.False(t, site.holdChargePlanAvailable(), "no plan")

	setBatterySuggestions(site, map[string]types.Suggestion{"b": {Charge: 1000}})
	require.True(t, site.holdChargePlanAvailable(), "fresh plan")

	site.suggestionsUpdated = time.Now().Add(-2 * suggestionMaxAge)
	require.False(t, site.holdChargePlanAvailable(), "stale plan")
}

// TestActiveSlotLookup guards the actual bug this design fixes: a multi-slot plan
// retains a boundary per slot, so looking it up by wall-clock time resolves to the
// slot that actually covers "now" instead of the frozen slot 0 it was built with.
func TestActiveSlotLookup(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 42, 0, 0, time.UTC)
	starts := []time.Time{base, base.Add(3 * time.Minute), base.Add(18 * time.Minute)}
	ends := []time.Time{base.Add(3 * time.Minute), base.Add(18 * time.Minute), base.Add(33 * time.Minute)}

	require.Equal(t, 0, activeSlot(starts, ends, base), "at run time: slot 0")
	require.Equal(t, 1, activeSlot(starts, ends, base.Add(10*time.Minute)), "10:52, within slot 1's window: slot 1, not the frozen slot 0")
	require.Equal(t, 2, activeSlot(starts, ends, base.Add(20*time.Minute)), "11:02, within slot 2's window: slot 2")
	require.Equal(t, -1, activeSlot(starts, ends, base.Add(40*time.Minute)), "beyond the plan's horizon: no slot matches")
}
