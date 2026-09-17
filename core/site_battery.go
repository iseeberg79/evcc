package core

import (
	"errors"
	"slices"
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

// allBatteriesHaveChargeCap reports whether every configured battery can have its charge
// power capped (BatteryChargePowerLimiter). It gates only the self-consumption holdcharge
// case in slotSuggestion, which needs every battery to follow a capped value: the resulting
// mode is dispatched site-wide (applyBatteryMode has no per-battery mode concept), so if any
// battery lacked the cap it would receive the same HoldCharge mode and, without a value push
// of its own, fall back to an unconditional 0 W block instead of the intended partial cap.
func (site *Site) allBatteriesHaveChargeCap() bool {
	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}
		if !api.HasCap[api.BatteryChargePowerLimiter](dev.Instance()) {
			return false
		}
	}

	return true
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

// fromTo reports whether the battery is entering, holding or leaving mode m:
// either m is requested, or nothing is requested and the battery is already in m.
func (site *Site) fromTo(requested, m api.BatteryMode) bool {
	return requested == m || requested == api.BatteryUnknown && site.batteryMode == m
}

func (site *Site) updateBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate) {
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive, rate)

	// put battery into hold mode when charging is active and HEMS dimmed
	if dimmed := hems.Dimmed(site.hems); site.fromTo(batteryMode, api.BatteryCharge) && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: HEMS dimmed")
		batteryMode = api.BatteryHold
	}

	// stop discharging to grid when HEMS curtailed production, but keep self-consumption
	if curtailed := hems.Curtailed(site.hems); site.fromTo(batteryMode, api.BatteryDischarge) && curtailed != nil && *curtailed {
		site.log.DEBUG.Println("battery mode: HEMS curtailed")
		batteryMode = api.BatteryNormal
	}

	// refresh each battery's charge values in its device-local cell before applying the
	// mode, so a control path consuming them (batterymode charge/holdcharge case) reads a
	// fresh value in the same cycle
	site.updateBatteryChargeValues()

	// NOTE: applyBatteryMode is always called when charge or discharge mode is active to
	// validate max soc / min soc reserve
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge || site.batteryMode == api.BatteryDischarge {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}
}

// requiredBatteryMode determines required battery mode based on grid charge/discharge and rate
func (site *Site) requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate) api.BatteryMode {
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

	// leaving the plan (or bridged into Hold for a smart-cost/fast charge session) must
	// not let a stale debounce window delay the next real suggestion once holdChargeMode
	// is back in charge
	if !site.holdChargePlanAvailable() || site.dischargeControlSessionActive(rate) {
		site.resetBatterySuggestionDebounce()
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
	case site.Automatic() && site.unmodelledCharging():
		// the suggestion ignores loads the optimizer cannot model as storage
		res = keepUnlessModified(api.BatteryHold)
	case batteryGridChargeActive:
		// independent limits (buy vs feed-in rate) can both be active at once;
		// charge wins to avoid buying and immediately selling
		if batteryGridDischargeActive && batMode != api.BatteryCharge {
			site.log.WARN.Println("battery mode: grid charge and grid discharge both active, charge takes priority")
		}
		res = keepUnlessModified(api.BatteryCharge)
	case site.dischargeControlActive(rate) || (batteryGridDischargeActive && site.evFastChargingActive()):
		// hold wins over feed-in discharge; fast charging holds even without
		// batteryDischargeControl, selling while an EV fast-charges is worse
		res = keepUnlessModified(api.BatteryHold)
	case batteryGridDischargeActive:
		res = keepUnlessModified(api.BatteryDischarge)
	case site.holdChargePlanAvailable():
		// follow the optimizer's current-slot plan
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

// unmodelledCharging reports a loadpoint charging at full power that the optimizer
// cannot model as storage (unknown vehicle capacity, see optimizerRequest). Its
// battery suggestion does not account for that load, so the battery must be held.
func (site *Site) unmodelledCharging() bool {
	for _, lp := range site.activeLoadpoints() {
		if v := lp.GetVehicle(); v != nil && v.Capacity() > 0 {
			continue
		}

		if lp.GetStatus() == api.StatusC && lp.IsFastChargingActive() {
			return true
		}
	}

	return false
}

// holdChargePlanAvailable reports whether a current-slot suggestion exists for any
// home battery; a stalled optimizer's suggestions are cleared by reapplySuggestions
// once its cached solve expires, see suggestionMaxAge.
func (site *Site) holdChargePlanAvailable() bool {
	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}
		if site.suggestion(batteryKey(dev.Config().Name), site.GetBatteryMode().String()) != nil {
			return true
		}
	}
	return false
}

// holdChargeMode collapses the plan's current-slot per-battery suggestions into the
// single global battery mode that applies to all home batteries. The optimizer already
// classifies each battery (normal/hold/charge/holdcharge), so we map that action
// directly instead of re-deriving it from raw charge power. On conflicting suggestions
// the peak-shaving intent wins: holdcharge > hold > charge > normal. Order-independent
// (holdcharge short-circuits; the rest only upgrade the priority).
//
// The result is debounced (see debounceBatterySuggestion): a degenerate solve near a
// very short slot can briefly suggest the wrong mode.
func (site *Site) holdChargeMode() api.BatteryMode {
	mode := api.BatteryNormal
loop:
	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}

		s := site.suggestion(batteryKey(dev.Config().Name), site.GetBatteryMode().String())
		if s == nil {
			continue
		}

		switch s.Action {
		case api.BatteryHoldCharge.String():
			mode = api.BatteryHoldCharge
			break loop
		case api.BatteryHold.String():
			if mode != api.BatteryHoldCharge {
				mode = api.BatteryHold
			}
		case api.BatteryCharge.String():
			if mode == api.BatteryNormal {
				mode = api.BatteryCharge
			}
		}
	}

	return site.debounceBatterySuggestion(mode)
}

// batterySuggestionDebounce delays adopting a changed suggestion until it has
// held for this long, filtering a single degenerate solve.
const batterySuggestionDebounce = 20 * time.Second

// resetBatterySuggestionDebounce discards the debounced mode, so the next
// suggestion is adopted immediately instead of held to a stale window - used
// whenever requiredBatteryMode isn't about to call holdChargeMode this cycle.
func (site *Site) resetBatterySuggestionDebounce() {
	site.Lock()
	defer site.Unlock()
	site.batterySuggestionConfirmed = api.BatteryUnknown
}

// debounceBatterySuggestion returns mode only once it has held for
// batterySuggestionDebounce, otherwise the last debounced mode.
func (site *Site) debounceBatterySuggestion(mode api.BatteryMode) api.BatteryMode {
	site.Lock()
	defer site.Unlock()

	now := time.Now()
	if mode != site.batterySuggestionPending {
		if site.batterySuggestionConfirmed != api.BatteryUnknown {
			site.log.DEBUG.Printf("battery suggestion: pending change to %s, confirmed %s", mode, site.batterySuggestionConfirmed)
		}
		site.batterySuggestionPending = mode
		site.batterySuggestionSince = now
	}

	if site.batterySuggestionConfirmed == api.BatteryUnknown || now.Sub(site.batterySuggestionSince) >= batterySuggestionDebounce {
		site.batterySuggestionConfirmed = site.batterySuggestionPending
	}

	return site.batterySuggestionConfirmed
}

// batterySocLimitReached reports whether the battery has reached the soc bound
// that should stop the requested mode: the max soc when charging, or the min
// soc reserve when discharging to grid. A configured limit of 0 disables the
// respective check (max is also disabled at 100).
func (site *Site) batterySocLimitReached(dev config.Device[api.Meter], discharge bool) (bool, error) {
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

	minSoc, maxSoc := batLimiter.GetSocLimits()

	if discharge {
		if minSoc > 0 && soc <= minSoc {
			site.log.DEBUG.Printf("battery %s: reserve soc reached (%.0f <= %.0f)", deviceTitleOrName(dev), soc, minSoc)
			return true, nil
		}
		return false, nil
	}

	if maxSoc > 0 && maxSoc < 100 && soc >= maxSoc {
		site.log.DEBUG.Printf("battery %s: limit soc reached (%.0f >= %.0f)", deviceTitleOrName(dev), soc, maxSoc)
		return true, nil
	}

	return false, nil
}

// holdChargeSuggestion returns the current-slot plan for the given home battery,
// or the zero value if none is available
func (site *Site) holdChargeSuggestion(name string) types.Suggestion {
	if s := site.suggestion(batteryKey(name), site.GetBatteryMode().String()); s != nil {
		return *s
	}
	return types.Suggestion{}
}

// applyBatteryMode applies the mode to each battery.
//
// A battery that reached the soc bound of the requested mode is held instead:
// the max soc when charging, the min soc reserve when discharging to grid. This
// is decided per device, so one battery reaching its bound does not force the
// others into hold.
func (site *Site) applyBatteryMode(mode api.BatteryMode) error {
	fromToCharge := site.fromTo(mode, api.BatteryCharge)
	fromToDischarge := site.fromTo(mode, api.BatteryDischarge)

	if site.batteryModeApplied == nil {
		site.batteryModeApplied = make(map[string]api.BatteryMode)
	}

	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		batCtrl, ok := api.Cap[api.BatteryController](meter)
		if !ok {
			continue
		}

		// per-device mode so one battery reaching its soc bound does not affect the others
		deviceMode := mode

		// hold at the soc bound of the requested mode (max soc for charge, min soc reserve for grid discharge)
		if (fromToCharge || fromToDischarge) && deviceMode != api.BatteryHold {
			hold, err := site.batterySocLimitReached(dev, fromToDischarge)
			if err != nil && !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
			if hold {
				deviceMode = api.BatteryHold
			}
		}

		// don't re-apply the mode the battery is already in
		name := dev.Config().Name
		if deviceMode == api.BatteryUnknown || deviceMode == site.batteryModeApplied[name] {
			continue
		}

		if !slices.Contains(batCtrl.BatteryModes(), deviceMode) {
			site.log.DEBUG.Printf("battery %s does not support mode: %s", deviceTitleOrName(dev), deviceMode)
			continue
		}

		if err := batCtrl.SetBatteryMode(deviceMode); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
			continue
		}

		site.batteryModeApplied[name] = deviceMode
		site.log.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), deviceMode)
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
// Charge power cap (holdcharge): the raw current-slot suggestion. Power setpoint (forced
// grid charge): the suggestion, or the battery's own reported max charge power when the
// optimizer has no current-slot value at all (no optimizer, or plan stale) - not merely
// because the current-slot suggestion is a deliberate 0 W (hold/holdcharge/normal).
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

		if setpointCtrl, ok := api.Cap[api.BatteryPowerSetpointController](instance); ok {
			watt := suggestion.Charge
			fallback := suggestion.Action == ""
			if fallback {
				if powerLimiter, ok := api.Cap[api.BatteryPowerLimiter](instance); ok {
					watt, _ = powerLimiter.GetPowerLimits()
				}
			}
			site.log.TRACE.Printf("battery %s power setpoint: %.0fW action=%q fallback=%v", deviceTitleOrName(dev), watt, suggestion.Action, fallback)
			if err := setpointCtrl.SetPowerSetpoint(watt); err != nil && !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power setpoint: %v", deviceTitleOrName(dev), err)
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

// batteryGridDischargeActive reports whether the feed-in rate has reached the
// grid discharge limit; the opt-in gates both this and the optimizer's planning
func (site *Site) batteryGridDischargeActive(rate api.Rate) bool {
	if !site.GetBatteryGridDischarge() {
		return false
	}

	limit := site.GetBatteryGridDischargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value >= *limit
}

// vehicleDone reports whether the vehicle's soc has reached its limit, without
// LimitSocReached's <100 exclusion (that guards against cutting a session short on soc
// rounding noise; here a false "done" only costs a little foregone self-consumption).
func (site *Site) vehicleDone(lp loadpoint.API) bool {
	soc := lp.GetSoc()
	return soc > 0 && soc >= float64(lp.EffectiveLimitSoc())
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}

	for _, lp := range site.activeLoadpoints() {
		smartCostActive := site.smartCostActive(lp, rate)
		if lp.GetStatus() == api.StatusC && !site.vehicleDone(lp) && (smartCostActive || lp.IsFastChargingActive()) {
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
			if !site.vehicleDone(lp) && (site.smartCostActive(lp, rate) || lp.IsFastChargingActive()) {
				return true
			}
		}
	}

	return false
}

// evFastChargingActive reports whether any loadpoint is fast charging,
// regardless of the batteryDischargeControl opt-in
func (site *Site) evFastChargingActive() bool {
	for _, lp := range site.activeLoadpoints() {
		if lp.GetStatus() == api.StatusC && lp.IsFastChargingActive() {
			return true
		}
	}

	return false
}
