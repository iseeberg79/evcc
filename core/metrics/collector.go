package metrics

import (
	"time"

	"github.com/evcc-io/evcc/db"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
)

var log = util.NewLogger("metrics")

const (
	// groups
	Forecast    = "forecast"
	Temperature = "temperature"
	Battery     = "battery"
	Grid        = "grid"
	PV          = "pv"
	Home        = "home" // meter and group (virtual measurement)
	Loadpoint   = "loadpoint"
	Meter       = "meter"    // additional meter (ext, monitoring only)
	Consumer    = "consumer" // consumer meter (consumers list or aux)
)

type Collector struct {
	entity     entity
	accu       *Accumulator
	started    time.Time
	restored   bool          // meter readings seeded from db
	lastSlot   time.Time     // last persisted slot at restore, for contiguity check
	maxGap     time.Duration // largest single read gap seen in the current slot
	statsCache EnergyStats
}

func NewCollector(group, name, title string, opt ...func(*Accumulator)) (*Collector, error) {
	entity, err := createEntity(group, name, title)
	if err != nil {
		return nil, err
	}

	c := &Collector{
		entity: entity,
		accu:   NewAccumulator(opt...),
	}

	// seed saved readings so the first delta covers the downtime
	if state := (AccumulatorState{entity.EnergyMeter, entity.ReturnEnergyMeter}); state.CompleteFor(group) {
		c.accu.Restore(state)
		c.restored = true
		// last persisted slot distinguishes a contiguous restart (energy stays
		// time-correct) from one that skipped whole slots (inflated catchup)
		var lastTs int64
		db.Instance.Model(new(meter)).Where("meter = ?", entity.Id).Select("COALESCE(max(ts), 0)").Scan(&lastTs)
		c.lastSlot = time.Unix(lastTs, 0)
	}

	return c, nil
}

// createEntity ensures the entity row exists and refreshes its title.
func createEntity(group, name, title string) (entity, error) {
	// keep history when a meter is regrouped "meter" -> "consumer" (aux, ext convert)
	if group == Consumer {
		var prev entity
		if db.Instance.Where(`"group" = ? AND name = ?`, Meter, name).Limit(1).Find(&prev).RowsAffected > 0 {
			db.Instance.Model(&prev).UpdateColumn("group", Consumer)
		}
	}

	e := entity{Group: group, Name: name}

	if err := db.Instance.Where(&e).Attrs(entity{Title: title}).FirstOrCreate(&e).Error; err != nil {
		return e, err
	}

	return e, e.updateTitle(title)
}

// updateTitle refreshes the entity's stored title if it changed
func (e *entity) updateTitle(title string) error {
	if title == "" || e.Title == title {
		return nil
	}

	e.Title = title
	return db.Instance.Model(e).UpdateColumn("title", title).Error
}

// updateIsTemp refreshes the entity's stored is_temp flag if it changed
func (e *entity) updateIsTemp(isTemp bool) error {
	if e.IsTemp == isTemp {
		return nil
	}

	e.IsTemp = isTemp
	return db.Instance.Model(e).UpdateColumn("is_temp", isTemp).Error
}

// UpdateTitle refreshes the collector entity's stored title if it changed.
func (c *Collector) UpdateTitle(title string) error {
	return c.entity.updateTitle(title)
}

func (c *Collector) process(fun func()) error {
	now := c.accu.clock.Now()

	// track the largest single read gap in the current slot - a read that
	// arrives long after the previous one attributes its whole delta to
	// whichever slot is current at persist time (see persist), which is the
	// mechanism behind the "energy jumped to the wrong slot" symptom
	if prev := c.accu.updated; !prev.IsZero() {
		if gap := now.Sub(prev); gap > c.maxGap {
			c.maxGap = gap
		}
	}

	fun()

	return c.advanceSlot(now)
}

// advanceSlot persists and resets the accumulator when now has entered a new
// slot, leaving it untouched within the current one.
func (c *Collector) advanceSlot(now time.Time) error {
	slotStart := now.Truncate(tariff.SlotDuration)

	switch {
	case c.started.IsZero():
		if c.restored {
			// seeded readings make the mid-slot start complete energy-wise
			c.started = slotStart
			return nil
		}
		// keep started un-truncated so a mid-slot start stays distinguishable
		c.started = now

	case slotStart.After(c.started):
		// persist the completed slot only if started is the immediately
		// preceding slot boundary - false for the mid-slot first slot and
		// for a slot reached after a data gap
		if c.started.Equal(slotStart.Add(-tariff.SlotDuration)) {
			// a restore that skipped whole slots dumps the downtime energy into
			// this single slot, inflating it - a contiguous restart keeps the
			// slot's meter delta time-correct, so only the former is recovered
			recovered := c.restored && !c.started.Equal(c.lastSlot.Add(tariff.SlotDuration))
			if err := c.persist(recovered); err != nil {
				return err
			}
		} else if c.started.Equal(c.started.Truncate(tariff.SlotDuration)) {
			// a genuine data gap (not just the unaligned mid-slot first start,
			// which is never slot-aligned): the buffered energy can't be
			// attributed to any single slot and is discarded below instead of
			// persisted
			log.DEBUG.Printf("%s %s data gap: %v missed since %v, discarding %.3f kWh",
				c.entity.Group, c.entity.Title, slotStart.Sub(c.started)-tariff.SlotDuration, c.started, c.accu.Energy)
		}

		c.restored = false // only the first slot inherits recovery energy
		c.started = slotStart

	default:
		return nil
	}

	c.accu.Energy = 0
	c.accu.ReturnEnergy = 0
	c.accu.SocTemp = nil
	c.maxGap = 0
	return nil
}

// persistGapWarnRatio: a single read gap covering more than this fraction of
// the slot duration is a strong signal that the delta it produced spans the
// slot boundary and got attributed entirely to one side - the mechanism
// behind the "energy jumped to the wrong slot" symptom.
const persistGapWarnRatio = 0.5

func (c *Collector) persist(recovered bool) error {
	if c.maxGap > time.Duration(float64(tariff.SlotDuration)*persistGapWarnRatio) {
		log.DEBUG.Printf("%s %s slot %v built from a %v read gap, energy may be misattributed across the slot boundary",
			c.entity.Group, c.entity.Title, c.started, c.maxGap)
	}

	if err := persist(c.entity, c.started, c.accu.Energy, c.accu.ReturnEnergy, c.accu.SocTemp, recovered); err != nil {
		return err
	}

	// checkpoint meter readings for downtime recovery (scoped write, keeps identity intact)
	s := c.accu.Snapshot()
	c.entity.EnergyMeter = s.EnergyMeter
	c.entity.ReturnEnergyMeter = s.ReturnEnergyMeter
	return db.Instance.Model(&c.entity).UpdateColumns(map[string]any{
		"energy_meter":        s.EnergyMeter,
		"return_energy_meter": s.ReturnEnergyMeter,
	}).Error
}

// SetSocTemp records the slot-start soc (temperature when isTemp).
// Advances the slot via process() so it can be used without a prior AddEnergy call.
func (c *Collector) SetSocTemp(value float64, isTemp bool) error {
	if err := c.process(func() { c.accu.setSocTemp(value) }); err != nil {
		return err
	}
	return c.entity.updateIsTemp(isTemp)
}

func (c *Collector) EnergyProfile(from time.Time) (*[96]float64, error) {
	return energyProfile(c.entity, from)
}

// LastSlotEnergy returns the energy in kWh of the most recently completed
// 15min slot, or false when it has not been persisted (boot, data gap) or
// contains recovered downtime energy.
func (c *Collector) LastSlotEnergy() (float64, bool) {
	ts := c.accu.clock.Now().Truncate(tariff.SlotDuration).Add(-tariff.SlotDuration)

	var m meter
	if db.Instance.Where("meter = ? AND ts = ? AND COALESCE(recovered, 0) = 0", c.entity.Id, ts.Unix()).Limit(1).Find(&m).RowsAffected == 0 {
		return 0, false
	}
	return m.Energy, true
}

// SetEnergy overwrites the current slot's energy in kWh for sources that yield
// the slot total rather than a delta. Repeated calls within a slot are
// idempotent, so the caller needs no tick bookkeeping of its own.
func (c *Collector) SetEnergy(energy float64) error {
	// advance first- the completed slot keeps the value it was last set to
	if err := c.advanceSlot(c.accu.clock.Now()); err != nil {
		return err
	}

	c.accu.Energy = energy
	return nil
}

// SetCapabilities drops the persisted reading for a direction the device no longer
// reports, so its energy falls back to power integration instead of freezing.
func (c *Collector) SetCapabilities(energy, returnEnergy bool) error {
	cols := make(map[string]any, 2)

	// keyed on the entity, since an incomplete state is left unrestored and would
	// otherwise resurface once the other direction is checkpointed again
	if !energy && c.entity.EnergyMeter != nil {
		c.accu.energyMeter = nil
		c.entity.EnergyMeter = nil
		cols["energy_meter"] = nil
	}
	if !returnEnergy && c.entity.ReturnEnergyMeter != nil {
		c.accu.returnEnergyMeter = nil
		c.entity.ReturnEnergyMeter = nil
		cols["return_energy_meter"] = nil
	}

	if len(cols) == 0 {
		return nil
	}

	// a surviving reading still covers the downtime for its own direction, so
	// keep the restore rather than discarding that delta with the cleared one
	c.restored = c.accu.energyMeter != nil || c.accu.returnEnergyMeter != nil

	return db.Instance.Model(&c.entity).UpdateColumns(cols).Error
}

func (c *Collector) SetEnergyMeterTotal(v float64) error {
	return c.process(func() {
		c.accu.SetEnergyMeterTotal(v)
	})
}

func (c *Collector) SetReturnEnergyMeterTotal(v float64) error {
	return c.process(func() {
		c.accu.SetReturnEnergyMeterTotal(v)
	})
}

// AddEnergy adds energy for this update. It trusts a meter's reported total
// and adds the difference from the last reading. If a direction has no
// total, or its total is stuck (see noteEnergyMeterActivity), it counts
// energy from power instead. A single missing or unchanged reading alone
// doesn't switch it - only being marked stuck does.
func (c *Collector) AddEnergy(energyTotal, returnEnergyTotal *float64, power float64) error {
	return c.process(func() {
		// check stuck first, so the call that gives up on a direction
		// already counts its own slice from power too
		if energyTotal != nil {
			prev := c.accu.energyMeter
			if c.accu.noteEnergyMeterActivity(prev != nil && *energyTotal == *prev, power >= meterStalePowerFloor) {
				log.WARN.Printf("%s %s energy total stuck at %.3f kWh for over %v while power indicates activity, falling back to power integration",
					c.entity.Group, c.entity.Title, *prev, energyMeterStaleDuration)
			}
		}
		if returnEnergyTotal != nil {
			prev := c.accu.returnEnergyMeter
			if c.accu.noteReturnEnergyMeterActivity(prev != nil && *returnEnergyTotal == *prev, -power >= meterStalePowerFloor) {
				log.WARN.Printf("%s %s return energy total stuck at %.3f kWh for over %v while power indicates activity, falling back to power integration",
					c.entity.Group, c.entity.Title, *prev, energyMeterStaleDuration)
			}
		}

		// a direction that ever reported a total is metered, so a nil read is a
		// transient failure rather than a power-only meter - only being marked
		// stuck (above) switches it to power
		hasEnergyMeter := (energyTotal != nil || c.accu.energyMeter != nil) && !c.accu.energyMeterStale
		hasReturnMeter := (returnEnergyTotal != nil || c.accu.returnEnergyMeter != nil) && !c.accu.returnEnergyMeterStale

		// count energy from power for the direction that has no total or is
		// stuck - do this before touching the meter total below, since that
		// also moves the accumulator's own clock forward
		if power >= 0 {
			if !hasEnergyMeter {
				c.accu.AddPower(power)
			}
		} else if !hasReturnMeter {
			c.accu.AddPower(power)
		}

		if energyTotal != nil {
			c.accu.SetEnergyMeterTotal(*energyTotal)
		}
		if returnEnergyTotal != nil {
			c.accu.SetReturnEnergyMeterTotal(*returnEnergyTotal)
		}
	})
}
