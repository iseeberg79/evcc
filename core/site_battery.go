package core

import (
	"errors"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
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

// solarCutoffTime finds the first point in rates where solar drops below 50W.
// If the matching rate is already running, cutoff is now. If no such point exists,
// the end of the last rate is used.
func solarCutoffTime(rates api.Rates, now time.Time) time.Time {
	for _, r := range rates {
		if r.End.After(now) && r.Value < 50 {
			if r.Start.Before(now) {
				return now
			}
			return r.Start
		}
	}
	return rates[len(rates)-1].End
}

// holdChargeTargetTime parses the configured target time and returns it as today's time.Time.
// Returns zero if the target time has already passed or is not set (falls back to "18:00").
func (site *Site) holdChargeTargetTime() time.Time {
	s := site.batteryAutoHoldChargeTargetTime
	if s == "" {
		s = "18:00"
	}
	t, err := time.ParseInLocation("15:04", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
	if target.Before(now) {
		return time.Time{} // already past today's target - no constraint
	}
	return target
}

// effectiveCutoffTime returns the earlier of the solar cutoff and the configured target time.
// This ensures the battery reaches maxSoC by the target time even if the sun sets later.
func (site *Site) effectiveCutoffTime(rates api.Rates) time.Time {
	now := time.Now()
	solar := solarCutoffTime(rates, now)
	target := site.holdChargeTargetTime()
	if !target.IsZero() && target.Before(solar) {
		return target
	}
	return solar
}

func (site *Site) isBufferTimeActiveAndBatteryFull() bool {
	// disable HoldCharge when buffer time is active and battery is full
	// this allows free discharge of fully charged battery during buffer time

	// check battery soc against maxSoC limit (with 0.5% tolerance)
	// get maxSoC from first battery's limiter, fallback to 100%
	maxSoC := 100.0
	if len(site.batteryMeters) > 0 {
		meter := site.batteryMeters[0].Instance()
		if batLimiter, ok := api.Cap[api.BatterySocLimiter](meter); ok {
			if _, max := batLimiter.GetSocLimits(); max > 0 && max < 100 {
				maxSoC = float64(max)
			}
		}
	}
	if site.battery.Soc < maxSoC-0.5 {
		return false // not full yet
	}

	// check if buffer time is active by examining solar forecast
	solarTariff := site.GetTariff(api.TariffUsageSolar)
	if solarTariff == nil {
		return false
	}

	rates, err := solarTariff.Rates()
	if err != nil || len(rates) == 0 {
		return false
	}

	cutoffTime := site.effectiveCutoffTime(rates)

	// calculate available time and buffer
	now := time.Now()
	availableHours := cutoffTime.Sub(now).Hours()
	if availableHours <= 0 {
		return true // already past cutoff, battery should be released
	}

	bufferHours := availableHours * 0.25
	if bufferHours > 2.0 {
		bufferHours = 2.0
	}

	// buffer time is active when remaining time <= buffer time
	bufferActive := availableHours <= bufferHours
	if bufferActive {
		site.log.TRACE.Printf("buffer time active: %.1fh remaining <= %.1fh buffer, battery full at %.1f%%",
			availableHours, bufferHours, site.battery.Soc)
	}

	return bufferActive
}

func (site *Site) shouldUseHoldChargePower() bool {
	// auto-activate BatteryHoldCharge mode when sufficient PV forecast available
	// skip if auto hold charge is disabled
	if !site.batteryAutoHoldCharge {
		return false
	}

	solarTariff := site.GetTariff(api.TariffUsageSolar)
	if solarTariff == nil {
		return false
	}

	rates, err := solarTariff.Rates()
	if err != nil || len(rates) == 0 {
		return false
	}

	now := time.Now()
	cutoffTime := site.effectiveCutoffTime(rates)

	// disable HoldCharge if cutoff already passed or very soon (less than 10 min remaining)
	remainingTime := cutoffTime.Sub(now)
	if remainingTime < 10*time.Minute {
		site.log.TRACE.Printf("hold charge: insufficient time to sunset (%.0f min remaining), disabling",
			remainingTime.Minutes())
		return false
	}

	// check: remaining PV forecast > remaining consumption * factor
	factor := site.batteryAutoHoldChargeFactor
	if factor <= 0 {
		factor = 1.5
	}

	// sum remaining PV forecast from now until cutoff
	var remainingPVForecast float64
	for _, r := range rates {
		if r.Start.After(cutoffTime) {
			break
		}
		if r.End.Before(now) {
			continue
		}
		start := r.Start
		if start.Before(now) {
			start = now
		}
		end := r.End
		if end.After(cutoffTime) {
			end = cutoffTime
		}
		durationHours := end.Sub(start).Hours()
		remainingPVForecast += r.Value * durationHours
	}

	// estimate remaining consumption until midnight
	// use current gridPower extrapolated to full remaining day
	hoursUntilMidnight := float64((24 - now.Hour()))
	remainingConsumption := site.gridPower * hoursUntilMidnight
	if remainingConsumption < 0 {
		remainingConsumption = 0
	}

	site.log.TRACE.Printf("hold charge auto-check: remaining PV %.0f Wh > consumption %.0f Wh * %.2f = %.0f Wh? cutoff=%.0f min",
		remainingPVForecast, remainingConsumption, factor, remainingConsumption*factor, remainingTime.Minutes())

	return remainingPVForecast > (remainingConsumption * factor)
}

func (site *Site) updateBatteryMode(batteryGridChargeActive bool, rate api.Rate) {
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, rate)

	// auto-enable hold charge mode if sufficient PV forecast (only from Unknown or Normal)
	if (batteryMode == api.BatteryUnknown || batteryMode == api.BatteryNormal) && site.shouldUseHoldChargePower() {
		site.log.DEBUG.Println("battery mode: auto-enable HoldCharge (sufficient PV forecast)")
		batteryMode = api.BatteryHoldCharge
	}

	// auto-disable hold charge mode when buffer time active and battery full - allow free discharge
	if batteryMode == api.BatteryHoldCharge && site.isBufferTimeActiveAndBatteryFull() {
		site.log.DEBUG.Println("battery mode: buffer time active and battery full, disabling HoldCharge")
		batteryMode = api.BatteryNormal
	}

	// put battery into hold mode when charging is active and HEMS dimmed
	fromToCharge := batteryMode == api.BatteryCharge || batteryMode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge
	if dimmed := hemsDimmed(site.hems); fromToCharge && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: HEMS dimmed")
		batteryMode = api.BatteryHold
	}

	// NOTE: applyBatteryMode is always called when charge mode is active to validate max soc or when in holdcharge mode
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge || site.batteryMode == api.BatteryHoldCharge {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}

	// TEMPORARY: calculate hold charge power for all batteries (for simulation/debugging)
	for _, dev := range site.batteryMeters {
		site.applyHoldChargePower(dev)
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
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
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

		// set charge power for hold charge mode
		if mode == api.BatteryHoldCharge {
			site.applyHoldChargePower(dev)
		}
	}

	return nil
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
