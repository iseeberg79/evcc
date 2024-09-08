package charger

// Code generated by github.com/evcc-io/evcc/cmd/tools/decorate.go. DO NOT EDIT.

import (
	"github.com/evcc-io/evcc/api"
)

func decorateCfos(base *CfosPowerBrain, meter func() (float64, error), meterEnergy func() (float64, error), phaseCurrents func() (float64, float64, float64, error), phaseSwitcher func(int) error) api.Charger {
	switch {
	case meter == nil && phaseSwitcher == nil:
		return base

	case meter != nil && meterEnergy == nil && phaseCurrents == nil && phaseSwitcher == nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
		}

	case meter != nil && meterEnergy != nil && phaseCurrents == nil && phaseSwitcher == nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
			api.MeterEnergy
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
			MeterEnergy: &decorateCfosMeterEnergyImpl{
				meterEnergy: meterEnergy,
			},
		}

	case meter != nil && meterEnergy == nil && phaseCurrents != nil && phaseSwitcher == nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
			api.PhaseCurrents
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
			PhaseCurrents: &decorateCfosPhaseCurrentsImpl{
				phaseCurrents: phaseCurrents,
			},
		}

	case meter != nil && meterEnergy != nil && phaseCurrents != nil && phaseSwitcher == nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
			api.MeterEnergy
			api.PhaseCurrents
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
			MeterEnergy: &decorateCfosMeterEnergyImpl{
				meterEnergy: meterEnergy,
			},
			PhaseCurrents: &decorateCfosPhaseCurrentsImpl{
				phaseCurrents: phaseCurrents,
			},
		}

	case meter == nil && phaseSwitcher != nil:
		return &struct {
			*CfosPowerBrain
			api.PhaseSwitcher
		}{
			CfosPowerBrain: base,
			PhaseSwitcher: &decorateCfosPhaseSwitcherImpl{
				phaseSwitcher: phaseSwitcher,
			},
		}

	case meter != nil && meterEnergy == nil && phaseCurrents == nil && phaseSwitcher != nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
			api.PhaseSwitcher
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
			PhaseSwitcher: &decorateCfosPhaseSwitcherImpl{
				phaseSwitcher: phaseSwitcher,
			},
		}

	case meter != nil && meterEnergy != nil && phaseCurrents == nil && phaseSwitcher != nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
			api.MeterEnergy
			api.PhaseSwitcher
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
			MeterEnergy: &decorateCfosMeterEnergyImpl{
				meterEnergy: meterEnergy,
			},
			PhaseSwitcher: &decorateCfosPhaseSwitcherImpl{
				phaseSwitcher: phaseSwitcher,
			},
		}

	case meter != nil && meterEnergy == nil && phaseCurrents != nil && phaseSwitcher != nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
			api.PhaseCurrents
			api.PhaseSwitcher
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
			PhaseCurrents: &decorateCfosPhaseCurrentsImpl{
				phaseCurrents: phaseCurrents,
			},
			PhaseSwitcher: &decorateCfosPhaseSwitcherImpl{
				phaseSwitcher: phaseSwitcher,
			},
		}

	case meter != nil && meterEnergy != nil && phaseCurrents != nil && phaseSwitcher != nil:
		return &struct {
			*CfosPowerBrain
			api.Meter
			api.MeterEnergy
			api.PhaseCurrents
			api.PhaseSwitcher
		}{
			CfosPowerBrain: base,
			Meter: &decorateCfosMeterImpl{
				meter: meter,
			},
			MeterEnergy: &decorateCfosMeterEnergyImpl{
				meterEnergy: meterEnergy,
			},
			PhaseCurrents: &decorateCfosPhaseCurrentsImpl{
				phaseCurrents: phaseCurrents,
			},
			PhaseSwitcher: &decorateCfosPhaseSwitcherImpl{
				phaseSwitcher: phaseSwitcher,
			},
		}
	}

	return nil
}

type decorateCfosMeterImpl struct {
	meter func() (float64, error)
}

func (impl *decorateCfosMeterImpl) CurrentPower() (float64, error) {
	return impl.meter()
}

type decorateCfosMeterEnergyImpl struct {
	meterEnergy func() (float64, error)
}

func (impl *decorateCfosMeterEnergyImpl) TotalEnergy() (float64, error) {
	return impl.meterEnergy()
}

type decorateCfosPhaseCurrentsImpl struct {
	phaseCurrents func() (float64, float64, float64, error)
}

func (impl *decorateCfosPhaseCurrentsImpl) Currents() (float64, float64, float64, error) {
	return impl.phaseCurrents()
}

type decorateCfosPhaseSwitcherImpl struct {
	phaseSwitcher func(int) error
}

func (impl *decorateCfosPhaseSwitcherImpl) Phases1p3p(p0 int) error {
	return impl.phaseSwitcher(p0)
}
