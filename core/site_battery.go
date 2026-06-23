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

	// find cutoff time (when solar < 50W)
	now := time.Now()
	var cutoffTime time.Time
	for _, r := range rates {
		if r.Start.After(now) && r.Value < 50 {
			cutoffTime = r.Start
			break
		}
	}
	if cutoffTime.IsZero() {
		cutoffTime = rates[len(rates)-1].End
	}

	// calculate available time and buffer
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
	// condition: daily PV forecast > (daily consumption * 1.5)

	solarTariff := site.GetTariff(api.TariffUsageSolar)
	if solarTariff == nil {
		return false
	}

	rates, err := solarTariff.Rates()
	if err != nil || len(rates) == 0 {
		return false
	}

	// sum today's PV forecast (in Wh)
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	todayEnd := todayStart.AddDate(0, 0, 1)

	var dailyPVForecast float64
	for _, r := range rates {
		if r.Start.After(todayEnd) {
			break
		}
		if r.End.Before(todayStart) {
			continue
		}
		// normalize overlap to today's window
		start := r.Start
		if start.Before(todayStart) {
			start = todayStart
		}
		end := r.End
		if end.After(todayEnd) {
			end = todayEnd
		}
		durationHours := end.Sub(start).Hours()
		dailyPVForecast += r.Value * durationHours
	}

	// estimate daily consumption from accumulated power data
	// use ratio of current gridPower to extrapolate consumption
	currentHour := float64(time.Now().Hour())
	if currentHour == 0 {
		currentHour = 1 // avoid division by zero at midnight
	}
	// rough estimate: grid import up to now, extrapolated to full day
	dailyConsumption := site.gridPower * 24 / currentHour
	if dailyConsumption < 0 {
		dailyConsumption = 0 // negative grid = export, no consumption
	}

	site.log.TRACE.Printf("hold charge auto-check: PV forecast %.0f Wh > consumption %.0f Wh * 1.5 = %.0f Wh?",
		dailyPVForecast, dailyConsumption, dailyConsumption*1.5)

	// condition: PV forecast > consumption * 1.5
	return dailyPVForecast > (dailyConsumption * 1.5)
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
