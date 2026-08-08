package metrics

import (
	"bytes"
	"fmt"
	"time"

	"github.com/benbjohnson/clock"
)

// rollbackConfirmations is the number of consecutive drops below a
// meterTotal's value required to treat it as a reset/rollover rather than a
// transient glitch (#29286, #30555).
const rollbackConfirmations = 2

// meterTotal tracks a cumulative meter reading (kWh), debouncing drops below
// the current value so a lone glitch can't rebase it.
type meterTotal struct {
	value    *float64
	rollback int
}

// apply folds v into the total, returning the resulting delta and whether v
// was accepted rather than withheld pending rollback confirmation.
func (m *meterTotal) apply(v float64) (delta float64, accepted bool) {
	if m.value == nil {
		m.value = new(v)
		return 0, true
	}

	if v < *m.value {
		if m.rollback++; m.rollback < rollbackConfirmations {
			return 0, false
		}
	} else {
		delta = v - *m.value
	}

	m.rollback = 0
	m.value = new(v)
	return delta, true
}

type Accumulator struct {
	clock        clock.Clock
	updated      time.Time
	energyTotal  meterTotal
	returnTotal  meterTotal
	Energy       float64  `json:"energy"`       // kWh
	ReturnEnergy float64  `json:"returnEnergy"` // kWh
	SocTemp      *float64 `json:"socTemp,omitempty"`
}

// AccumulatorState is the resumable meter-reading checkpoint of an Accumulator.
type AccumulatorState struct {
	EnergyMeter       *float64 // kWh, last absolute reading
	ReturnEnergyMeter *float64 // kWh, last absolute reading
}

// Snapshot returns the current meter readings for persistence.
func (m *Accumulator) Snapshot() AccumulatorState {
	return AccumulatorState{EnergyMeter: m.energyTotal.value, ReturnEnergyMeter: m.returnTotal.value}
}

// Restore seeds the meter readings so the first delta covers the downtime.
func (m *Accumulator) Restore(s AccumulatorState) {
	m.energyTotal.value = s.EnergyMeter
	m.returnTotal.value = s.ReturnEnergyMeter
}

// CompleteFor reports whether the state can seed a collector of the given group.
// Bidirectional groups need both readings for a complete restore.
func (s AccumulatorState) CompleteFor(group string) bool {
	if group == Battery || group == Grid {
		return s.EnergyMeter != nil && s.ReturnEnergyMeter != nil
	}
	return s.EnergyMeter != nil || s.ReturnEnergyMeter != nil
}

// setSocTemp keeps the first reading per slot.
func (m *Accumulator) setSocTemp(value float64) {
	if m.SocTemp == nil {
		m.SocTemp = &value
	}
}

func WithClock(clock clock.Clock) func(*Accumulator) {
	return func(m *Accumulator) {
		m.clock = clock
	}
}

func NewAccumulator(opt ...func(*Accumulator)) *Accumulator {
	m := &Accumulator{clock: clock.New()}
	for _, o := range opt {
		o(m)
	}
	return m
}

func (m *Accumulator) Updated() time.Time {
	return m.updated
}

func (m *Accumulator) String() string {
	b := new(bytes.Buffer)
	fmt.Fprintf(b, "Accumulated: %.3fkWh energy, %.3fkWh return energy, updated: %v", m.Energy, m.ReturnEnergy, m.updated.Truncate(time.Second))
	if m.energyTotal.value != nil || m.returnTotal.value != nil {
		fmt.Fprintf(b, " energy total:")
		if m.energyTotal.value != nil {
			fmt.Fprintf(b, " %.3fkWh", *m.energyTotal.value)
		}
		if m.returnTotal.value != nil {
			fmt.Fprintf(b, " %.3fkWh return energy", *m.returnTotal.value)
		}
	}
	return b.String()
}

// SetEnergyMeterTotal adds the difference to the last total meter value in kWh
func (m *Accumulator) SetEnergyMeterTotal(v float64) {
	if delta, ok := m.energyTotal.apply(v); ok {
		m.Energy += delta
		m.updated = m.clock.Now()
	}
}

// SetReturnEnergyMeterTotal adds the difference to the last total meter value in kWh
func (m *Accumulator) SetReturnEnergyMeterTotal(v float64) {
	if delta, ok := m.returnTotal.apply(v); ok {
		m.ReturnEnergy += delta
		m.updated = m.clock.Now()
	}
}

// AddEnergy adds the given energy in kWh to the energy total
func (m *Accumulator) AddEnergy(v float64) {
	defer func() { m.updated = m.clock.Now() }()

	if m.updated.IsZero() {
		return
	}

	m.Energy += v
}

// AddReturnEnergy adds the given energy in kWh to the return energy total
func (m *Accumulator) AddReturnEnergy(v float64) {
	defer func() { m.updated = m.clock.Now() }()

	if m.updated.IsZero() {
		return
	}

	m.ReturnEnergy += v
}

// AddPower adds the given power in W, calculating the energy based on the time since the last update
func (m *Accumulator) AddPower(v float64) {
	since := v * m.clock.Since(m.updated).Hours() / 1e3
	if v >= 0 {
		m.AddEnergy(since)
	} else {
		m.AddReturnEnergy(-since)
	}
}
