package core

import (
	"errors"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/util/config"
)

func batteryModeModified(mode api.BatteryMode) bool {
	return mode != api.BatteryUnknown && mode != api.BatteryNormal
}

func (site *Site) batteryConfigured() bool {
	return len(site.batteryMeters) > 0
}

func (site *Site) hasBatteryControl() bool {
	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		if api.HasCap[api.BatteryController](meter) {
			return true
		}
	}

	return false
}

// hasBatteryChargeControl reports whether any configured battery can have its charge power
// capped or set to a target (BatteryChargePowerLimiter/BatteryChargeSetpointController).
// This is the opt-in for following the optimizer's per-slot plan automatically: only a
// battery whose template actually implements one of these capabilities can follow a partial
// planned charge value, so the capability itself gates the automation instead of a separate
// user-facing setting.
func (site *Site) hasBatteryChargeControl() bool {
	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}
		meter := dev.Instance()

		if api.HasCap[api.BatteryChargePowerLimiter](meter) || api.HasCap[api.BatteryChargeSetpointController](meter) {
			return true
		}
	}

	return false
}

// setBatteryMode sets the battery mode
func (site *Site) setBatteryMode(batMode api.BatteryMode) {
	site.batteryMode = batMode
	site.publish(keys.BatteryMode, batMode)
}

// SetBatteryMode sets the battery mode
func (site *Site) SetBatteryMode(batMode api.BatteryMode) {
	site.Lock()
	defer site.Unlock()

	site.log.DEBUG.Println("set battery mode:", batMode)

	if site.batteryMode != batMode {
		site.setBatteryMode(batMode)
	}

	if site.batteryModeExternal == api.BatteryUnknown {
		site.batteryModeExternalTimer = time.Time{}
	}
}

func (site *Site) updateBatteryMode(batteryGridChargeActive bool, rate api.Rate) {
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, rate)

	// battery estimator: auto-enable HoldCharge to spread PV charging power over time,
	// independent of the optimizer (only from Unknown or Normal - it must not override an
	// externally or grid-charge/discharge-controlled mode)
	if (batteryMode == api.BatteryUnknown || batteryMode == api.BatteryNormal) && site.shouldUseBatteryEstimator() {
		batteryMode = api.BatteryHoldCharge
	}
	// release the estimator's HoldCharge once the battery is full and cutoff is close, so it
	// can discharge freely instead of sitting needlessly capped
	if site.batteryEstimator && batteryMode == api.BatteryHoldCharge && site.isEstimatorBufferTimeActiveAndBatteryFull() {
		batteryMode = api.BatteryNormal
	}
	// keep the estimator's per-battery hold charge power (Reg 1038 etc.) current whenever
	// it may be applied
	if site.batteryEstimator && (batteryMode == api.BatteryHoldCharge || site.batteryMode == api.BatteryHoldCharge) {
		site.updateBatteryEstimatorSuggestions()
	}

	// put battery into hold mode when charging is active and HEMS dimmed
	fromToCharge := batteryMode == api.BatteryCharge || batteryMode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge
	if dimmed := hems.Dimmed(site.hems); fromToCharge && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: HEMS dimmed")
		batteryMode = api.BatteryHold
	}

	// refresh each battery's charge values in its device-local cell before applying the
	// mode, so a control path consuming them (batterymode charge/holdcharge case) reads a
	// fresh value in the same cycle
	site.updateBatteryChargeValues()

	// NOTE: applyBatteryMode is always called when charge mode is active to validate max soc
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}
}

// requiredBatteryMode determines required battery mode based on grid charge and rate
func (site *Site) requiredBatteryMode(batteryGridChargeActive bool, rate api.Rate) api.BatteryMode {
	var res api.BatteryMode
	batMode := site.GetBatteryMode()
	extMode := site.GetBatteryModeExternal()

	var extModeReset bool
	if extMode == api.BatteryUnknown {
		site.Lock()
		extModeReset = !site.batteryModeExternalTimer.IsZero()
		site.Unlock()
	}

	keepUnlessModified := func(s api.BatteryMode) api.BatteryMode {
		return map[bool]api.BatteryMode{false: s, true: api.BatteryUnknown}[batMode == s]
	}

	switch {
	case !site.batteryConfigured():
		res = api.BatteryUnknown
	case extModeReset:
		// require normal mode to leave external control
		res = api.BatteryNormal
	case extMode != api.BatteryUnknown:
		// require external mode only once
		if extMode != batMode {
			res = extMode
		}
	case batteryGridChargeActive:
		res = keepUnlessModified(api.BatteryCharge)
	case site.dischargeControlActive(rate):
		res = keepUnlessModified(api.BatteryHold)
	case !site.batteryEstimator && site.hasBatteryChargeControl() && site.holdChargePlanAvailable():
		// follow the optimizer's current-slot plan; disabled while the battery estimator is
		// active since the two hold charge sources must not fight over the same battery mode.
		// gated on hasBatteryChargeControl() instead of a user-facing toggle: only a battery
		// whose template implements the charge power/setpoint capability can actually follow
		// a partial planned value, so the capability itself is the opt-in.
		if site.dischargeControlSessionActive(rate) {
			// Hold wins over HoldCharge for the whole smart-cost/fast charge session: the
			// case above already yields Hold via dischargeControlActive while the vehicle
			// draws power, but that signal's StatusC gate drops out on brief status blips
			// (PWM pause, phase switch, handshake retry). Bridging the session here keeps
			// Hold instead of flickering to the more disruptive HoldCharge, which would
			// also block the vehicle's charging.
			res = keepUnlessModified(api.BatteryHold)
		} else {
			res = keepUnlessModified(site.holdChargeMode())
		}
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
}

// holdChargeStale bounds how long a stored plan is trusted, regardless of whether its
// slots still cover "now" - the plan's inputs (SoC, PV, prices) age independently of
// slot coverage.
const holdChargeStale = 30 * time.Minute

// holdChargePlan is an immutable per-slot snapshot of each home battery's planned
// action (from the optimizer or the battery estimator), keyed by battery name.
// slotSuggestion only depends on plan data, never on live measurements, so the full
// horizon is computed once per run instead of being
// re-derived from a single frozen "slot 0" on every site cycle - which drifted behind
// the wall clock the longer the cached plan was reused (see FORK_CHANGES_evcc.md).
// The producer (optimizerUpdate/updateBatteryEstimatorSuggestions) swaps the pointer
// under site.Lock; consumers read it once under RLock, so a run landing mid-cycle can
// never mix mode and charge value from two different plans.
type holdChargePlan struct {
	updated time.Time
	starts  []time.Time
	ends    []time.Time
	slots   []map[string]types.Suggestion
}

// singleSlotHoldChargePlan wraps a one-off suggestion in the same plan shape the
// optimizer produces, for the battery estimator, which recomputes live every cycle
// instead of solving a multi-slot horizon.
func singleSlotHoldChargePlan(now time.Time, suggestions map[string]types.Suggestion) *holdChargePlan {
	return &holdChargePlan{
		updated: now,
		starts:  []time.Time{now},
		ends:    []time.Time{now.Add(holdChargeStale)},
		slots:   []map[string]types.Suggestion{suggestions},
	}
}

// suggestions returns the plan's slot covering now, or nil if none does.
func (p *holdChargePlan) suggestions(now time.Time) map[string]types.Suggestion {
	if p == nil {
		return nil
	}
	for i, start := range p.starts {
		if !now.Before(start) && now.Before(p.ends[i]) {
			return p.slots[i]
		}
	}
	return nil
}

// holdChargePlanAvailable reports whether a recent plan exists with a slot for now
func (site *Site) holdChargePlanAvailable() bool {
	site.RLock()
	defer site.RUnlock()
	if site.holdChargePlan == nil || time.Since(site.holdChargePlan.updated) >= holdChargeStale {
		return false
	}
	return len(site.holdChargePlan.suggestions(time.Now())) > 0
}

// holdChargeMode collapses the plan's current-slot per-battery suggestions into the
// single global battery mode that applies to all home batteries. The optimizer already
// classifies each battery (normal/hold/charge/holdcharge), so we map that action
// directly instead of re-deriving it from raw charge power. On conflicting suggestions
// the peak-shaving intent wins: holdcharge > hold > charge > normal. Order-independent
// (holdcharge short-circuits; the rest only upgrade the priority).
func (site *Site) holdChargeMode() api.BatteryMode {
	site.RLock()
	defer site.RUnlock()

	mode := api.BatteryNormal
	for _, s := range site.holdChargePlan.suggestions(time.Now()) {
		switch s.Action {
		case "holdcharge":
			return api.BatteryHoldCharge
		case "hold":
			if mode != api.BatteryHoldCharge {
				mode = api.BatteryHold
			}
		case "charge":
			if mode == api.BatteryNormal {
				mode = api.BatteryCharge
			}
		}
	}
	return mode
}

// batteryMaxSocReached checks is battery has exceed max soc limit
func (site *Site) batteryMaxSocReached(dev config.Device[api.Meter]) (bool, error) {
	meter := dev.Instance()

	batLimiter, ok := api.Cap[api.BatterySocLimiter](meter)
	if !ok {
		return false, nil
	}

	batSoc, ok := api.Cap[api.Battery](meter)
	if !ok {
		return false, errors.New("battery with soc limits must have soc")
	}

	soc, err := batSoc.Soc()
	if err != nil {
		return false, err
	}

	if _, max := batLimiter.GetSocLimits(); max > 0 && max < 100 && soc >= max {
		site.log.DEBUG.Printf("battery %s: limit soc reached (%.0f > %.0f)", deviceTitleOrName(dev), soc, max)
		return true, nil
	}

	return false, nil
}

// holdChargeSuggestion returns the current-slot plan for the given home battery,
// or the zero value if none is available
func (site *Site) holdChargeSuggestion(name string) types.Suggestion {
	site.RLock()
	defer site.RUnlock()
	return site.holdChargePlan.suggestions(time.Now())[name]
}

// applyBatteryMode applies the mode to each battery
//
// api.BatteryCharge:
//
//	The current soc is validated against max soc.
//	In case max soc is reached, hold mode is applied.
func (site *Site) applyBatteryMode(mode api.BatteryMode) error {
	fromToCharge := mode == api.BatteryCharge || mode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge

	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		batCtrl, ok := api.Cap[api.BatteryController](meter)
		if !ok {
			continue
		}

		// validate max soc
		if fromToCharge && mode != api.BatteryHold {
			ok, err := site.batteryMaxSocReached(dev)
			if err != nil && !errors.Is(err, api.ErrNotAvailable) {
				return err
			}

			// put battery into hold mode when soc limit reached
			if ok {
				// TODO do this only once
				mode = api.BatteryHold
			}
		}

		if mode != api.BatteryUnknown {
			if err := batCtrl.SetBatteryMode(mode); err == nil {
				site.log.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), mode)
			} else if !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
		}
	}

	return nil
}

// updateBatteryChargeValues pushes the optimizer's current-slot charge values into each
// battery's device-local cell via typed capabilities. The values are consumed by the
// device's own batterymode control path (charge / holdcharge case) and therefore ride the
// mode's watchdog/reset lifecycle - on leaving the mode that path stops writing them, so
// there is no separate lifecycle to unwind here. Pushing unconditionally (not gated on the
// current mode) keeps the cell fresh, so the consuming case reads the right value the
// moment the mode applies.
//
// Charge power cap (holdcharge): the raw current-slot suggestion. Charge setpoint (forced
// grid charge): the suggestion, or the battery's own reported max charge power when the
// optimizer has no current-slot value at all (no optimizer, or plan stale) - not merely
// because the current-slot suggestion is a deliberate 0 W (hold/holdcharge/normal).
//
// TODO: guard the pushed setpoint against an active circuit/load-management limit once
// that constraint is modelled, so forced grid-charging cannot exceed the site's headroom.
func (site *Site) updateBatteryChargeValues() {
	for _, dev := range site.batteryMeters {
		instance := dev.Instance()
		suggestion := site.holdChargeSuggestion(dev.Config().Name)

		if powerLimiter, ok := api.Cap[api.BatteryChargePowerLimiter](instance); ok {
			site.log.TRACE.Printf("battery %s max charge power: %.0fW action=%q", deviceTitleOrName(dev), suggestion.Charge, suggestion.Action)
			if err := powerLimiter.SetMaxChargePower(suggestion.Charge); err != nil && !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s max charge power: %v", deviceTitleOrName(dev), err)
			}
		}

		if setpointCtrl, ok := api.Cap[api.BatteryChargeSetpointController](instance); ok {
			watt := suggestion.Charge
			fallback := suggestion.Action == ""
			if fallback {
				if powerLimiter, ok := api.Cap[api.BatteryPowerLimiter](instance); ok {
					watt, _ = powerLimiter.GetPowerLimits()
				}
			}
			site.log.TRACE.Printf("battery %s charge setpoint: %.0fW action=%q fallback=%v", deviceTitleOrName(dev), watt, suggestion.Action, fallback)
			if err := setpointCtrl.SetChargeSetpoint(watt); err != nil && !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s charge setpoint: %v", deviceTitleOrName(dev), err)
			}
		}
	}
}

func (site *Site) tariffRates(usage api.TariffUsage) (api.Rates, error) {
	tariff := site.GetTariff(usage)
	if tariff == nil || tariff.Type() == api.TariffTypePriceStatic {
		return nil, nil
	}

	return tariff.Rates()
}

func (site *Site) smartCostActive(lp loadpoint.API, rate api.Rate) bool {
	limit := lp.GetSmartCostLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) batteryGridChargeActive(rate api.Rate) bool {
	limit := site.GetBatteryGridChargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}

	for _, lp := range site.Loadpoints() {
		smartCostActive := site.smartCostActive(lp, rate)
		if lp.GetStatus() == api.StatusC && (smartCostActive || lp.IsFastChargingActive()) {
			return true
		}
	}

	return false
}

// dischargeControlSessionActive mirrors dischargeControlActive but is tolerant of brief
// loadpoint status gaps: it gates on a connected vehicle (StatusB or StatusC) instead of
// active charging (StatusC only). During a smart-cost/fast charge the status drops from C
// to B on normal blips (PWM pause, phase switch, handshake retry) while the session keeps
// running; StatusB only clears on an actual unplug. It is used to keep Hold winning over
// the fork's HoldCharge for the duration of the session, so such a blip does not flip Hold
// to the more disruptive HoldCharge (which would also block the vehicle's charging) for a
// single update cycle.
func (site *Site) dischargeControlSessionActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}

	for _, lp := range site.Loadpoints() {
		if status := lp.GetStatus(); status == api.StatusB || status == api.StatusC {
			if site.smartCostActive(lp, rate) || lp.IsFastChargingActive() {
				return true
			}
		}
	}

	return false
}
