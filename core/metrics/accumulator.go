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

// SetEnergyMeterTotal adds the difference between v and the last known meter
// total to the running energy total, in kWh. If the meter is currently
// marked stuck (see noteEnergyMeterActivity), this reading is not added -
// that time period was already counted from the power reading instead. The
// last known value is still updated so later readings compare against it,
// and the stuck mark is cleared.
func (m *Accumulator) SetEnergyMeterTotal(v float64) {
	defer func() {
		m.updated = m.clock.Now()
		m.energyMeter = new(v)
	}()

	if m.energyMeter == nil {
		return
	}

	// deliberately > and not >=: an unchanged value is not a recovery - it
	// might be this very call that just marked the meter stuck. Only an
	// actual increase means the meter is reporting real values again.
	if v > *m.energyMeter {
		if !m.energyMeterStale {
			m.Energy += v - *m.energyMeter
		}
		m.energyMeterStale = false
		m.energyMeterStaleSince = time.Time{}
	}
}

// SetReturnEnergyMeterTotal is SetEnergyMeterTotal for the return direction
// (e.g. feed-in).
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

// energyMeterStaleDuration is how long a meter total may stay exactly the
// same, while its own power reading shows real activity, before it's treated
// as stuck instead of just quiet. Seen live: a balcony solar panel behind a
// custom MQTT bridge reported the same total for over an hour while
// producing - when a fresh value finally arrived, the whole missed period
// was booked as one big, wrong jump.
//
// Chosen shorter than a 15-minute history slot on purpose: the wait itself
// is never added back afterwards (see SetEnergyMeterTotal), so a meter
// getting stuck right at a slot's start would zero out that whole slot if
// the wait were as long as the slot. A shorter wait keeps at least some
// correct data in every slot.
const energyMeterStaleDuration = 10 * time.Minute

// meterStalePowerFloor (W): below this power, an unchanged total isn't
// suspicious - the device just isn't producing or consuming much, not stuck.
const meterStalePowerFloor = 10.0

// noteEnergyMeterActivity checks how long the total has stayed the same
// while active is true (this direction's power is above
// meterStalePowerFloor). Once that has lasted energyMeterStaleDuration, it
// marks the meter stuck, so AddEnergy switches to counting energy from power
// instead, until the total changes again (see SetEnergyMeterTotal). Returns
// true only on the one call that makes that decision, so the caller can log it.
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
