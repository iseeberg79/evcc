package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/messenger"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/sponsor"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/jinzhu/now"
	"github.com/samber/lo"
	"golang.org/x/exp/constraints"
)

const (
	// eta is the efficiency of the battery charging/discharging
	eta = 0.9

	// batteryPower is the default power of the battery in W
	batteryPower = 6000

	// socDepletionCostHigh was a small €/h price meant to nudge the optimizer away from parking
	// a home battery above 80% SOC (calendar aging). Set to 0: priced per hour rather than as a
	// one-off, it rewarded finishing a charge late (minimizing time above 80%) over finishing
	// early with margin - traced on a real request to pushing full charge from 16:45 to 18:15,
	// into a load spike and the next morning's PV shortfall, for ~1ct avoided aging cost against
	// a resulting shortfall worth 10-20ct. The hourly pricing shape was the actual lever, not the
	// price level - any value from 0.0005 up reproduced it, up to 10x the real 0.005.
	socDepletionCostHigh = float32(0.0)

	// socDepletionCostLowDefault was the €/h reserve-comfort price that keeps the home battery
	// off the low floor: ramps from zero at 20% SOC to full at s_min. Not battery aging (low SOC
	// is gentle) but a soft buffer for spontaneous loads / forecast deviation. Deliberately below
	// the import price so it never triggers grid charging to hold the band and yields to real
	// arbitrage. Unlike socDepletionCostHigh, no real-world case has shown it does harm - real
	// tests this far only found it inert (every checked low-SOC event either sat at s_min already
	// or drained through in a single slot), never actively wrong, so it isn't disabled outright.
	// site.socDepletionCostLow (testing only, see site.go) now carries the live value instead of
	// this constant, defaulting to 0 - set it back to 0.002 at runtime to compare.
	socDepletionCostLowDefault = float32(0.002)
)

// optimizerChargeBeforeExport is kept as a selectable config value even though optimizer PR
// 126 removed it from the API's own charging_strategy enum - it is no longer a string the
// optimizer itself accepts, translated to none at the API boundary instead (see the Strategy
// construction below), so existing site configs and the UI dropdown keep working unchanged.
// It used to also set a battery_first tie-break; that field was dropped from the optimizer
// (controlled A/B runs found no measurable effect), so the translation is now a plain alias
// for none.
const optimizerChargeBeforeExport = "charge_before_export"

// optimizerChargingStrategies are the valid grid charging strategies; the first
// entry is the default and preserves the previous hard-coded behavior.
var optimizerChargingStrategies = []string{
	optimizerChargeBeforeExport,
	string(optimizer.OptimizerStrategyChargingStrategyAttenuateDemandPeaks),
	string(optimizer.OptimizerStrategyChargingStrategyAttenuateFeedinPeaks),
	string(optimizer.OptimizerStrategyChargingStrategyAttenuateGridPeaks),
	string(optimizer.OptimizerStrategyChargingStrategyNone),
}

const defaultOptimizerChargingStrategy = optimizerChargeBeforeExport

// optimizerDecaySlots is the number of slots over which measured values decay into the forecast
const optimizerDecaySlots = 4

// optimizerResult wraps the optimizer publish payload to implement BytesMarshaler.
// This ensures publishComplex serializes it as a single JSON message instead of
// recursively decomposing each struct field and array element into individual MQTT
// topics (~1,500 messages per optimizer run).
type optimizerResult struct {
	Updated time.Time                    `json:"updated"`
	Req     optimizer.OptimizationInput  `json:"req"`
	Res     optimizer.OptimizationResult `json:"res"`
	Details requestDetails               `json:"details"`
}

var _ api.BytesMarshaler = (*optimizerResult)(nil)

func (r optimizerResult) MarshalBytes() ([]byte, error) {
	return json.Marshal(r)
}

type batteryType string

const (
	OPTIMIZER_URI = "https://optimizer.evcc.io"

	batteryTypeLoadpoint batteryType = "loadpoint"
	batteryTypeVehicle   batteryType = "vehicle"
	batteryTypeBattery   batteryType = "battery"
)

type batteryDetail struct {
	Type     batteryType `json:"type"`
	Title    string      `json:"title,omitempty"`
	Name     string      `json:"name,omitempty"`
	Capacity float64     `json:"capacity,omitempty"`

	loadpoint    *int // originating loadpoint id for loadpoint/vehicle entries
	controllable bool // device can act on suggestions
}

// batteryKey and loadpointKey build the canonical device keys used for
// suggestion routing and notifications
func batteryKey(name string) string { return "battery:" + name }
func loadpointKey(id int) string    { return fmt.Sprintf("loadpoint:%d", id) }

// key identifies the device across optimizer runs; an empty key means the
// device can't act on a suggestion.
func (d batteryDetail) key() string {
	switch {
	case d.Type == batteryTypeBattery:
		return batteryKey(d.Name)
	case d.loadpoint != nil:
		return loadpointKey(*d.loadpoint)
	default:
		return ""
	}
}

// currentAction returns the device's current operating mode for suggestion
// comparison. Must only be called for devices with a non-empty key.
func (d batteryDetail) currentAction(site *Site) string {
	if d.Type == batteryTypeBattery {
		return site.batteryAction()
	}
	return loadpointCurrentAction(site.loadpoints[*d.loadpoint])
}

// batteryAction returns the battery's current mode for suggestion comparison.
// A battery that was never switched (BatteryUnknown) is in normal operation.
func (site *Site) batteryAction() string {
	if mode := site.GetBatteryMode(); mode != api.BatteryUnknown {
		return mode.String()
	}
	return api.BatteryNormal.String()
}

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

// evSuggestion notifies when the optimizer's advisory action for a device changes
const evSuggestion = "suggestion"

// pendingSuggestion pairs a device's current-run suggestion with the
// notification event to emit if it represents an actionable change.
type pendingSuggestion struct {
	suggestion types.Suggestion
	event      messenger.Event
}

// suggestionEvent builds the notification event for a device suggestion
func suggestionEvent(detail batteryDetail, s types.Suggestion) messenger.Event {
	ev := messenger.Event{Event: evSuggestion, Attributes: map[string]any{
		"suggestionAction": s.Action,
		"suggestionTitle":  detail.Title,
	}}

	switch {
	case detail.Type == batteryTypeBattery:
		ev.Attributes["suggestionName"] = detail.Name
	case detail.loadpoint != nil:
		id := *detail.loadpoint
		ev.Loadpoint = &id
	}

	return ev
}

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
// (hold), exporting means charging is withheld (holdcharge). Charging without importing (pure
// self-consumption) is capped at the planned value via holdcharge too, but only when canCapCharge
// - otherwise there is nothing to gain from holdcharge over normal, since no capability would
// apply the cap. canCapCharge must hold for every home battery, not just this one: the resulting
// mode is dispatched site-wide (applyBatteryMode has no per-battery mode concept), so if any
// other battery lacked the cap it would receive the same HoldCharge mode and, without a value
// push of its own, fall back to an unconditional 0 W block instead of the intended partial cap.
func slotSuggestion(detail batteryDetail, res optimizer.BatteryResult, i int, f slotFlags, slotHours float64, gridImport, gridExport float32) types.Suggestion {
	if slotHours <= 0 || i < 0 || i >= len(res.ChargingPower) || i >= len(res.DischargingPower) {
		return types.Suggestion{}
	}

	// slot i's charge value used to be unreliable under the old peak+ramp leveling - the LP
	// was indifferent about *when* within a locally flat price window to charge, so the
	// solver could arbitrarily defer all of it into a later, larger slot. We used to rely on
	// battery_first (set unconditionally for every home battery request) to bias the tie
	// towards charging early and make slot i trustworthy at any index. That flag is no longer
	// set (see batteryRequest): it moved from a per-battery field to one shared by the whole
	// request in optimizer PR 126, and controlled A/B runs against real production requests
	// found no measurable effect once PR 130's level-deviation leveling is in place - that
	// term on its own already prefers a spread schedule over an arbitrary one, which is
	// presumably why toggling battery_first moved nothing: there was no genuine tie left to
	// break. If slot readings ever look stale again, re-check that assumption first.
	// For loadpoints/vehicles the same degeneracy can still occur, but their suggestion only
	// ever feeds the advisory UI (see loadpointSuggestion), never real charge control, so an
	// occasionally-stale "stop" instead of "charge" is cosmetic - matching upstream, which
	// reads slot 0 unconditionally for the same reason (loadpoints/vehicles are never
	// plan-cached across slots, only batteries are - see site.suggestion/reapplySuggestions).
	charge := float64(res.ChargingPower[i]) / slotHours
	discharge := float64(res.DischargingPower[i]) / slotHours

	s := types.Suggestion{
		Charge:    charge,
		Discharge: discharge,
		Grid:      float64(gridImport-gridExport) / slotHours,
	}

	if detail.Type == batteryTypeBattery {
		idle := charge <= suggestionThreshold && discharge <= suggestionThreshold
		switch {
		case charge > suggestionThreshold && f.gridImporting:
			// charging while importing means grid charging
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
		case idle && f.gridImporting:
			// idle while importing: discharge is deliberately withheld
			s.Action = api.BatteryHold.String()
		case idle && f.gridExporting && f.attenuating:
			// idle while exporting: charge is withheld for a later, bigger peak -
			// only while an attenuate_* strategy actually reserves it; otherwise
			// (see f.attenuating) this falls through to normal instead, so live
			// surplus gets charged rather than exported for no reservation
			s.Action = api.BatteryHoldCharge.String()
		case discharge > suggestionThreshold && f.gridExporting:
			// discharging while exporting means battery-to-grid discharge
			s.Action = api.BatteryDischarge.String()
		default:
			s.Action = api.BatteryNormal.String()
		}
	} else if charge > suggestionThreshold {
		s.Action = actionCharge
	} else {
		s.Action = actionStop
	}

	return s
}

// loadpointCurrentAction returns the loadpoint's current operating mode for
// suggestion comparison, reusing chargeGoalReached so a loadpoint left
// enabled while idle (e.g. vehicle finished at its limit) is treated as
// stopped instead of triggering a spurious pause suggestion.
func loadpointCurrentAction(lp *Loadpoint) string {
	lp.RLock()
	enabled := lp.enabled
	lp.RUnlock()

	if enabled && !lp.chargeGoalReached(enabled) {
		return actionCharge
	}
	return actionStop
}

// suggestionMaxAge releases a loadpoint gated by a solve the site stopped
// refreshing; the site itself expires its cached solve in reapplySuggestions
const suggestionMaxAge = 2 * tariff.SlotDuration

// setSuggestions replaces the suggestions applied on each publish
func (site *Site) setSuggestions(suggestions map[string]types.Suggestion) {
	site.Lock()
	defer site.Unlock()

	site.suggestions = suggestions
}

// setBatteryForecast replaces the battery forecast of the cached state
func (site *Site) setBatteryForecast(forecast *types.BatteryForecast) {
	site.Lock()
	defer site.Unlock()

	site.battery.Forecast = forecast
}

// suggestion returns the optimizer suggestion for the given device key.
// The actionable flag is evaluated on read against the device's current
// action since that changes between optimizer runs.
func (site *Site) suggestion(key, currentAction string) *types.Suggestion {
	site.RLock()
	s, ok := site.suggestions[key]
	site.RUnlock()

	if !ok {
		return nil
	}

	s.Actionable = s.Action != currentAction

	return &s
}

// publishSuggestions publishes the loadpoints' suggestions and hands them to the
// loadpoints, where they act as start/stop gate while the optimizer is in control
func (site *Site) publishSuggestions() {
	for id, lp := range site.loadpoints {
		if lp == nil {
			continue
		}

		s := site.suggestion(loadpointKey(id), loadpointCurrentAction(lp))

		var val any
		if s != nil {
			val = *s
		}
		site.publishLoadpoint(id, keys.Suggestion, val)

		lp.setSuggestion(s)
	}
}

// clearSuggestions removes all suggestions and the battery forecast when the
// optimizer result is stale
func (site *Site) clearSuggestions() {
	site.setSuggestions(nil)
	site.setBatteryForecast(nil)

	site.publishBattery()
	site.publishSuggestions()

	site.Lock()
	site.suggestionActions = nil
	site.lastOptimizerSolve = nil
	site.Unlock()
}

// pendingSuggestions collects the stored suggestions with their actionable flag
// evaluated against the devices' current operating mode
func (site *Site) pendingSuggestions(details []batteryDetail) map[string]pendingSuggestion {
	pending := make(map[string]pendingSuggestion, len(details))

	for _, detail := range details {
		key := detail.key()
		if key == "" {
			continue
		}

		s := site.suggestion(key, detail.currentAction(site))
		if s == nil {
			continue
		}

		pending[key] = pendingSuggestion{suggestion: *s, event: suggestionEvent(detail, *s)}
	}

	return pending
}

// diffSuggestions updates the tracked actionable optimizer suggestions and
// returns the events to send for devices whose actionable action changed since
// the last run. Non-actionable or vanished devices are pruned so a later
// actionable change re-notifies.
func (site *Site) diffSuggestions(pending map[string]pendingSuggestion) []messenger.Event {
	site.Lock()
	defer site.Unlock()

	if site.suggestionActions == nil {
		site.suggestionActions = make(map[string]string)
	}

	// prune devices that are gone or no longer actionable
	for key := range site.suggestionActions {
		if p, ok := pending[key]; !ok || !p.suggestion.Actionable {
			delete(site.suggestionActions, key)
		}
	}

	var events []messenger.Event
	for key, p := range pending {
		if !p.suggestion.Actionable || site.suggestionActions[key] == p.suggestion.Action {
			continue
		}
		site.suggestionActions[key] = p.suggestion.Action
		events = append(events, p.event)
	}
	return events
}

type requestDetails struct {
	Timestamps     []time.Time     `json:"timestamp"`
	BatteryDetails []batteryDetail `json:"batteryDetails"`
}

// optimizerBattery pairs a battery request entry with its device detail
type optimizerBattery struct {
	cfg    optimizer.BatteryConfig
	detail batteryDetail
}

func optimizerURI() string {
	return cmp.Or(os.Getenv("OPTIMIZER_URI"), OPTIMIZER_URI)
}

const slotsPerHour = float64(time.Hour / tariff.SlotDuration)

// errOptimizerNotReady means battery measurements aren't available yet (e.g. at
// startup); the slot gate is left open so the next cycle retries.
var errOptimizerNotReady = errors.New("battery measurements not ready")

// optimizerInterval is the refresh cadence. It divides the slot duration so
// every slot starts on a fresh result.
const optimizerInterval = 5 * time.Minute

// optimizerUpdateAsync runs the optimizer unless the last run is younger than
// optimizerInterval. Pass force to run regardless, e.g. when a changed setting
// should take effect immediately. It is a no-op when the optimizer is not
// active or a run is already in progress; the running update reflects the
// change on its next run.
func (site *Site) optimizerUpdateAsync(force bool) {
	if !sponsor.IsAuthorized() || !optimizerEnabled() {
		return
	}

	if !site.optimizerMu.TryLock() {
		return
	}
	defer site.optimizerMu.Unlock()

	if force {
		// keep the gate open so a not-ready run is retried on the next cycle
		site.optimizerUpdated = time.Time{}
	} else if time.Since(site.optimizerUpdated) < optimizerInterval {
		return
	}

	var err error

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic %v", r)
		}

		// not ready yet: keep the gate open for an immediate retry next cycle
		if errors.Is(err, errOptimizerNotReady) {
			return
		}

		site.optimizerUpdated = time.Now()

		// a failed run keeps the advice: dropping it would release the controlled
		// devices for one cycle. reapplySuggestions expires the cached solve.
		if err != nil {
			site.log.ERROR.Println("optimizer:", err)
		}
	}()

	err = site.optimizerUpdate(site.state().battery.Devices)
}

// optimizerRequest assembles the optimizer request and the matching device
// details from tariffs, home profile, loadpoints and battery meters
func (site *Site) optimizerRequest(battery []types.Measurement) (optimizer.OptimizationInput, requestDetails, error) {
	var req optimizer.OptimizationInput
	var details requestDetails

	solarTariff := site.GetTariff(api.TariffUsageSolar)
	solar := currentRates(solarTariff)

	grid := currentRates(site.GetTariff(api.TariffUsageGrid))
	feedIn := currentRates(site.GetTariff(api.TariffUsageFeedIn))

	minLen := lo.Min([]int{len(grid), len(feedIn)})
	// exclude empty solar forecast from minLen
	if solarTariff != nil && len(solar) > 0 {
		minLen = min(minLen, len(solar))
	}

	if optimizerURI() == OPTIMIZER_URI {
		minLen = slotsUntil(grid, optimizerHorizon(time.Now()), minLen)
	}

	if expectedSlots := 8; minLen < expectedSlots {
		if solarTariff != nil {
			return req, details, fmt.Errorf("not enough forecast slots for meaningful optimization: %d < %d (grid=%d, feedIn=%d, solar=%d)", minLen, expectedSlots, len(grid), len(feedIn), len(solar))
		}
		return req, details, fmt.Errorf("not enough forecast slots for meaningful optimization: %d < %d (grid=%d, feedIn=%d)", minLen, expectedSlots, len(grid), len(feedIn))
	}

	now := time.Now()
	dt := timeSteps(minLen, now)
	firstSlotDuration := time.Duration(dt[0]) * time.Second

	site.log.DEBUG.Printf("optimizer: optimizing %d slots until %v: grid=%d, feedIn=%d, solar=%d, first slot: %v",
		minLen,
		grid[minLen-1].End.Local(),
		len(grid), len(feedIn), len(solar),
		firstSlotDuration,
	)

	gt, err := site.homeProfile(minLen)
	if err != nil {
		return req, details, err
	}

	// blend measured energy of the last metrics slot into the first slots
	if v := site.measuredSlotEnergy(metrics.Home); v > 0 {
		orig := slices.Clone(gt[:min(optimizerDecaySlots, len(gt))])
		blendMeasured(gt, v, optimizerDecaySlots)
		site.log.DEBUG.Printf("optimizer: home slots updated with measured %.0fWh: %.0f -> %.0f", v, orig, gt[:len(orig)])
	}

	// heating loadpoints add their forecast demand on top of the measured base load
	heaters := site.addHeatingDemand(gt, minLen)

	// allow empty solar forecast
	ft := lo.RepeatBy(minLen, func(i int) float32 { return float32(0) })
	if solarTariff != nil && len(solar) > 0 {
		solarEnergy, err := solarRatesToEnergy(solar)
		if err != nil {
			return req, details, err
		}

		// scale the forecast by the trailing percentile of the measured production
		// ratio when enabled
		scale := site.effectiveSolarScale()
		ftSlots := scaleAndPrune(solarEnergy, scale, minLen)

		// decay the scale derived from measured vs forecasted energy of the last completed slot
		if pv, fcst := site.measuredSlotEnergy(site.Meters.PVMetersRef...), site.measuredSlotEnergy(metrics.Forecast)*scale; pv > 0 && fcst > 0 {
			orig := slices.Clone(ftSlots[:min(optimizerDecaySlots, len(ftSlots))])
			blendScale(ftSlots, pv/fcst, optimizerDecaySlots)
			site.log.DEBUG.Printf("optimizer: pv slots updated with scale %.2f: %.0f -> %.0f", pv/fcst, orig, ftSlots[:len(orig)])
		}
		ft = prorate(ftSlots, firstSlotDuration)
	}

	// charge_before_export was removed from the optimizer's charging_strategy enum by PR 126:
	// it is now just none (see the comment on optimizerChargeBeforeExport). Translated at the
	// API boundary so a site still configured for it keeps working instead of erroring against
	// the now-invalid string.
	chargingStrategy := site.GetOptimizerChargingStrategy()
	if chargingStrategy == optimizerChargeBeforeExport {
		chargingStrategy = string(optimizer.OptimizerStrategyChargingStrategyNone)
	}

	// attenuate_feedin_peaks/attenuate_grid_peaks level the export profile by withholding
	// battery charge for a later, bigger peak (the holdcharge suggestion in site_battery.go).
	// That's only a good trade on a day with enough PV left to actually fill the reservation -
	// on a poor one it just risks skipping today's safe, unconstrained charge for a peak that
	// never comes. Downgrade to none instead of gambling on it; see holdChargeYieldSufficient.
	switch optimizer.OptimizerStrategyChargingStrategy(chargingStrategy) {
	case optimizer.OptimizerStrategyChargingStrategyAttenuateFeedinPeaks, optimizer.OptimizerStrategyChargingStrategyAttenuateGridPeaks:
		if !site.holdChargeYieldSufficient() {
			site.log.DEBUG.Printf("optimizer: charging strategy %s downgraded to none, insufficient PV yield expected today", chargingStrategy)
			chargingStrategy = string(optimizer.OptimizerStrategyChargingStrategyNone)
		}
	}

	req = optimizer.OptimizationInput{
		Strategy: optimizer.OptimizerStrategy{
			ChargingStrategy:    optimizer.OptimizerStrategyChargingStrategy(chargingStrategy),
			DischargingStrategy: optimizer.OptimizerStrategyDischargingStrategyDischargeBeforeImport,
		},
		EtaC: eta,
		EtaD: eta,
		TimeSeries: optimizer.TimeSeries{
			Dt: dt,
			Gt: prorate(gt, firstSlotDuration),
			Ft: ft,
			PN: scaleAndPrune(grid, 0.001, minLen),
			PE: scaleAndPrune(feedIn, 0.001, minLen),
		},
	}

	// end of horizon Wh value
	pa := lo.Min(req.TimeSeries.PN) * eta * 0.99

	details = requestDetails{
		Timestamps: asTimestamps(dt, now),
	}

	if site.circuit != nil {
		if pMaxImp := site.circuit.GetMaxPower(); pMaxImp > 0 {
			// hard grid import limit if no price penalty is set by PrcPExcImp
			req.Grid.PMaxImp = float32(pMaxImp)
		}
	}

	// static grid export limit configured in the UI: export is capped at this
	// power, excess PV is curtailed instead of exported
	if limit := site.GetGridExportLimit(); limit > 0 {
		req.Grid.PMaxExp = float32(limit)
	}

	// soft grid feed-in cap from active HEMS curtailment (e.g. German 70% rule)
	// wins over the static limit while active
	if curtailed := hems.Curtailed(site.hems); curtailed != nil && *curtailed {
		if pMaxExp := site.hems.MaxProductionPower(); pMaxExp != nil {
			req.Grid.PMaxExp = float32(*pMaxExp)
		}
	}

	var batteries []optimizerBattery

	// uncontrollable power of loadpoints that cannot be modelled as storage
	var unmodelled float64

	for id, lp := range site.ActiveLoadpoints() {
		// ignore disconnected loadpoints, including StatusNone
		if s := lp.GetStatus(); s != api.StatusB && s != api.StatusC {
			continue
		}

		// heating loadpoints are already accounted for by their demand forecast
		if slices.Contains(heaters, lp) {
			continue
		}

		// no vehicle capacity and no session energy limit to model against:
		// account for the consumption as uncontrollable load
		if v := lp.GetVehicle(); v == nil || (v.Capacity() == 0 && lp.GetLimitEnergy() == 0) {
			unmodelled += unmodelledPower(lp)
			continue
		}

		// skip disabled loadpoints
		if cfg, detail := site.loadpointRequest(lp, minLen, firstSlotDuration, grid); cfg.CMax > 0 {
			detail.loadpoint = &id
			batteries = append(batteries, optimizerBattery{cfg, detail})
		}
	}

	// home profile subtracts all loadpoint power, so unmodelled loadpoints would
	// leave the optimizer planning against surplus that is already consumed. Their
	// forecast is zero, so the measured power only decays into the near slots -
	// without a capacity there is no fill point to assert it any further.
	if unmodelled > 0 {
		load := make([]float64, minLen)
		blendMeasured(load, unmodelled/slotsPerHour, optimizerDecaySlots)

		site.log.DEBUG.Printf("optimizer: home slots updated with unmodelled %.0fW loadpoint load: %.0f", unmodelled, load[:min(optimizerDecaySlots, len(load))])

		for i, v := range prorate(load, firstSlotDuration) {
			req.TimeSeries.Gt[i] += v
		}
	}

	for i, dev := range site.batteryMeters {
		// measurements may lag the configured meters on an off-cycle trigger
		if i >= len(battery) {
			break
		}
		b := battery[i]

		if b.Capacity == nil || *b.Capacity == 0 || b.Soc == nil {
			continue
		}

		cfg, detail := site.batteryRequest(dev, b, grid, minLen, firstSlotDuration)
		batteries = append(batteries, optimizerBattery{cfg, detail})
	}

	for _, b := range batteries {
		b.cfg.PA = pa
		req.Batteries = append(req.Batteries, b.cfg)
		details.BatteryDetails = append(details.BatteryDetails, b.detail)
	}

	return req, details, nil
}

func (site *Site) optimizerUpdate(battery []types.Measurement) error {
	req, details, err := site.optimizerRequest(battery)
	if err != nil {
		return err
	}

	if len(req.Batteries) == 0 {
		// meters configured but measurements not in yet: retry instead of
		// consuming the slot gate
		if len(site.batteryMeters) > 0 {
			return errOptimizerNotReady
		}
		return nil // nothing to optimize
	}

	httpClient := request.NewClient(site.log)
	httpClient.Timeout = 90 * time.Second

	apiClient, err := optimizer.NewClientWithResponses(optimizerURI(), optimizer.WithHTTPClient(httpClient))
	if err != nil {
		return err
	}

	resp, err := apiClient.PostOptimizeChargeScheduleWithResponse(context.TODO(), req, func(_ context.Context, req *http.Request) error {
		if sponsor.IsAuthorizedForApi() {
			req.Header.Set("Authorization", "Bearer "+sponsor.Token)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if resp.StatusCode() != http.StatusOK {
		return apiError(resp)
	}

	// publish before the status check so the optimizer page stays available
	// for diagnosing non-optimal results
	site.publish("evopt", optimizerResult{
		Updated: time.Now(),
		Req:     req,
		Res:     *resp.JSON200,
		Details: details,
	})

	// feasible results are usable, they are just not proven optimal
	if status := resp.JSON200.Status; status != optimizer.Optimal && status != optimizer.Feasible {
		return errors.New(string(status))
	}

	if len(details.Timestamps) != len(req.TimeSeries.Dt) || len(req.Batteries) != len(resp.JSON200.Batteries) {
		return errors.New("inconsistent optimizer result dimensions")
	}

	now := time.Now()
	schedule := optimizerSchedule{timestamps: details.Timestamps, dt: req.TimeSeries.Dt}
	if schedule.activeSlot(now) < 0 {
		return errors.New("optimizer result expired")
	}

	site.applyOptimizerResult(req, details, *resp.JSON200, schedule, now, now)

	return nil
}

// optimizerSolve caches a solve so the control cycle can reapply it to a newer slot
type optimizerSolve struct {
	req       optimizer.OptimizationInput
	details   requestDetails
	res       optimizer.OptimizationResult
	schedule  optimizerSchedule
	slot      int       // last applied slot
	completed time.Time // solve completion, not last reapply
}

// setLastOptimizerSolve remembers a solve's inputs for reapplySuggestions
func (site *Site) setLastOptimizerSolve(solve *optimizerSolve) {
	site.Lock()
	defer site.Unlock()

	site.lastOptimizerSolve = solve
}

// reapplySuggestions re-derives the last solve's suggestions for the slot covering now
// without a new solve. TryLock so it never overwrites a fresher concurrent solve. The
// caller gates on optimizer enabled/sponsored, same as optimizerUpdateAsync.
func (site *Site) reapplySuggestions(now time.Time) {
	if !site.optimizerMu.TryLock() {
		return
	}
	defer site.optimizerMu.Unlock()

	site.RLock()
	last := site.lastOptimizerSolve
	site.RUnlock()

	if last == nil {
		return
	}

	slot := last.schedule.activeSlot(now)
	if slot == last.slot {
		return
	}

	// horizon passed or no completed solve within suggestionMaxAge: don't march a dead plan forward
	if slot < 0 || now.Sub(last.completed) > suggestionMaxAge {
		site.log.DEBUG.Println("optimizer: cached result expired")
		site.clearSuggestions()
		return
	}

	// disconnected loadpoints are excluded from a fresh solve; don't advise an empty charger
	for _, d := range last.details.BatteryDetails {
		if d.loadpoint == nil {
			continue
		}
		if lp := site.loadpoints[*d.loadpoint]; lp == nil || !lp.connected() {
			site.setLastOptimizerSolve(nil)
			return
		}
	}

	site.applyOptimizerResult(last.req, last.details, last.res, last.schedule, now, last.completed)
}

// applyOptimizerResult maps the optimizer response onto suggestions, battery
// forecast and notifications, for whichever slot covers now
func (site *Site) applyOptimizerResult(req optimizer.OptimizationInput, details requestDetails, res optimizer.OptimizationResult, schedule optimizerSchedule, now time.Time, completed time.Time) {
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

		suggestion := slotSuggestion(detail, batRes, slot, f, slotHours, gridImport, gridExport)
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

		// uncontrollable devices can't act on a suggestion
		if key := detail.key(); key != "" && detail.controllable {
			suggestions[key] = suggestion
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

	site.setLastOptimizerSolve(&optimizerSolve{req: req, details: details, res: res, schedule: schedule, slot: slot, completed: completed})
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

func (site *Site) loadpointRequest(lp loadpoint.API, minLen int, firstSlotDuration time.Duration, grid api.Rates) (optimizer.BatteryConfig, batteryDetail) {
	bat := optimizer.BatteryConfig{
		ChargeFromGrid: true,
		CMin:           float32(lp.EffectiveMinPower()),
		CMax:           float32(lp.EffectiveMaxPower()),
		DMax:           0,
		SMin:           0,
		// PA:             pa,
	}

	// breaks cost ties between the loadpoint and the home battery- without it, two equally
	// priced schedules can flip which one charges from one solve to the next, toggling the
	// charger for no real reason. Follows the same rule as the PV-surplus loop (site.go): the
	// home battery keeps priority below prioritySoc, the loadpoint wins once it is past that.
	if site.GetBatterySoc() >= site.GetPrioritySoc() {
		bat.CPriority = 1
	}

	if profile := loadpointProfile(lp, minLen); profile != nil {
		bat.PDemand = prorate(profile, firstSlotDuration)
	}

	detail := batteryDetail{
		Type:         batteryTypeLoadpoint,
		Title:        lp.GetTitle(),
		controllable: true,
	}

	// vehicle
	v := lp.GetVehicle()

	capacity := v.Capacity() // kWh
	soc := lp.GetSoc()       // percent

	// without capacity or soc there is no battery state to model, but a session energy
	// limit still bounds the charge- use charged energy as state (see remainingLimitEnergy)
	if limit := lp.GetLimitEnergy(); limit > 0 && (capacity == 0 || soc == 0) {
		bat.SInitial = float32(lp.GetChargedEnergy())    // Wh
		bat.SMax = max(bat.SInitial, float32(limit*1e3)) // prevent infeasible if limit already exceeded
	} else {
		maxSoc := capacity * float64(lp.EffectiveLimitSoc()) * 10 // Wh
		bat.SInitial = float32(capacity * soc * 10)               // Wh
		bat.SMax = max(bat.SInitial, float32(maxSoc))             // prevent infeasible if current soc above maximum
	}

	detail.Type = batteryTypeVehicle
	detail.Capacity = capacity

	if vt := v.GetTitle(); vt != "" {
		if detail.Title != "" {
			detail.Title += " (" + vt + ")"
		} else {
			detail.Title = vt
		}
	}

	// find vehicle name/id
	for _, dev := range config.Vehicles().Devices() {
		if dev.Instance() == v {
			detail.Name = dev.Config().Name
		}
	}

	var demand []float32

	switch lp.GetMode() {
	case api.ModeOff:
		// disable charging
		bat.CMax = 0

	case api.ModeNow:
		// forced max charging
		demand = continuousDemand(lp, minLen)

	case api.ModeSmart:
		if lp.GetAlwaysCharge().Active() {
			// forced min charging
			demand = continuousDemand(lp, minLen)
		}
		// add smartcost limit, precondition and plan goal, if configured
		demand = applySmartCostLimit(lp, demand, grid, minLen)
		demand = applyPrecondition(lp, demand, minLen)
		site.applyPlanGoal(lp, &bat, minLen)
	}

	if demand != nil {
		// after prorate, so the shortened first slot counts with the energy it really carries
		bat.PDemand = clearDemandWhenFull(prorate(demand, firstSlotDuration), bat.SMax-bat.SInitial)
	}

	return bat, detail
}

// clearDemandWhenFull zeroes the charge demand from the slot the accumulated energy fills the
// vehicle. The optimizer drops the demand at s_max anyway, but pays two binaries per slot to
// detect it, so slots that cannot bind are worth not asking about. Losses are accounted for.
//
// The cut assumes the demand is met every slot. A grid import limit can throttle charging below
// it, moving the real fill point later than the estimate - the next request corrects that from
// the measured soc, and the near slots are never affected because the cut sits a full charge away.
func clearDemandWhenFull(demand []float32, headroom float32) []float32 {
	res := slices.Clone(demand)

	var acc float32
	for i, d := range res {
		if acc >= headroom {
			res[i] = 0
			continue
		}
		acc += d * eta
	}

	return res
}

func (site *Site) batteryRequest(dev config.Device[api.Meter], b types.Measurement, grid api.Rates, minLen int, firstSlotDuration time.Duration) (optimizer.BatteryConfig, batteryDetail) {
	bat := optimizer.BatteryConfig{
		CMax:      batteryPower,
		DMax:      batteryPower,
		SCapacity: float32(*b.Capacity * 1e3),         // Wh
		SInitial:  float32(*b.Capacity * *b.Soc * 10), // Wh
		// PA:       pa,
	}

	instance := dev.Instance()

	ctrl, controllable := api.Cap[api.BatteryController](instance)
	if controllable {
		bat.ChargeFromGrid = slices.Contains(ctrl.BatteryModes(), api.BatteryCharge)
		bat.DischargeToGrid = site.GetBatteryGridDischarge()
	}

	if m, ok := api.Cap[api.BatteryPowerLimiter](instance); ok {
		charge, discharge := m.GetPowerLimits()
		bat.CMax = float32(charge)
		bat.DMax = float32(discharge)
	}

	if m, ok := api.Cap[api.BatterySocLimiter](instance); ok {
		minSoc, maxSoc := m.GetSocLimits()
		if maxSoc == 0 {
			maxSoc = 100 // empty/unset maxsoc means no upper limit
		}
		// clamp against current soc to prevent infeasible if it is outside the configured limits
		bat.SMin = min(bat.SInitial, float32(*b.Capacity*minSoc*10)) // Wh
		bat.SMax = max(bat.SInitial, float32(*b.Capacity*maxSoc*10)) // Wh
	}

	// mirrors the loadpoint side (see loadpointRequest): below prioritySoc the home battery
	// wins a cost tie against a concurrently charging loadpoint, past it the loadpoint wins
	if site.GetBatterySoc() < site.GetPrioritySoc() {
		bat.CPriority = 1
	}

	detail := batteryDetail{
		Type:         batteryTypeBattery,
		Name:         dev.Config().Name,
		Title:        deviceProperties(dev).Title,
		Capacity:     *b.Capacity,
		controllable: controllable,
	}

	// tariff forecast-based grid charging demand
	if bat.ChargeFromGrid {
		if demand := site.applyBatteryGridChargeLimit(bat.CMax, grid, minLen); demand != nil {
			bat.PDemand = prorate(demand, firstSlotDuration)
		}
	}

	// nudge the optimizer away from parking the battery at very high SOC (calendar aging)
	bat.PrcDplSocHigh = socDepletionCostHigh

	// nudge the optimizer to keep a reserve buffer off the low floor (comfort, not aging) so a
	// spontaneous load or forecast deviation is covered from the battery, not a grid purchase.
	// testing only: defaults on (see site.go), site.GetSocDepletionCostLowEnabled() lets it be
	// switched off live for evaluation.
	if site.GetSocDepletionCostLowEnabled() {
		bat.PrcDplSocLow = socDepletionCostLowDefault
	}

	// battery_first (optimizer PR 126) moved from a per-battery field to a single
	// strategy-level flag shared by the whole request - home batteries and any
	// concurrently charging EV loadpoint alike, since both land in the same req.Batteries.
	// Controlled A/B runs against real production requests (identical schedules, W-for-W,
	// with the flag on and off under attenuate_grid_peaks) found no measurable effect: PA
	// via PrcDplSocHigh/Low already keeps the battery off both floor and ceiling, and PR
	// 130's level-deviation leveling on its own already prefers a spread schedule over an
	// arbitrary one, leaving no genuine cost-neutral tie left for earliness to break. Not
	// worth the now-shared-with-EVs blast radius for an unproven effect - left unset.

	return bat, detail
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

// continuousDemand creates a slice of power demands depending on loadpoint mode
func continuousDemand(lp loadpoint.API, minLen int) []float32 {
	if lp.GetStatus() != api.StatusC {
		return nil
	}

	pwr := lp.EffectiveMaxPower()
	if loadpoint.AlwaysChargeActive(lp) {
		pwr = lp.EffectiveMinPower()
	}

	return lo.RepeatBy(minLen, func(i int) float32 {
		return float32(pwr / slotsPerHour)
	})
}

// loadpointProfile returns the loadpoint's charging profile in Wh
// TODO consider charging efficiency
func loadpointProfile(lp loadpoint.API, minLen int) []float64 {
	minActive := loadpoint.AlwaysChargeActive(lp)

	if lp.GetStatus() != api.StatusC || (!minActive && lp.GetMode() != api.ModeNow) {
		return nil
	}

	power := lp.GetChargePower()
	if minP := lp.EffectiveMinPower(); minActive && minP < power {
		power = minP
	}

	energy := lp.GetRemainingEnergy() * 1e3 // Wh
	energyKnown := energy > 0

	res := make([]float64, 0, minLen)
	for range minLen {
		deltaEnergy := power * float64(tariff.SlotDuration) / float64(time.Hour) // Wh
		if energyKnown && deltaEnergy >= energy {
			deltaEnergy = energy
		}
		energy -= deltaEnergy

		res = append(res, deltaEnergy)
	}

	return res
}

// unmodelledPower returns the uncontrollable power of a connected loadpoint that
// cannot be modelled as storage because the vehicle capacity is unknown
func unmodelledPower(lp loadpoint.API) float64 {
	power := lp.GetChargePower()

	// always charge keeps drawing at least min power while the vehicle is connected,
	// even before the charge meter has caught up
	if loadpoint.AlwaysChargeActive(lp) && lp.GetStatus() == api.StatusC {
		power = max(power, lp.EffectiveMinPower())
	}

	return max(0, power)
}

// measuredSlotEnergy returns the summed energy in Wh of the last completed
// metrics slot for the given collector refs, 0 when not available
func (site *Site) measuredSlotEnergy(refs ...string) float64 {
	var sum float64
	for _, ref := range refs {
		c, ok := site.collectors[ref]
		if !ok {
			return 0
		}

		v, ok := c.LastSlotEnergy()
		if !ok {
			return 0
		}
		sum += v
	}

	return sum * 1e3
}

// blendMeasured decays the first slots from the measured value into the
// forecast. Slot 0 uses the measured value, the forecast takes over from
// slot decaySlots on.
func blendMeasured[T constraints.Float](slots []T, measured T, decaySlots int) {
	for i := range min(decaySlots, len(slots)) {
		w := T(decaySlots-i) / T(decaySlots)
		slots[i] = w*measured + (1-w)*slots[i]
	}
}

// blendScale decays a scale factor towards 1 over the first slots.
// Slot 0 is scaled by the full factor, from slot decaySlots on it is 1.
func blendScale[T constraints.Float](slots []T, scale float64, decaySlots int) {
	for i := range min(decaySlots, len(slots)) {
		w := float64(decaySlots-i) / float64(decaySlots)
		slots[i] = T(float64(slots[i]) * (w*scale + (1 - w)))
	}
}

// prorate adjusts the first slot's energy amount according to remaining duration
func prorate[T constraints.Float](slots []T, firstSlotDuration time.Duration) []float32 {
	// return empty slice instead of nil to make api happy
	if len(slots) == 0 {
		return []float32{}
	}

	res := slices.Clone(slots)
	res[0] = res[0] * T(firstSlotDuration) / T(tariff.SlotDuration)
	return lo.Map(res, func(f T, _ int) float32 {
		return float32(f)
	})
}

func solarRatesToEnergy(rr api.Rates) (api.Rates, error) {
	res := make(api.Rates, 0, len(rr))

	for _, r := range rr {
		energy := solarEnergy(rr, r.Start, r.End)
		if energy < 0 {
			return nil, fmt.Errorf("negative solar energy from %v to %v: %.3f", r.Start, r.End, energy)
		}

		res = append(res, api.Rate{
			Start: r.Start,
			End:   r.End,
			Value: energy,
		})
	}

	return res, nil
}

func currentRates(tariff api.Tariff) api.Rates {
	if tariff == nil {
		return nil
	}

	rates, err := tariff.Rates()
	if err != nil {
		return nil
	}

	// filter past slots
	now := time.Now()
	return lo.Filter(rates, func(slot api.Rate, _ int) bool {
		return slot.End.After(now)
	})
}

// optimizerHorizon is the timeframe the hosted optimizer is limited to for sake
// of performance: 48 hours, extended to the end of that day. In the early hours
// the extension would add almost a full day, hence it only applies past 6:00.
func optimizerHorizon(t time.Time) time.Time {
	horizon := t.Add(48 * time.Hour)
	if t.Hour() < 6 {
		return horizon
	}
	return now.With(horizon).EndOfDay()
}

// slotsUntil limits maxLen to the slots starting before the given horizon
func slotsUntil(rates api.Rates, horizon time.Time, maxLen int) int {
	if i := slices.IndexFunc(rates[:min(maxLen, len(rates))], func(slot api.Rate) bool {
		return slot.Start.After(horizon)
	}); i >= 0 {
		return i
	}
	return maxLen
}

func timeSteps(minLen int, now time.Time) []int {
	res := make([]int, 0, minLen)

	eos := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	if d := eos.Sub(now); d > time.Second && d < tariff.SlotDuration {
		res = append(res, int(d.Seconds()))
	}

	for i := len(res); i < minLen; i++ {
		res = append(res, int(tariff.SlotDuration.Seconds())) // 15min slots
	}

	return res
}

func asTimestamps(dt []int, now time.Time) []time.Time {
	res := make([]time.Time, 0, len(dt))

	eos := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	res = append(res, eos.Add(-time.Duration(dt[0])*time.Second))

	for i := range len(dt) - 1 {
		res = append(res, res[i].Add(time.Duration(dt[i])*time.Second))
	}

	return res
}

func scaleAndPrune(rates api.Rates, scale float64, maxLen int) []float32 {
	res := make([]float32, 0, maxLen)

	for _, slot := range rates {
		res = append(res, float32(slot.Value*scale))
		if len(res) >= maxLen {
			break
		}
	}

	return res
}

func (site *Site) applyPlanGoal(lp loadpoint.API, bat *optimizer.BatteryConfig, minLen int) {
	goal, socBased := lp.GetPlanGoal()
	if goal <= 0 {
		return
	}

	// Convert to Wh
	if vehicle := lp.GetVehicle(); socBased && vehicle != nil {
		goal *= vehicle.Capacity() * 10
	} else {
		goal *= 1000 // Wh
	}

	ts := lp.EffectivePlanTime()
	if ts.IsZero() {
		return
	}

	// TODO precise slot placement
	slot := int(time.Until(ts) / tariff.SlotDuration)
	if slot >= 0 && slot < minLen {
		bat.SGoal = make([]float32, minLen)
		bat.SGoal[slot] = float32(goal)
		bat.SMax = max(bat.SMax, float32(goal))
	} else {
		site.log.DEBUG.Printf("plan beyond forecast range or overrun: %.1f at %v slot %d", goal, ts.Round(time.Minute), slot)
	}
}

// TODO remove once smart cost limit usage becomes obsolete
func applySmartCostLimit(lp loadpoint.API, demand []float32, grid api.Rates, minLen int) []float32 {
	costLimit := lp.GetSmartCostLimit()
	if costLimit == nil {
		return demand
	}

	maxLen := min(minLen, len(grid))

	// Check if any slots meet the cost limit
	if hasAffordableSlots := slices.ContainsFunc(grid[:maxLen], func(r api.Rate) bool {
		return r.Value <= *costLimit
	}); !hasAffordableSlots {
		return demand
	}

	maxPower := lp.EffectiveMaxPower()

	if demand == nil {
		demand = make([]float32, minLen)
	}

	for i := range maxLen {
		if grid[i].Value <= *costLimit {
			demand[i] = float32(maxPower / slotsPerHour)
		}
		// else: keep existing demand (either 0 or minPower from always charge)
	}

	return demand
}

// applyPrecondition forces max charging power during the planner's precondition window
// ("late charging"), i.e. the last precondition duration before the plan time
func applyPrecondition(lp loadpoint.API, demand []float32, minLen int) []float32 {
	precondition := lp.EffectivePlanStrategy().Precondition
	if precondition <= 0 {
		return demand
	}

	ts := lp.EffectivePlanTime()
	if ts.IsZero() {
		return demand
	}

	// limit to the required charging duration, i.e. "all" must not demand beyond the plan goal
	goal, _ := lp.GetPlanGoal()
	if required := lp.GetPlanRequiredDuration(goal, lp.EffectiveMaxPower()); required < precondition {
		precondition = required
	}
	if precondition <= 0 {
		return demand
	}

	// TODO precise slot placement
	end := time.Until(ts)
	start := end - precondition
	if end <= 0 {
		return demand
	}

	first := max(int(start/tariff.SlotDuration), 0)
	if first >= minLen {
		return demand
	}

	if demand == nil {
		demand = make([]float32, minLen)
	}

	energy := float32(lp.EffectiveMaxPower() / slotsPerHour)

	for i := first; i < minLen; i++ {
		slotStart := time.Duration(i) * tariff.SlotDuration
		overlap := min(end, slotStart+tariff.SlotDuration) - max(start, slotStart)
		if overlap <= 0 {
			break
		}

		demand[i] = max(demand[i], energy*float32(overlap)/float32(tariff.SlotDuration))
	}

	return demand
}

func (site *Site) applyBatteryGridChargeLimit(cMax float32, grid api.Rates, minLen int) []float32 {
	limit := site.GetBatteryGridChargeLimit()
	if limit == nil {
		return nil
	}

	maxLen := min(minLen, len(grid))

	if hasAffordableSlots := slices.ContainsFunc(grid[:maxLen], func(r api.Rate) bool {
		return r.Value <= *limit
	}); !hasAffordableSlots {
		return nil
	}

	demand := make([]float32, minLen)
	for i := range maxLen {
		if grid[i].Value <= *limit {
			demand[i] = float32(float64(cMax) / slotsPerHour)
		}
	}

	return demand
}

// apiError extracts error message from optimizer API response
func apiError(resp *optimizer.PostOptimizeChargeScheduleResponse) error {
	var errObj *optimizer.Error
	switch resp.StatusCode() {
	case http.StatusBadRequest:
		errObj = resp.JSON400
	case http.StatusInternalServerError:
		errObj = resp.JSON500
	}

	if errObj == nil {
		return fmt.Errorf("invalid status: %d: %s", resp.StatusCode(), resp.Body)
	}

	if len(errObj.Details) > 0 {
		var details []string
		for field, msg := range errObj.Details {
			details = append(details, fmt.Sprintf("%s: %s", field, msg))
		}
		slices.Sort(details)
		return fmt.Errorf("%s (%s)", errObj.Message, strings.Join(details, ", "))
	}

	return errors.New(errObj.Message)
}
