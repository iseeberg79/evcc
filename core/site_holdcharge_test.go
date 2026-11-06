package core

import (
	"testing"

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

// the slot-boundary lookup itself is covered by TestOptimizerSchedule
// (core/optimizer_schedule_test.go) - optimizerSchedule.activeSlot is the
// production mechanism now, this file only exercises the site/battery layer
// built on top of it.
