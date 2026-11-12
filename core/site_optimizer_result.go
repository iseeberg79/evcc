package core

import (
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/samber/lo"
)

type batteryResult struct {
	batteryDetail
	Full       time.Time        `json:"full,omitzero"`
	Empty      time.Time        `json:"empty,omitzero"`
	Suggestion types.Suggestion `json:"suggestion,omitzero"`
}

// suggestionThreshold ignores numerical noise in power comparisons (W)
const suggestionThreshold = 50

// advisory actions for a loadpoint/vehicle slot; battery actions use api.BatteryMode
const (
	actionStop   = "stop"
	actionCharge = "charge"
)

// slotFlags are the per-run/per-slot facts slotSuggestion classifies against.
type slotFlags struct {
	// attenuating is true while an attenuate_feedin_peaks/attenuate_grid_peaks
	// charging strategy is actually in effect (see attenuating()). Both of
	// slotSuggestion's holdcharge cases only make sense under it: withholding
	// charge, whether idle or at a capped partial rate, is a deliberate trade
	// against a live PV forecast - one this specific run's forecast may
	// already have been judged too unreliable for (holdChargeYieldSufficient
	// downgrades the strategy to none on exactly that basis). Without an
	// active reservation to honor, holding back live surplus just exports it
	// for nothing.
	attenuating bool
	// canCapCharge is true when every home battery site-wide can enforce a
	// partial charge cap - see the charge-cap case below for why it must
	// hold for all of them, not just this one.
	canCapCharge  bool
	gridImporting bool
	gridExporting bool
}

// slotSuggestion maps the optimizer's slot-i corner result onto an advisory action.
// Because the optimization is linear, each slot is at an operating-range extreme, so it
// maps cleanly onto the discrete battery mode / loadpoint intent that control would later apply.
// An idle battery is interpreted from the grid flow: importing means discharge is withheld
// (holddischarge), exporting means charging is withheld (holdcharge). Charging without importing (pure
// self-consumption) is capped at the planned value via holdcharge too, but only when canCapCharge
// - otherwise there is nothing to gain from holdcharge over normal, since no capability would
// apply the cap. canCapCharge must hold for every home battery, not just this one: the resulting
// mode is dispatched site-wide (applyBatteryMode has no per-battery mode concept), so if any
// other battery lacked the cap it would receive the same HoldCharge mode and, without a value
// push of its own, fall back to an unconditional 0 W block instead of the intended partial cap.
// At its soc bound nothing is withheld, the bound does it, so the battery stays in normal mode.
// Without planned discharge the battery is held when holdWorthwhile, since normal mode would let the
// inverter cover intra-slot dips from the battery.
func slotSuggestion(detail batteryDetail, req optimizer.BatteryConfig, res optimizer.BatteryResult, ts optimizer.TimeSeries, etaD float32, i int, f slotFlags, slotHours float64, gridImport, gridExport float32, pvShare float64) types.Suggestion {
	if slotHours <= 0 || i < 0 || i >= len(res.ChargingPower) || i >= len(res.DischargingPower) {
		return types.Suggestion{}
	}

	charge := float64(res.ChargingPower[i]) / slotHours
	discharge := float64(res.DischargingPower[i]) / slotHours

	s := types.Suggestion{
		Charge:    charge,
		Discharge: discharge,
		Grid:      float64(gridImport-gridExport) / slotHours,
	}

	if detail.Type == batteryTypeBattery {
		idle := charge <= suggestionThreshold && discharge <= suggestionThreshold

		var empty, full bool
		if i < len(res.StateOfCharge) {
			soc := res.StateOfCharge[i]
			empty = soc <= req.SMin
			full = req.SMax > 0 && soc >= req.SMax
		}

		switch {
		case charge > pvShare+suggestionThreshold && f.gridImporting:
			// charging beyond its PV surplus share while importing means grid charging
			s.Action = api.BatteryCharge.String()
		case charge > suggestionThreshold && !f.gridImporting && f.canCapCharge && f.attenuating:
			// self-consumption charging with a planned partial power: cap it via holdcharge
			// so the plan's target is enforced instead of the device's own self-consumption
			// logic charging past it. Without a charge-cap capability, holdcharge would apply
			// no cap either, so normal is no worse. Gated on attenuating for the same reason
			// as the idle+exporting case below: the planned cap is only worth enforcing
			// against live surplus while the forecast it's based on is trusted enough to
			// attenuate on in the first place.
			s.Action = api.BatteryHoldCharge.String()
		case idle && f.gridImporting && !empty:
			// idle while importing: discharge is deliberately withheld
			s.Action = api.BatteryHoldDischarge.String()
		case idle && f.gridExporting && f.attenuating && !full:
			// idle while exporting: charge is withheld for a later, bigger peak -
			// only while an attenuate_* strategy actually reserves it; otherwise
			// (see f.attenuating) this falls through to normal instead, so live
			// surplus gets charged rather than exported for no reservation
			s.Action = api.BatteryHoldCharge.String()
		case discharge > suggestionThreshold && f.gridExporting:
			// discharging while exporting means battery-to-grid discharge
			s.Action = api.BatteryDischarge.String()
		case discharge <= suggestionThreshold && !(idle && empty) && holdWorthwhile(req, res, ts, etaD, i):
			s.Action = api.BatteryHoldDischarge.String()
		default:
			s.Action = api.BatteryNormal.String()
		}

		// suggest the mode control applies instead of one the battery does not support
		if mode, err := api.BatteryModeString(s.Action); err == nil && detail.modes != nil {
			if mode = supportedBatteryMode(detail.modes, mode); mode != api.BatteryUnknown {
				s.Action = mode.String()
			}
		}
	} else if charge > suggestionThreshold {
		s.Action = actionCharge
	} else {
		s.Action = actionStop
	}

	return s
}

// attenuating reports whether s is one of the strategies that deliberately
// withholds battery charge - idle or capped - to reserve it for a later,
// bigger peak. Both holdcharge cases in slotSuggestion are gated on it: a
// downgraded-to-none request has no such reservation to honor.
func attenuating(s optimizer.OptimizerStrategyChargingStrategy) bool {
	return s == optimizer.OptimizerStrategyChargingStrategyAttenuateFeedinPeaks || s == optimizer.OptimizerStrategyChargingStrategyAttenuateGridPeaks
}

// holdChargeHeadroomMarginFactor gives the remaining forecast some safety margin over a
// battery's own headroom before treating its later-peak reservation as still worthwhile - see
// holdChargeReservationWorthwhile. 20% margin: the forecast itself may be optimistic, so
// "exactly enough" on paper is not treated as enough in practice.
const holdChargeHeadroomMarginFactor = 1.2

// holdChargeReservationWorthwhile reports whether remainingSurplusWh - the site's forecast PV
// yield minus its forecast household consumption, both from now to the end of today (see
// remainingForecastToday) - still comfortably covers headroomWh, a battery's own remaining
// capacity to 100% SOC (with holdChargeHeadroomMarginFactor margin against the forecast itself
// being optimistic). Consumption is netted out here because not all remaining PV reaches the
// battery: the household is served first, so gross PV yield overstates what could actually still
// be stored. Below the margin, the reservation attenuate_* preserves capacity for is no longer
// credible: there is not enough surplus left today to be confident the battery fills up
// regardless of whether it charges now, so every bit of currently available surplus should go in
// now instead of being deferred for a later peak that may not leave enough behind to complete
// the fill.
func holdChargeReservationWorthwhile(remainingSurplusWh, headroomWh float64) bool {
	return headroomWh <= 0 || remainingSurplusWh >= holdChargeHeadroomMarginFactor*headroomWh
}

// headroomWh is battery i's own remaining capacity to 100% SOC at slot i, in Wh - see
// holdChargeReservationWorthwhile. detail.Capacity is the nominal full capacity (kWh); a
// configured SMax below 100% (e.g. a lower limit soc) is deliberately not used here, since the
// question this answers is physical ("can more sun still fit"), not policy ("are we allowed to
// charge that high").
func headroomWh(detail batteryDetail, res optimizer.BatteryResult, i int) float64 {
	if i < 0 || i >= len(res.StateOfCharge) {
		return 0
	}
	return detail.Capacity*1e3 - float64(res.StateOfCharge[i])
}

// remainingForecastToday sums values from slot i up to (not including) the first later slot that
// falls on a different calendar day than timestamps[i] - the portion of a per-slot Wh series
// (ft or gt) still relevant to today, see holdChargeReservationWorthwhile. values and timestamps
// are assumed the same length and slot-aligned (both come from the same optimizer
// request/response pair); a short values is treated as ending early rather than panicking.
func remainingForecastToday(values []float32, timestamps []time.Time, i int) float64 {
	if i < 0 || i >= len(timestamps) {
		return 0
	}
	y, m, d := timestamps[i].Date()

	var sum float64
	for j := i; j < len(values) && j < len(timestamps); j++ {
		jy, jm, jd := timestamps[j].Date()
		if jy != y || jm != m || jd != d {
			break
		}
		sum += float64(values[j])
	}
	return sum
}

// downgradeUnworthwhileHoldCharge clears s back to Normal when it suggests HoldCharge but
// detail's remaining surplus for today no longer credibly covers its own headroom to 100% SOC -
// see holdChargeReservationWorthwhile. Applied as a post-step on slotSuggestion's result instead
// of threading remaining-forecast data through slotSuggestion/slotFlags's signatures.
func downgradeUnworthwhileHoldCharge(s types.Suggestion, detail batteryDetail, res optimizer.BatteryResult, i int, remainingSurplusWh float64) types.Suggestion {
	if s.Action != api.BatteryHoldCharge.String() {
		return s
	}
	if holdChargeReservationWorthwhile(remainingSurplusWh, headroomWh(detail, res, i)) {
		return s
	}
	s.Action = api.BatteryNormal.String()
	return s
}

// gridFlags derives slot i's import/export state from the optimizer's
// site-level flow, at the same power threshold as charge/discharge so a
// trickle (numerical residual) does not count as importing or exporting for
// mode selection.
func gridFlags(res *optimizer.OptimizationResult, i int, slotHours float64) (importing, exporting bool) {
	importing = i < len(res.GridImport) && float64(res.GridImport[i])/slotHours > suggestionThreshold
	exporting = i < len(res.GridExport) && float64(res.GridExport[i])/slotHours > suggestionThreshold
	return
}

// holdWorthwhile reports whether energy kept in the battery now replaces a later import at a
// higher price. That happens where the plan, once discharging, runs the battery short. A
// battery that fills up first only displaces pv charging with the kept energy, and one with
// energy to spare only raises the leftover at the end of the horizon.
func holdWorthwhile(req optimizer.BatteryConfig, res optimizer.BatteryResult, ts optimizer.TimeSeries, etaD float32, slot int) bool {
	if slot >= len(ts.PN) {
		return false
	}

	discharging := false
	// normal mode may drain the battery at full power until the next run sees the drained soc
	drain := req.DMax * float32(optimizerInterval.Hours())
	if etaD > 0 {
		drain /= etaD
	}
	reserve := req.SMin + drain

	for i := slot + 1; i < min(len(res.DischargingPower), len(res.StateOfCharge), len(ts.PN), len(ts.Dt)); i++ {
		if req.SMax > 0 && res.StateOfCharge[i] >= req.SMax {
			return false
		}

		if !discharging {
			slotHours := float64(ts.Dt[i]) / 3600
			if slotHours <= 0 || float64(res.DischargingPower[i])/slotHours <= suggestionThreshold {
				continue
			}
			discharging = true
		}

		if res.StateOfCharge[i] <= reserve {
			return ts.PN[i] > ts.PN[slot]
		}
	}

	return false
}

// optimizerPlan is a loadpoint's charging schedule from the last solve
type optimizerPlan struct {
	rates  api.Rates // charging slots valued at the grid import price
	energy []float64 // charge energy per slot, Wh
}

// loadpointPlan extracts the remaining charging slots of a battery result
func loadpointPlan(res optimizer.BatteryResult, prices []float32, schedule optimizerSchedule, now time.Time) optimizerPlan {
	var plan optimizerPlan

	for slot := range schedule.endsAfter(now) {
		if slot >= len(res.ChargingPower) || slot >= len(prices) {
			break
		}

		energy := float64(res.ChargingPower[slot])
		if energy/schedule.duration(slot).Hours() <= suggestionThreshold {
			continue
		}

		plan.energy = append(plan.energy, energy)
		plan.rates = append(plan.rates, api.Rate{
			Start: schedule.timestamps[slot],
			End:   schedule.end(slot),
			Value: float64(prices[slot]) * 1e3, // per Wh to per kWh
		})
	}

	return plan
}

// setBatteryForecast replaces the battery forecast of the cached state
func (site *Site) setBatteryForecast(forecast *types.BatteryForecast) {
	site.Lock()
	defer site.Unlock()

	site.battery.Forecast = forecast
}

// applyOptimizerResult maps the optimizer response onto suggestions, battery
// forecast and notifications
func (site *Site) applyOptimizerResult(solve optimizerSolve, now time.Time) {
	req, details, res, schedule := solve.req, solve.details, solve.res, solve.schedule

	slot := schedule.activeSlot(now)
	slotHours := schedule.duration(slot).Hours()
	f := slotFlags{
		attenuating:  attenuating(req.Strategy.ChargingStrategy),
		canCapCharge: site.allBatteriesHaveChargeCap(),
	}
	f.gridImporting, f.gridExporting = gridFlags(&res, slot, slotHours)

	var gridImport, gridExport float32
	if slot >= 0 && slot < len(res.GridImport) {
		gridImport = res.GridImport[slot]
	}
	if slot >= 0 && slot < len(res.GridExport) {
		gridExport = res.GridExport[slot]
	}

	// home battery discharge planned for the slot, a loadpoint charge it covers is no surplus
	var batteryDischarge, batteryCharge, pvSurplus float64
	if slot >= 0 && slotHours > 0 {
		for i, detail := range details.BatteryDetails {
			if detail.Type == batteryTypeBattery && slot < len(res.Batteries[i].DischargingPower) {
				batteryDischarge += float64(res.Batteries[i].DischargingPower[slot]) / slotHours
			}
			if detail.Type == batteryTypeBattery && slot < len(res.Batteries[i].ChargingPower) {
				batteryCharge += float64(res.Batteries[i].ChargingPower[slot]) / slotHours
			}
		}

		// PV surplus over home demand (gt, excluding loadpoints) covers home battery charging without grid charging (#35416).
		// Loadpoints are not deducted: the optimizer splits PV between them and the battery arbitrarily.
		if ts := req.TimeSeries; slot < len(ts.Ft) && slot < len(ts.Gt) {
			pvSurplus = max(0, float64(ts.Ft[slot]-ts.Gt[slot])/slotHours)
		}
	}

	var batteries []batteryResult
	suggestions := make(map[string]types.Suggestion, len(req.Batteries))

	for i, batReq := range req.Batteries {
		// guard against a malformed/short optimizer response - request and result are
		// expected to line up index-for-index, but nothing enforces that across the wire
		if i >= len(res.Batteries) || i >= len(details.BatteryDetails) {
			site.log.WARN.Printf("optimizer: result has fewer batteries (%d) than requested (%d), skipping remainder", len(res.Batteries), len(req.Batteries))
			break
		}

		batRes := res.Batteries[i]
		detail := details.BatteryDetails[i]

		// home batteries share the PV surplus in proportion to their charge
		var pvShare float64
		if batteryCharge > 0 && slot >= 0 && slot < len(batRes.ChargingPower) {
			pvShare = pvSurplus * float64(batRes.ChargingPower[slot]) / slotHours / batteryCharge
		}

		suggestion := slotSuggestion(detail, batReq, batRes, req.TimeSeries, req.EtaD, slot, f, slotHours, gridImport, gridExport, pvShare)
		remainingSurplusWh := remainingForecastToday(req.TimeSeries.Ft, details.Timestamps, slot) - remainingForecastToday(req.TimeSeries.Gt, details.Timestamps, slot)
		suggestion = downgradeUnworthwhileHoldCharge(suggestion, detail, batRes, slot, remainingSurplusWh)

		batteries = append(batteries, batteryResult{
			batteryDetail: detail,
			Full: matchSoc(batRes.StateOfCharge, schedule, now, func(soc float32) bool {
				return soc >= batReq.SMax
			}),
			Empty: matchSoc(batRes.StateOfCharge, schedule, now, func(soc float32) bool {
				return soc <= batReq.SMin
			}),
			Suggestion: suggestion,
		})

		if suggestion.Action == "" {
			continue
		}

		if detail.loadpoint != nil {
			suggestion.BatteryDischarge = batteryDischarge
			suggestion.BatteryCharge = batteryCharge
		}

		// uncontrollable devices can't act on a suggestion
		if key := detail.key(); key != "" && detail.controllable {
			suggestions[key] = suggestion

			// the schedule is gated by the suggestion, so it needs no clearing.
			// Without the plan goal in the model its slots are no plan.
			if detail.loadpoint != nil {
				var plan optimizerPlan
				if batReq.SGoal != nil {
					plan = loadpointPlan(batRes, req.TimeSeries.PN, schedule, now)
				}
				site.loadpoints[*detail.loadpoint].setOptimizerPlan(plan)
			}
		}
	}

	site.publish("evopt-batteries", batteries)

	site.setSuggestions(suggestions)
	site.setBatteryForecast(site.addBatteryForecastTotals(req.Batteries, res.Batteries, schedule, now))

	site.publishBattery()

	// publish for all loadpoints so suggestions of dropped-out loadpoints clear
	site.publishSuggestions()

	// notify on actionable suggestion changes (advisory only, see #31903)
	for _, ev := range site.diffSuggestions(site.pendingSuggestions(details.BatteryDetails)) {
		site.pushEvent(ev)
	}

	solve.slot = slot
	site.setLastOptimizerSolve(&solve)
}

func (site *Site) addBatteryForecastTotals(req []optimizer.BatteryConfig, resp []optimizer.BatteryResult, schedule optimizerSchedule, now time.Time) *types.BatteryForecast {
	if len(resp) == 0 || len(resp[0].StateOfCharge) == 0 {
		return nil
	}

	high, low := batteryForecastSocExtremes(req, resp, schedule, now)
	if high == nil && low == nil {
		return nil
	}

	point := func(p *batteryForecastSlot) *types.BatteryForecastPoint {
		if p == nil {
			return nil
		}
		ts := schedule.end(p.slot)
		if !ts.After(now) {
			return nil
		}
		return &types.BatteryForecastPoint{Soc: p.soc, Time: ts, Limit: p.limit}
	}

	res := types.BatteryForecast{
		Highest: point(high),
		Lowest:  point(low),
	}
	if res.Highest == nil && res.Lowest == nil {
		return nil
	}
	return &res
}

type batteryForecastSlot struct {
	slot  int
	soc   float64 // percent
	limit bool    // true when SMax (highest) or SMin (lowest) boundary reached
}

// batteryForecastSocExtremes returns the highest and lowest aggregate SOC
// points across home batteries (SCapacity > 0) over the forecast horizon.
// The Limit flag indicates whether the SOC reached the configured SMax (for
// the highest point) or SMin (for the lowest point) boundary - in which case
// the battery is forecasted to become fully charged or empty.
// Returns nil for either point when no home battery is present or when the
// battery already is at the respective limit.
func batteryForecastSocExtremes(req []optimizer.BatteryConfig, resp []optimizer.BatteryResult, schedule optimizerSchedule, now time.Time) (*batteryForecastSlot, *batteryForecastSlot) {
	slot := schedule.activeSlot(now)
	homeIndices := lo.FilterMap(req, func(b optimizer.BatteryConfig, i int) (int, bool) {
		return i, b.SCapacity > 0
	})
	if len(homeIndices) == 0 || len(resp) == 0 || slot < 0 || slot >= len(resp[homeIndices[0]].StateOfCharge) {
		return nil, nil
	}

	totalCapacity := lo.SumBy(homeIndices, func(i int) float32 { return req[i].SCapacity })
	totalSMax := lo.SumBy(homeIndices, func(i int) float32 { return req[i].SMax })
	totalSMin := lo.SumBy(homeIndices, func(i int) float32 { return req[i].SMin })
	totalSInitial := lo.SumBy(homeIndices, func(i int) float32 {
		if slot > 0 {
			return resp[i].StateOfCharge[slot-1]
		}
		return req[i].SInitial
	})

	var high, low *batteryForecastSlot
	for i := range schedule.endsAfter(now) {
		if i >= len(resp[homeIndices[0]].StateOfCharge) {
			break
		}
		sum := lo.SumBy(homeIndices, func(idx int) float32 { return resp[idx].StateOfCharge[i] })
		soc := float64(sum/totalCapacity) * 100
		fullReached := totalSMax > 0 && sum >= totalSMax
		emptyReached := sum <= totalSMin

		// first slot at SMax wins for highest
		if high == nil || (!high.limit && (soc > high.soc || fullReached)) {
			high = &batteryForecastSlot{slot: i, soc: soc, limit: fullReached}
		}
		// first slot at SMin wins for lowest
		if low == nil || (!low.limit && (soc < low.soc || emptyReached)) {
			low = &batteryForecastSlot{slot: i, soc: soc, limit: emptyReached}
		}
	}

	// battery is already at the limit - announcing it will become full/empty is pointless
	if high != nil && high.limit && high.slot == slot && totalSInitial >= totalSMax {
		high = nil
	}
	if low != nil && low.limit && low.slot == slot && totalSInitial <= totalSMin {
		low = nil
	}

	return high, low
}

// matchSoc returns the end of the first slot whose soc satisfies fun.
func matchSoc(ts []float32, schedule optimizerSchedule, now time.Time, fun func(float32) bool) time.Time {
	for i := range schedule.endsAfter(now) {
		if i >= len(ts) {
			break
		}
		if fun(ts[i]) {
			return schedule.end(i)
		}
	}

	return time.Time{}
}
