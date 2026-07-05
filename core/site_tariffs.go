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

const (
	solarScaleWindow  = 28 // days of completed history to consider
	solarScaleMinDays = 5  // require at least this many usable days, else no adjustment
)

// solarScale returns the median of the daily produced/forecasted ratios over a
// trailing window of completed days. Used to adjust the forecast for a
// systematic installation bias (soiling, shading, model error). Deliberately
// not a single-day ratio: today's deviation is weather noise and must not be
// projected onto future days. The median ignores outlier days (metering
// outages, broken forecast feeds) without any explicit filtering. The trailing
// window follows slow drift and forgets old data. Returns 1 until enough days
// are available.
func (site *Site) solarScale() float64 {
	// completed days only: to is exclusive, so today's partial day is skipped
	from := now.BeginningOfDay().AddDate(0, 0, -solarScaleWindow)
	series, err := metrics.QueryEnergy(from, now.BeginningOfDay(), "day", true)
	if err != nil {
		site.log.ERROR.Printf("solar forecast scale: %v", err)
		return 1
	}

	// index produced energy per day, pairing with forecast below
	pv := make(map[string]float64)
	for _, s := range series {
		if s.Group == metrics.PV {
			for _, d := range s.Data {
				pv[d.Start.Format("2006-01-02")] += d.Energy
			}
		}
	}

	var ratios []float64
	for _, s := range series {
		if s.Group != metrics.Forecast {
			continue
		}
		for _, d := range s.Data {
			// require data on both sides; the median tolerates the rest
			if p := pv[d.Start.Format("2006-01-02")]; p > 0 && d.Energy > 0 {
				ratios = append(ratios, p/d.Energy)
			}
		}
	}

	scale := solarScaleFactor(ratios)
	if scale != 1 {
		site.log.DEBUG.Printf("solar forecast scale %.3f (median of %d days)", scale, len(ratios))
	}
	return scale
}

// solarScaleFactor returns the median of the given daily ratios, or 1 when
// there are fewer than solarScaleMinDays of them.
func solarScaleFactor(ratios []float64) float64 {
	if len(ratios) < solarScaleMinDays {
		return 1
	}
	slices.Sort(ratios)
	return ratios[len(ratios)/2]
}

func (site *Site) isDynamicTariff(usage api.TariffUsage) bool {
	tariff := site.GetTariff(usage)
	return tariff != nil && tariff.Type() != api.TariffTypePriceStatic
}
