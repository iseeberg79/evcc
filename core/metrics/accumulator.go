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

	// stuck-meter fallback state, see noteEnergyMeterActivity/SetEnergyMeterTotal
	energyMeterStaleSince       time.Time
	returnEnergyMeterStaleSince time.Time
	energyMeterStale            bool
	returnEnergyMeterStale      bool

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

// SetEnergyMeterTotal adds the difference to the last total meter value in
// kWh. A reading that arrives while the meter is flagged stale (see
// noteEnergyMeterActivity) is not credited - that period was already bridged
// via power integration - it just resyncs the baseline and clears the flag.
func (m *Accumulator) SetEnergyMeterTotal(v float64) {
	defer func() {
		m.updated = m.clock.Now()
		m.energyMeter = new(v)
	}()

	if m.energyMeter == nil {
		return
	}

	// > not >=: an exact repeat must not clear the stale flag noteEnergyMeterActivity
	// may just have set for this very call - only an actual move past the baseline
	// means the reading recovered
	if v > *m.energyMeter {
		if !m.energyMeterStale {
			m.Energy += v - *m.energyMeter
		}
		m.energyMeterStale = false
		m.energyMeterStaleSince = time.Time{}
	}
}

// SetReturnEnergyMeterTotal adds the difference to the last total meter value
// in kWh (see SetEnergyMeterTotal for the stale-baseline handling).
func (m *Accumulator) SetReturnEnergyMeterTotal(v float64) {
	defer func() {
		m.updated = m.clock.Now()
		m.returnEnergyMeter = new(v)
	}()

	if m.returnEnergyMeter == nil {
		return
	}

	if v > *m.returnEnergyMeter {
		if !m.returnEnergyMeterStale {
			m.ReturnEnergy += v - *m.returnEnergyMeter
		}
		m.returnEnergyMeterStale = false
		m.returnEnergyMeterStaleSince = time.Time{}
	}
}

// energyMeterStaleDuration bounds how long an energy-total reading may repeat
// exactly while its direction's power indicates real activity before the
// meter is treated as stuck rather than reporting a genuine flat stretch -
// see noteEnergyMeterActivity. Seen live: a Balkonkraftwerk fed through a
// custom MQTT bridge sat frozen at the same total for over an hour while
// producing, then booked the whole gap as one implausible spike once a fresh
// value finally arrived.
const energyMeterStaleDuration = 15 * time.Minute

// meterStalePowerFloor (W) is the power below which an unchanged total is
// unremarkable - the direction is genuinely idle, not stuck.
const meterStalePowerFloor = 10.0

// noteEnergyMeterActivity tracks how long the energy-total reading has sat
// unchanged while active is true (this direction's own power is above
// meterStalePowerFloor). Once that has gone on for energyMeterStaleDuration,
// it flags the meter stale so AddEnergy falls back to integrating power for
// this direction until the reading moves again (see SetEnergyMeterTotal).
// Returns true exactly once, on the call that raises the flag, so the caller
// can log the transition.
func (m *Accumulator) noteEnergyMeterActivity(unchanged, active bool) bool {
	return noteStale(&m.energyMeterStaleSince, &m.energyMeterStale, m.clock, unchanged, active)
}

// noteReturnEnergyMeterActivity is noteEnergyMeterActivity for the return direction.
func (m *Accumulator) noteReturnEnergyMeterActivity(unchanged, active bool) bool {
	return noteStale(&m.returnEnergyMeterStaleSince, &m.returnEnergyMeterStale, m.clock, unchanged, active)
}

// noteStale is the shared implementation behind noteEnergyMeterActivity and
// noteReturnEnergyMeterActivity, parameterized over which direction's fields
// to update.
func noteStale(since *time.Time, stale *bool, clock clock.Clock, unchanged, active bool) bool {
	if !unchanged || !active {
		*since = time.Time{}
		return false
	}
	if since.IsZero() {
		*since = clock.Now()
		return false
	}
	if *stale || clock.Since(*since) < energyMeterStaleDuration {
		return false
	}
	*stale = true
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
