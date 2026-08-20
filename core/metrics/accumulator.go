package metrics

import (
	"bytes"
	"fmt"
	"time"

	"github.com/benbjohnson/clock"
)

type Accumulator struct {
	clock             clock.Clock
	updated           time.Time
	energyMeter       *float64 // kWh
	returnEnergyMeter *float64 // kWh
	Energy            float64  `json:"energy"`       // kWh
	ReturnEnergy      float64  `json:"returnEnergy"` // kWh
	SocTemp           *float64 `json:"socTemp,omitempty"`
}

// AccumulatorState is the resumable meter-reading checkpoint of an Accumulator.
type AccumulatorState struct {
	EnergyMeter       *float64 // kWh, last absolute reading
	ReturnEnergyMeter *float64 // kWh, last absolute reading
}

// Snapshot returns the current meter readings for persistence.
func (m *Accumulator) Snapshot() AccumulatorState {
	return AccumulatorState{EnergyMeter: m.energyMeter, ReturnEnergyMeter: m.returnEnergyMeter}
}

// Restore seeds the meter readings so the first delta covers the downtime.
func (m *Accumulator) Restore(s AccumulatorState) {
	m.energyMeter = s.EnergyMeter
	m.returnEnergyMeter = s.ReturnEnergyMeter
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

func (m *Accumulator) String() string {
	b := new(bytes.Buffer)
	fmt.Fprintf(b, "Accumulated: %.3fkWh energy, %.3fkWh return energy, updated: %v", m.Energy, m.ReturnEnergy, m.updated.Truncate(time.Second))
	if m.energyMeter != nil || m.returnEnergyMeter != nil {
		fmt.Fprintf(b, " energy total:")
		if m.energyMeter != nil {
			fmt.Fprintf(b, " %.3fkWh", *m.energyMeter)
		}
		if m.returnEnergyMeter != nil {
			fmt.Fprintf(b, " %.3fkWh return energy", *m.returnEnergyMeter)
		}
	}
	return b.String()
}

// meterTotalNoiseFloor absorbs backward jitter that is not a real decrease
// (register/timing noise, or - as seen with some Shelly PV meters, see
// meter/shelly/gen2.go's TotalEnergy - a value derived by subtracting two
// independently accumulating counters). A meter sitting at a near-constant total
// for a long stretch (e.g. return energy while barely exporting) would otherwise
// trip the torn-read guard on that noise alone, every poll cycle. Real
// torn/implausible reads seen in practice are orders of magnitude larger (a
// baseline reset or garbled register), so this stays well clear of masking them.
const meterTotalNoiseFloor = 1e-3 // kWh

// floatEpsilon absorbs float64 representation error in the noise-floor
// comparison itself - e.g. 166.570-166.569 is 0.00100000000000477485, not
// exactly meterTotalNoiseFloor, so a dip that is decimally exactly at the
// floor would otherwise randomly land on either side of it.
const floatEpsilon = 1e-9 // kWh

// SetEnergyMeterTotal adds the difference to the last total meter value in
// kWh. A cumulative counter cannot run backwards, so a decrease relative to
// the last known total (beyond meterTotalNoiseFloor) is treated as a torn or
// implausible read and ignored without moving the baseline - the next valid
// reading is then still measured against the last known-good total instead of
// booking the recovery as one spike. Returns false in that case so the caller
// can log it for diagnosis, true otherwise, including for the very first
// reading (no baseline to compare against) and a within-noise-floor dip
// (nothing to diagnose).
func (m *Accumulator) SetEnergyMeterTotal(v float64) bool {
	defer func() { m.updated = m.clock.Now() }()

	if m.energyMeter == nil {
		m.energyMeter = new(v)
		return true
	}

	if v < *m.energyMeter {
		return *m.energyMeter-v <= meterTotalNoiseFloor+floatEpsilon
	}

	m.Energy += v - *m.energyMeter
	m.energyMeter = new(v)

	return true
}

// SetReturnEnergyMeterTotal adds the difference to the last total meter value
// in kWh (see SetEnergyMeterTotal for the return value).
func (m *Accumulator) SetReturnEnergyMeterTotal(v float64) bool {
	defer func() { m.updated = m.clock.Now() }()

	if m.returnEnergyMeter == nil {
		m.returnEnergyMeter = new(v)
		return true
	}

	if v < *m.returnEnergyMeter {
		return *m.returnEnergyMeter-v <= meterTotalNoiseFloor+floatEpsilon
	}

	m.ReturnEnergy += v - *m.returnEnergyMeter
	m.returnEnergyMeter = new(v)

	return true
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
