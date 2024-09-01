package core

import (
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
)

func batteryModeModified(mode api.BatteryMode) bool {
	return mode != api.BatteryUnknown && mode != api.BatteryNormal
}

// GetBatteryMode returns the battery mode
func (site *Site) GetBatteryMode() api.BatteryMode {
	site.RLock()
	defer site.RUnlock()
	return site.batteryMode
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
}

// requiredBatteryMode determines required battery mode based on grid charge and rate
func (site *Site) requiredBatteryMode(batteryGridChargeActive bool, rate api.Rate) api.BatteryMode {
	var res api.BatteryMode
	batMode := site.GetBatteryMode()

	mapper := func(s api.BatteryMode) api.BatteryMode {
		return map[bool]api.BatteryMode{false: s, true: api.BatteryUnknown}[batMode == s]
	}

	switch {
	case batteryGridChargeActive:
		res = mapper(api.BatteryCharge)
	case site.dischargeControlActive(rate):
		res = mapper(api.BatteryHold)
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
}

// applyBatteryMode applies the mode to each battery
func (site *Site) applyBatteryMode(mode api.BatteryMode) error {
	for _, meter := range site.batteryMeters {
		if batCtrl, ok := meter.(api.BatteryController); ok {
			if err := batCtrl.SetBatteryMode(mode); err != nil {
				return err
			}
		}
	}

	return nil
}

func (site *Site) plannerRates() (api.Rates, error) {
	tariff := site.GetTariff(PlannerTariff)
	if tariff == nil || tariff.Type() == api.TariffTypePriceStatic {
		return nil, nil
	}

	return tariff.Rates()
}

func (site *Site) smartCostActive(lp loadpoint.API, rate api.Rate) bool {
	limit := lp.GetSmartCostLimit()
	return limit != nil && !rate.IsEmpty() && rate.Price <= *limit
}

func (site *Site) batteryGridChargeActive(rate api.Rate) bool {
	limit := site.GetBatteryGridChargeLimit()
	enable := site.GetBatteryGridChargeEnableThreshold()
	disable := site.GetBatteryGridChargeDisableThreshold()
	
	// ensure proper values for thresholds and initialize smartGridUsage-Feature with defaults
	// TODO based on batteryMaxSoC & batteryMinSoC 
	if site.BatteryGridChargeDisableThreshold == 0 || site.BatteryGridChargeDisableThreshold>100 {
		site.BatteryGridChargeDisableThreshold=100
	}
	if site.BatteryGridChargeEnableThreshold < 0 {
		site.BatteryGridChargeEnableThreshold=0
	}
		
	if disable < enable {
		site.log.WARN.Println("correction of grid charge disable threshold as it shall not be lower than enable threshold, correcting: ", enable)
		site.SetBatteryGridChargeDisableThreshold(enable)
	} 
	
	//return limit != nil && !rate.IsEmpty() && rate.Price <= *limit
	if site.batteryMode == api.BatteryCharge {
		// take disable threshold into account
		return limit != nil && !rate.IsEmpty() && rate.Price <= *limit && site.batterySoc <= disable
	} else {
		// take enable threshold into account (default: 20% lower than disable threshold)
		return limit != nil && !rate.IsEmpty() && rate.Price <= *limit && site.batterySoc <= enable
	}
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}
	
	//site.log.DEBUG.Println("batteryDischange on GridChargeLimit setting: ", site.GetHoldBatteryOnSmartCostLimit())
	if site.GetHoldBatteryOnSmartCostLimit() {
		limit := site.GetBatteryGridChargeLimit()
		if limit != nil && !rate.IsEmpty() && rate.Price <= *limit {
			site.log.DEBUG.Println("batteryDischange hold because of gridChargeLimit setting: ", site.GetHoldBatteryOnSmartCostLimit())
			return true
		}
	}

	for _, lp := range site.Loadpoints() {
		smartCostActive := site.smartCostActive(lp, rate)
		if lp.GetStatus() == api.StatusC && (smartCostActive || lp.IsFastChargingActive()) {
			if !lp.GetDisableDischargeControl() {
				return true
			} else {
				site.log.DEBUG.Println("DischargeControl for this loadpoint is disabled.")
			}
		}
	}

	return false
}
