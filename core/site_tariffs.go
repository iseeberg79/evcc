package core

import (
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/jinzhu/now"
)

type solarDetails struct {
	Scale            *float64     `json:"scale,omitempty"`            // scale factor yield/forecasted today
	Today            dailyDetails `json:"today,omitempty"`            // tomorrow
	Tomorrow         dailyDetails `json:"tomorrow,omitempty"`         // tomorrow
	DayAfterTomorrow dailyDetails `json:"dayAfterTomorrow,omitempty"` // day after tomorrow
	Timeseries       timeseries   `json:"timeseries,omitempty"`       // timeseries of forecasted energy
}

type dailyDetails struct {
	Yield    float64 `json:"energy"`
	Complete bool    `json:"complete"`
}

// greenShare returns
//   - the current green share, calculated for the part of the consumption between powerFrom and powerTo
//     the consumption below powerFrom will get the available green power first
func (site *Site) greenShare(powerFrom float64, powerTo float64) float64 {
	greenPower := math.Max(0, site.pvPower) + math.Max(0, site.battery.Power)
	greenPowerAvailable := math.Max(0, greenPower-powerFrom)

	power := powerTo - powerFrom
	share := math.Min(greenPowerAvailable, power) / power

	if math.IsNaN(share) {
		if greenPowerAvailable > 0 {
			share = 1
		} else {
			share = 0
		}
	}

	return share
}

// effectivePrice calculates the real energy price based on self-produced and grid-imported energy.
func (site *Site) effectivePrice(greenShare float64) *float64 {
	if grid, err := tariff.Now(site.GetTariff(api.TariffUsageGrid)); err == nil {
		feedin, err := tariff.Now(site.GetTariff(api.TariffUsageFeedIn))
		if err != nil {
			feedin = 0
		}
		effPrice := grid*(1-greenShare) + feedin*greenShare
		return &effPrice
	}
	return nil
}

// effectiveCo2 calculates the amount of emitted co2 based on self-produced and grid-imported energy.
func (site *Site) effectiveCo2(greenShare float64) *float64 {
	if co2, err := tariff.Now(site.GetTariff(api.TariffUsageCo2)); err == nil {
		effCo2 := co2 * (1 - greenShare)
		return &effCo2
	}
	return nil
}

func (site *Site) publishTariffs(greenShareHome float64, greenShareLoadpoints float64) {
	site.publish(keys.GreenShareHome, greenShareHome)
	site.publish(keys.GreenShareLoadpoints, greenShareLoadpoints)

	if v, err := tariff.Now(site.GetTariff(api.TariffUsageGrid)); err == nil {
		site.publish(keys.TariffGrid, v)
	}
	if v, err := tariff.Now(site.GetTariff(api.TariffUsageFeedIn)); err == nil {
		site.publish(keys.TariffFeedIn, v)
	}
	if v, err := tariff.Now(site.GetTariff(api.TariffUsageCo2)); err == nil {
		site.publish(keys.TariffCo2, v)
	}
	if v, err := tariff.Now(site.GetTariff(api.TariffUsageSolar)); err == nil {
		site.publish(keys.TariffSolar, v)
	}
	if v := site.effectivePrice(greenShareHome); v != nil {
		site.publish(keys.TariffPriceHome, v)
	}
	if v := site.effectiveCo2(greenShareHome); v != nil {
		site.publish(keys.TariffCo2Home, v)
	}
	if v := site.effectivePrice(greenShareLoadpoints); v != nil {
		site.publish(keys.TariffPriceLoadpoints, v)
	}
	if v := site.effectiveCo2(greenShareLoadpoints); v != nil {
		site.publish(keys.TariffCo2Loadpoints, v)
	}

	fc := struct {
		Co2     api.Rates     `json:"co2,omitempty"`
		FeedIn  api.Rates     `json:"feedin,omitempty"`
		Grid    api.Rates     `json:"grid,omitempty"`
		Planner api.Rates     `json:"planner,omitempty"`
		Solar   *solarDetails `json:"solar,omitempty"`
	}{
		Co2:     tariff.Rates(site.GetTariff(api.TariffUsageCo2)),
		FeedIn:  tariff.Rates(site.GetTariff(api.TariffUsageFeedIn)),
		Planner: tariff.Rates(site.GetTariff(api.TariffUsagePlanner)),
		Grid:    tariff.Rates(site.GetTariff(api.TariffUsageGrid)),
	}

	// calculate adjusted solar rates
	if solar := tariff.Rates(site.GetTariff(api.TariffUsageSolar)); len(solar) > 0 {
		fc.Solar = new(site.solarDetails(solar))
	}

	site.publish(keys.Forecast, util.NewSharder(keys.Forecast, fc))
}

func (site *Site) solarDetails(solar api.Rates) solarDetails {
	res := solarDetails{
		Timeseries: solarTimeseries(solar),
	}

	last := solar[len(solar)-1].Start

	bod := now.BeginningOfDay()
	eod := bod.AddDate(0, 0, 1)
	eot := eod.AddDate(0, 0, 1)

	remainingToday := solarEnergy(solar, time.Now(), eod)
	tomorrow := solarEnergy(solar, eod, eot)
	dayAfterTomorrow := solarEnergy(solar, eot, eot.AddDate(0, 0, 1))

	res.Today = dailyDetails{
		Yield:    remainingToday,
		Complete: !last.Before(eod),
	}
	res.Tomorrow = dailyDetails{
		Yield:    tomorrow,
		Complete: !last.Before(eot),
	}
	res.DayAfterTomorrow = dailyDetails{
		Yield:    dayAfterTomorrow,
		Complete: !last.Before(eot.AddDate(0, 0, 1)),
	}

	if r, err := solar.At(time.Now()); err == nil {
		if err := site.collectors[metrics.Forecast].AddEnergy(nil, nil, r.Value); err != nil {
			site.log.ERROR.Printf("solar forecast collector: %v", err)
		}
	}

	if scale := site.solarScale(); scale != 1 {
		res.Scale = &scale
	}

	return res
}

// solarScale returns the ratio of produced solar energy to forecasted solar
// energy for the current day, queried from the metrics database. Used to
// adjust forecasts when PV is consistently under-/over-producing relative
// to the forecast. Returns 1.0 when not enough data is available to make
// the ratio meaningful.
func (site *Site) solarScale() float64 {
	series, err := metrics.QueryEnergy(now.BeginningOfDay(), time.Now(), "day", true)
	if err != nil {
		site.log.ERROR.Printf("solar forecast scale: %v", err)
		return 1
	}

	var pv, fcst float64
	for _, s := range series {
		if len(s.Data) == 0 {
			continue
		}
		switch s.Group {
		case metrics.PV:
			pv = s.Data[0].Energy
		case metrics.Forecast:
			fcst = s.Data[0].Energy
		}
	}

	const minEnergy = 0.5 // kWh
	if fcst <= 0 || pv+fcst <= minEnergy {
		return 1
	}

	scale := pv / fcst
	site.log.DEBUG.Printf("solar forecast: produced %.3fkWh, forecasted %.3fkWh, scale %.3f", pv, fcst, scale)
	return scale
}

const (
	consumptionMarginDays       = 90   // trailing window of days for the reserve percentile
	consumptionMarginBaseline   = 30   // days averaged for the per-day forecast baseline (matches homeProfile window)
	consumptionMarginPercentile = 0.80 // plan to cover this share of days
	consumptionMarginMinSamples = 14   // minimum ratio samples before applying a margin
)

// consumptionMargin returns a multiplier (>= 1) for the forecast home consumption so
// the optimizer sizes the battery with reserve for spontaneous loads. It is the
// consumptionMarginPercentile of the daily ratio between actual consumption and its
// own trailing consumptionMarginBaseline-day mean (the baseline homeProfile forecasts
// from), over the last consumptionMarginDays. Unlike solarScale (a median-style
// correction) this is a percentile buffer, floored at 1 so it never plans for less
// than the forecast. Returns 1 when there is not enough history.
func (site *Site) consumptionMargin() float64 {
	from := now.BeginningOfDay().AddDate(0, 0, -(consumptionMarginDays + consumptionMarginBaseline))
	series, err := metrics.QueryEnergy(from, time.Now(), "day", true)
	if err != nil {
		site.log.ERROR.Printf("consumption margin: %v", err)
		return 1
	}

	var daily []metrics.Slot
	for _, s := range series {
		if s.Group == metrics.Home {
			daily = s.Data
			break
		}
	}
	slices.SortFunc(daily, func(a, b metrics.Slot) int { return a.Start.Compare(b.Start) })
	// drop the current (partial) day so it does not depress the ratios
	if n := len(daily); n > 0 && !daily[n-1].Start.Before(now.BeginningOfDay()) {
		daily = daily[:n-1]
	}

	energies := make([]float64, len(daily))
	for i, d := range daily {
		energies[i] = d.Energy
	}

	margin, samples := consumptionReserveMargin(energies)
	if samples > 0 {
		site.log.DEBUG.Printf("consumption margin: P%.0f over %d days = %.2f", consumptionMarginPercentile*100, samples, margin)
	}
	return margin
}

// consumptionReserveMargin computes the reserve multiplier from a chronological
// series of daily home consumption. It is the consumptionMarginPercentile of the
// ratio between each day and its trailing consumptionMarginBaseline-day mean,
// floored at 1. Returns (1, 0) when there is not enough history to be meaningful.
func consumptionReserveMargin(daily []float64) (margin float64, samples int) {
	if len(daily) < consumptionMarginBaseline+consumptionMarginMinSamples {
		return 1, 0
	}

	ratios := make([]float64, 0, len(daily)-consumptionMarginBaseline)
	for i := consumptionMarginBaseline; i < len(daily); i++ {
		var sum float64
		for _, v := range daily[i-consumptionMarginBaseline : i] {
			sum += v
		}
		if mean := sum / consumptionMarginBaseline; mean > 0 {
			ratios = append(ratios, daily[i]/mean)
		}
	}
	if len(ratios) == 0 {
		return 1, 0
	}
	slices.Sort(ratios)
	return math.Max(1, ratios[int(consumptionMarginPercentile*float64(len(ratios)-1))]), len(ratios)
}

func (site *Site) isDynamicTariff(usage api.TariffUsage) bool {
	tariff := site.GetTariff(usage)
	return tariff != nil && tariff.Type() != api.TariffTypePriceStatic
}
