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

	// whether the meter total is stuck (same value for too long while power
	// shows real activity) - see noteEnergyMeterActivity and SetEnergyMeterTotal
	energyMeterStaleSince       time.Time
	returnEnergyMeterStaleSince time.Time
	energyMeterStale            bool
	returnEnergyMeterStale      bool

	// how long a direction has been missing (nil) readings in a row - see
	// MissEnergyMeterTotal
	energyMeterFailingSince       time.Time
	returnEnergyMeterFailingSince time.Time

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

// SetEnergyMeterTotal adds v's difference from the last known total to the
// running energy, in kWh. While the meter is marked stuck, nothing is added
// here - that time was already counted from power - it just updates the
// last known value and clears the stuck mark.
func (m *Accumulator) SetEnergyMeterTotal(v float64) {
	m.energyMeterFailingSince = time.Time{}
	defer func() { m.energyMeter = new(v) }()

	if m.energyMeter == nil {
		m.updated = m.clock.Now()
		return
	}

	// > not >=: unchanged isn't a recovery - this call might be the one that
	// just marked it stuck. Only an increase means it's real again.
	if v > *m.energyMeter {
		if !m.energyMeterStale {
			m.Energy += v - *m.energyMeter
		}
		m.energyMeterStale = false
		m.energyMeterStaleSince = time.Time{}
		m.updated = m.clock.Now()
	} else if m.energyMeterStale {
		// already stuck: keep the clock moving so later calls add only
		// their own slice, not the whole stuck period again
		m.updated = m.clock.Now()
	}
	// else: unchanged, not yet stuck - leave the clock alone so a later
	// catch-up covers the whole gap, not just since the last call
}

// SetReturnEnergyMeterTotal is SetEnergyMeterTotal for the return direction
// (e.g. feed-in).
func (m *Accumulator) SetReturnEnergyMeterTotal(v float64) {
	m.returnEnergyMeterFailingSince = time.Time{}
	defer func() { m.returnEnergyMeter = new(v) }()

	if m.returnEnergyMeter == nil {
		m.updated = m.clock.Now()
		return
	}

	if v > *m.returnEnergyMeter {
		if !m.returnEnergyMeterStale {
			m.ReturnEnergy += v - *m.returnEnergyMeter
		}
		m.returnEnergyMeterStale = false
		m.returnEnergyMeterStaleSince = time.Time{}
		m.updated = m.clock.Now()
	} else if m.returnEnergyMeterStale {
		m.updated = m.clock.Now()
	}
}

// energyMeterStaleDuration is how long a direction may repeat the same value,
// or miss readings, before it's given up on and AddEnergy switches to
// counting from power instead. Seen live: a balcony panel's total froze for
// over an hour while producing, booked as one big spike once it finally
// moved; a meter's energy register removed from its config left its total
// frozen forever, history stuck at flat zero (evcc-io/evcc#33091).
//
// No energy is lost either way - the eventual catch-up covers the whole gap,
// not just the last poll (see SetEnergyMeterTotal). A shorter wait only
// limits how much of that catch-up can land in the wrong 15-minute slot.
const energyMeterStaleDuration = 5 * time.Minute

// meterStalePowerFloor (W): below this power, an unchanged total isn't
// suspicious - the device just isn't producing or consuming much, not stuck.
const meterStalePowerFloor = 10.0

// noteEnergyMeterActivity checks how long the total has repeated while
// active is true (power above meterStalePowerFloor). Past
// energyMeterStaleDuration, it marks the meter stuck. Returns true only on
// the call that makes that decision, so the caller can log it.
func (m *Accumulator) noteEnergyMeterActivity(unchanged, active bool) bool {
	return noteStale(&m.energyMeterStaleSince, &m.energyMeterStale, m.clock, unchanged, active)
}

// noteReturnEnergyMeterActivity is noteEnergyMeterActivity for the return direction.
func (m *Accumulator) noteReturnEnergyMeterActivity(unchanged, active bool) bool {
	return noteStale(&m.returnEnergyMeterStaleSince, &m.returnEnergyMeterStale, m.clock, unchanged, active)
}

// noteStale holds the check shared by noteEnergyMeterActivity and
// noteReturnEnergyMeterActivity - same logic, different fields per direction.
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

// MissEnergyMeterTotal counts a missing (nil) reading for a direction that
// has reported a total before. Past energyMeterStaleDuration in a row, the
// stored total is dropped. Returns true only on that call, so the caller
// can log it.
func (m *Accumulator) MissEnergyMeterTotal() bool {
	if m.energyMeter == nil {
		return false
	}
	if m.energyMeterFailingSince.IsZero() {
		m.energyMeterFailingSince = m.clock.Now()
		return false
	}
	if m.clock.Since(m.energyMeterFailingSince) < energyMeterStaleDuration {
		return false
	}
	m.energyMeter = nil
	m.energyMeterFailingSince = time.Time{}
	return true
}

// MissReturnEnergyMeterTotal is MissEnergyMeterTotal for the return direction.
func (m *Accumulator) MissReturnEnergyMeterTotal() bool {
	if m.returnEnergyMeter == nil {
		return false
	}
	if m.returnEnergyMeterFailingSince.IsZero() {
		m.returnEnergyMeterFailingSince = m.clock.Now()
		return false
	}
	if m.clock.Since(m.returnEnergyMeterFailingSince) < energyMeterStaleDuration {
		return false
	}
	m.returnEnergyMeter = nil
	m.returnEnergyMeterFailingSince = time.Time{}
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
