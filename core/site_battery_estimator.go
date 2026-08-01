package core

import (
	"math"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util/config"
	optimizer "github.com/evcc-io/optimizer/client"
)

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

// estimatorTargetTime parses the configured target time and returns it as today's time.Time.
// Returns zero if the target time has already passed or is not set (falls back to "18:00").
func (site *Site) estimatorTargetTime() time.Time {
	s := site.batteryEstimatorTargetTime
	if s == "" {
		s = "18:00"
	}
	t, err := time.ParseInLocation("15:04", s, time.Local)
	if err != nil {
		site.log.WARN.Printf("invalid batteryEstimatorTargetTime %q, using solar cutoff: %v", s, err)
		return time.Time{}
	}
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
	if target.Before(now) {
		return time.Time{} // already past today's target - no constraint
	}
	return target
}

// effectiveEstimatorCutoffTime returns the earlier of the solar cutoff and the configured
// target time, so the battery reaches maxSoc by the target time even if the sun sets later.
func (site *Site) effectiveEstimatorCutoffTime(rates api.Rates) time.Time {
	now := time.Now()
	solar := solarCutoffTime(rates, now)
	target := site.estimatorTargetTime()
	if !target.IsZero() && target.Before(solar) {
		return target
	}
	return solar
}

// isEstimatorBufferTimeActiveAndBatteryFull disables the estimator's hold charge when the
// battery is already full and the remaining time until cutoff is within the safety buffer,
// allowing the battery to discharge freely instead of sitting needlessly capped.
func (site *Site) isEstimatorBufferTimeActiveAndBatteryFull() bool {
	maxSoC := 100.0
	if len(site.batteryMeters) > 0 {
		if limiter, ok := api.Cap[api.BatterySocLimiter](site.batteryMeters[0].Instance()); ok {
			if _, max := limiter.GetSocLimits(); max > 0 && max < 100 {
				maxSoC = max
			}
		}
	}
	if site.battery.Soc < maxSoC-0.5 {
		return false // not full yet
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
	cutoff := site.effectiveEstimatorCutoffTime(rates)
	availableHours := cutoff.Sub(now).Hours()
	if availableHours <= 0 {
		return true // already past cutoff, release the battery
	}

	bufferHours := math.Min(availableHours*0.25, 2.0)
	return availableHours <= bufferHours
}

// shouldUseBatteryEstimator reports whether the estimator should auto-activate hold charge:
// enough PV forecast remains until cutoff to cover remaining consumption with margin.
func (site *Site) shouldUseBatteryEstimator() bool {
	if !site.batteryEstimator {
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
	cutoff := site.effectiveEstimatorCutoffTime(rates)
	remaining := cutoff.Sub(now)
	if remaining < 10*time.Minute {
		return false
	}

	factor := site.batteryEstimatorFactor
	if factor <= 0 {
		factor = 1.5
	}

	var remainingPV float64
	for _, r := range rates {
		if r.Start.After(cutoff) {
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
		if end.After(cutoff) {
			end = cutoff
		}
		remainingPV += r.Value * end.Sub(start).Hours()
	}

	const fallbackDailyConsumption = 15000.0 // Wh
	remainingConsumption := fallbackDailyConsumption * remaining.Hours() / 24
	slots := int(remaining/tariff.SlotDuration) + 1
	if profile, err := site.homeProfile(slots); err == nil {
		var sum float64
		for _, v := range profile {
			sum += v
		}
		remainingConsumption = sum
	}

	return remainingPV > remainingConsumption*factor
}

// calculateBatteryDeficit returns the energy (kWh) needed to bring a battery from its
// current soc to maxSoc.
func (site *Site) calculateBatteryDeficit(dev config.Device[api.Meter]) float64 {
	meter := dev.Instance()

	batSoc, ok := api.Cap[api.Battery](meter)
	if !ok {
		return 0
	}
	soc, err := batSoc.Soc()
	if err != nil {
		return 0
	}

	batCap, ok := api.Cap[api.BatteryCapacity](meter)
	if !ok || batCap.Capacity() == 0 {
		return 0
	}

	maxSoc := 100.0
	if limiter, ok := api.Cap[api.BatterySocLimiter](meter); ok {
		if _, max := limiter.GetSocLimits(); max > 0 && max <= 100 {
			maxSoc = max
		}
	}

	return math.Max(0, batCap.Capacity()*(maxSoc-soc)/100)
}

// totalMaxBatteryChargePower sums the max AC charge power of all home batteries.
func (site *Site) totalMaxBatteryChargePower() float64 {
	var total float64
	for _, dev := range site.batteryMeters {
		if limiter, ok := api.Cap[api.BatteryPowerLimiter](dev.Instance()); ok {
			charge, _ := limiter.GetPowerLimits()
			total += charge
		}
	}
	return total
}

// spreadChargePower is the pure calculation behind estimateTotalHoldChargePower: given the
// energy deficit and the time budget until cutoff, it derives a charge power that reaches
// the deficit evenly. A safety margin (25% of the remaining time, capped at 2h) is held back
// from the divisor, so as cutoff approaches the required power - and with it the min(...,
// maxPower) cap below - naturally ramps up towards the system maximum, still reaching the
// deficit even if the forecast was pessimistic. The result is always capped to the current
// PV surplus so the estimator never forces grid import - staying "independent" of the
// optimizer must not mean fighting it for the grid connection.
func spreadChargePower(deficitWh, availableHours, maxACPower, maxChargePower, pvSurplus float64) float64 {
	if deficitWh <= 0 || availableHours <= 0 || maxACPower <= 0 || maxChargePower <= 0 || pvSurplus <= 0 {
		return 0
	}

	bufferHours := math.Min(availableHours*0.25, 2.0)
	safeAvailableHours := availableHours - bufferHours
	if safeAvailableHours <= 0 {
		safeAvailableHours = 0.5
	}

	power := deficitWh / safeAvailableHours
	if power < 500 {
		return 0 // below this it's not worth switching the battery into hold charge
	}

	maxPower := math.Min(maxACPower, maxChargePower)
	return math.Min(math.Min(power, maxPower), pvSurplus)
}

// estimateTotalHoldChargePower estimates the total charge power (across all home batteries)
// needed to reach maxSoc by the effective cutoff time, spread evenly over the remaining time.
func (site *Site) estimateTotalHoldChargePower(totalDeficit float64) float64 {
	if totalDeficit <= 0 {
		return 0
	}

	var totalMaxACPower float64
	for _, dev := range site.pvMeters {
		if getter, ok := api.Cap[api.MaxACPowerGetter](dev.Instance()); ok {
			totalMaxACPower += getter.MaxACPower()
		}
	}
	if totalMaxACPower <= 0 {
		return 0
	}

	totalMaxChargePower := site.totalMaxBatteryChargePower()
	if totalMaxChargePower <= 0 {
		return 0
	}

	solarTariff := site.GetTariff(api.TariffUsageSolar)
	if solarTariff == nil {
		return 0
	}
	allRates, err := solarTariff.Rates()
	if err != nil || len(allRates) == 0 {
		return 0
	}

	now := time.Now()
	var rates api.Rates
	for _, r := range allRates {
		if r.End.After(now) {
			rates = append(rates, r)
		}
	}
	if len(rates) == 0 {
		return 0
	}

	cutoff := site.effectiveEstimatorCutoffTime(rates)
	availableHours := cutoff.Sub(now).Hours()

	pvSurplus := math.Max(0, site.pvPower-math.Max(0, site.gridPower))

	return spreadChargePower(totalDeficit*1000, availableHours, totalMaxACPower, totalMaxChargePower, pvSurplus)
}

// updateBatteryEstimatorSuggestions computes the estimator's per-battery hold charge power
// and stores it using the same plan contract the optimizer uses (site.holdChargePlan and
// the "evopt-batteries" publish), so device templates such as the Kostal Reg 1038
// holdcharge case apply it without any changes.
func (site *Site) updateBatteryEstimatorSuggestions() {
	deficits := make(map[string]float64, len(site.batteryMeters))
	var totalDeficit float64
	for _, dev := range site.batteryMeters {
		d := site.calculateBatteryDeficit(dev)
		deficits[dev.Config().Name] = d
		totalDeficit += d
	}

	suggestions := make(map[string]types.Suggestion, len(site.batteryMeters))
	var batteries []batteryResult

	if totalDeficit > 0 {
		totalPower := site.estimateTotalHoldChargePower(totalDeficit)
		for _, dev := range site.batteryMeters {
			name := dev.Config().Name
			power := deficits[name] / totalDeficit * totalPower

			action := "normal"
			if power > suggestionThreshold {
				action = "holdcharge"
			}

			var capacity float64
			if batCap, ok := api.Cap[api.BatteryCapacity](dev.Instance()); ok {
				capacity = batCap.Capacity()
			}

			s := types.Suggestion{Action: action, Charge: power}
			suggestions[name] = s
			batteries = append(batteries, batteryResult{
				batteryDetail: batteryDetail{
					Type:     batteryTypeBattery,
					Name:     name,
					Title:    deviceProperties(dev).Title,
					Capacity: capacity,
				},
				Suggestion: s,
			})
		}
	}

	site.publish("evopt-batteries", batteries)
	site.publish("batteryEstimatorForecast", site.estimatorForecast(deficits))

	site.Lock()
	site.holdChargePlan = singleSlotHoldChargePlan(time.Now(), suggestions)
	site.Unlock()
}

// estimatorForecast simulates each battery's projected SoC from now until the effective
// cutoff time, re-evaluating spreadChargePower at each solar forecast slot so the projected
// power ramps the same way the live suggestion will as time passes. Published in the same
// shape the optimizer uses (see forecastToSeries on the frontend) so the SoC chart can show
// the active strategy's plan instead of always the optimizer's, even while the estimator -
// not the optimizer - is driving hold charge.
func (site *Site) estimatorForecast(deficits map[string]float64) optimizerResult {
	var totalMaxACPower float64
	for _, dev := range site.pvMeters {
		if getter, ok := api.Cap[api.MaxACPowerGetter](dev.Instance()); ok {
			totalMaxACPower += getter.MaxACPower()
		}
	}
	totalMaxChargePower := site.totalMaxBatteryChargePower()
	if totalMaxACPower <= 0 || totalMaxChargePower <= 0 {
		return optimizerResult{}
	}

	solarTariff := site.GetTariff(api.TariffUsageSolar)
	if solarTariff == nil {
		return optimizerResult{}
	}
	allRates, err := solarTariff.Rates()
	if err != nil || len(allRates) == 0 {
		return optimizerResult{}
	}

	now := time.Now()
	var rates api.Rates
	for _, r := range allRates {
		if r.End.After(now) {
			rates = append(rates, r)
		}
	}
	if len(rates) == 0 {
		return optimizerResult{}
	}
	cutoff := site.effectiveEstimatorCutoffTime(rates)

	names := make([]string, 0, len(site.batteryMeters))
	socWh := make(map[string]float64, len(site.batteryMeters))
	capacityWh := make(map[string]float64, len(site.batteryMeters))
	details := make([]batteryDetail, 0, len(site.batteryMeters))
	for _, dev := range site.batteryMeters {
		name := dev.Config().Name
		batCap, ok := api.Cap[api.BatteryCapacity](dev.Instance())
		if !ok || batCap.Capacity() == 0 {
			continue
		}
		batSoc, ok := api.Cap[api.Battery](dev.Instance())
		if !ok {
			continue
		}
		soc, err := batSoc.Soc()
		if err != nil {
			continue
		}

		names = append(names, name)
		capacityWh[name] = batCap.Capacity() * 1000
		socWh[name] = capacityWh[name] * soc / 100
		details = append(details, batteryDetail{
			Type:     batteryTypeBattery,
			Name:     name,
			Title:    deviceProperties(dev).Title,
			Capacity: batCap.Capacity(),
		})
	}
	if len(names) == 0 {
		return optimizerResult{}
	}

	remaining := make(map[string]float64, len(names))
	for _, name := range names {
		remaining[name] = deficits[name] * 1000
	}

	timestamps := []time.Time{now}
	series := make(map[string][]float32, len(names))
	for _, name := range names {
		series[name] = append(series[name], float32(socWh[name]))
	}

	t := now
	for _, r := range rates {
		if r.Start.After(t) {
			t = r.Start
		}
		if !t.Before(cutoff) {
			break
		}
		end := r.End
		if end.After(cutoff) {
			end = cutoff
		}
		dt := end.Sub(t).Hours()
		if dt <= 0 {
			continue
		}

		var totalRemaining float64
		for _, name := range names {
			totalRemaining += remaining[name]
		}

		power := spreadChargePower(totalRemaining, cutoff.Sub(t).Hours(), totalMaxACPower, totalMaxChargePower, r.Value)
		energy := power * dt

		for _, name := range names {
			if totalRemaining > 0 {
				share := energy * remaining[name] / totalRemaining
				socWh[name] = math.Min(socWh[name]+share, capacityWh[name])
				remaining[name] = math.Max(0, capacityWh[name]-socWh[name])
			}
			series[name] = append(series[name], float32(socWh[name]))
		}
		timestamps = append(timestamps, end)

		t = end
	}

	res := optimizer.OptimizationResult{Batteries: make([]optimizer.BatteryResult, len(names))}
	for i, name := range names {
		res.Batteries[i] = optimizer.BatteryResult{StateOfCharge: series[name]}
	}

	return optimizerResult{
		Updated: now,
		Res:     res,
		Details: requestDetails{Timestamps: timestamps, BatteryDetails: details},
	}
}
