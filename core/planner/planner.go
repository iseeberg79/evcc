package planner

import (
	"slices"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
)

// Planner plans a series of charging slots for a given (variable) tariff
type Planner struct {
	log    *util.Logger
	clock  clock.Clock // mockable time
	tariff api.Tariff
}

// New creates a price planner
func New(log *util.Logger, tariff api.Tariff) *Planner {
	return &Planner{
		log:    log,
		clock:  clock.New(),
		tariff: tariff,
	}
}

// plan creates a lowest-cost plan or required duration.
// It MUST already established that
// - rates are sorted in ascending order by cost and descending order by start time (prefer late slots)
// - target time and required duration are before end of rates
func (t *Planner) plan(rates api.Rates, requiredDuration time.Duration, targetTime time.Time) api.Rates {
	var plan api.Rates

	for _, source := range rates {
		// slot not relevant
		if !(source.End.After(t.clock.Now()) && source.Start.Before(targetTime)) {
			continue
		}

		// adjust slot start and end
		slot := source
		if slot.Start.Before(t.clock.Now()) {
			slot.Start = t.clock.Now()
		}
		if slot.End.After(targetTime) {
			slot.End = targetTime
		}

		slotDuration := slot.End.Sub(slot.Start)
		requiredDuration -= slotDuration

		// slot covers more than we need, so shorten it
		if requiredDuration < 0 {
			// the first (if not single) slot should start as late as possible
			if IsFirst(slot, plan) && len(plan) > 0 {
				slot.Start = slot.Start.Add(-requiredDuration)
			} else {
				slot.End = slot.End.Add(requiredDuration)
			}
			requiredDuration = 0

			if slot.End.Before(slot.Start) {
				panic("slot end before start")
			}
		}

		plan = append(plan, slot)

		// we found all necessary slots
		if requiredDuration == 0 {
			break
		}
	}

	return plan
}

// Plan creates a continuous emergency charging plan
func (t *Planner) continuousPlan(rates api.Rates, start, end time.Time) api.Rates {
	rates.Sort()

	res := make(api.Rates, 0, len(rates)+2)
	for _, r := range rates {
		// slot before continuous plan
		if !r.End.After(start) {
			continue
		}

		// slot after continuous plan
		if !r.Start.Before(end) {
			continue
		}

		// adjust first slot
		if r.Start.Before(start) && r.End.After(start) {
			r.Start = start
		}

		// adjust last slot
		if r.Start.Before(end) && r.End.After(end) {
			r.End = end
		}

		res = append(res, r)
	}

	if len(res) == 0 {
		res = append(res, api.Rate{
			Start: start,
			End:   end,
		})
	} else {
		// prepend missing slot
		if res[0].Start.After(start) {
			res = slices.Insert(res, 0, api.Rate{
				Start: start,
				End:   res[0].Start,
			})
		}
		// append missing slot
		if last := res[len(res)-1]; last.End.Before(end) {
			res = append(res, api.Rate{
				Start: last.End,
				End:   end,
			})
		}
	}

	return res
}

func (t *Planner) Plan(requiredDuration, precondition time.Duration, targetTime time.Time) api.Rates {
	if t == nil || requiredDuration <= 0 {
		return nil
	}

	latestStart := targetTime.Add(-requiredDuration)
	if latestStart.Before(t.clock.Now()) {
		latestStart = t.clock.Now()
		targetTime = latestStart.Add(requiredDuration)
	}

	// simplePlan only considers time, but not cost
	simplePlan := api.Rates{
		api.Rate{
			Start: latestStart,
			End:   targetTime,
		},
	}

	// target charging without tariff or late start
	if t.tariff == nil {
		return simplePlan
	}

	rates, err := t.tariff.Rates()

	// treat like normal target charging if we don't have rates
	if len(rates) == 0 || err != nil {
		return simplePlan
	}

	// consume remaining time
	if t.clock.Until(targetTime) <= requiredDuration {
		return t.continuousPlan(rates, latestStart, targetTime)
	}

	// rates are by default sorted by date, oldest to newest
	last := rates[len(rates)-1].End

	// keep track of precondition slots for mandatory enforcement later
	preCondRates := extractPreconditionSlots(rates, precondition, targetTime)

	// reduce planning horizon to available rates
	if targetTime.After(last) {
		// there is enough time for charging after end of current rates
		durationAfterRates := targetTime.Sub(last)
		if durationAfterRates >= requiredDuration {
			return nil
		}

		// need to use some of the available slots
		t.log.DEBUG.Printf("target time beyond available slots- reducing plan horizon from %v to %v",
			requiredDuration.Round(time.Second), durationAfterRates.Round(time.Second))

		targetTime = last
		requiredDuration -= durationAfterRates
	}

	// use slot bundling to minimize charging interruptions
	plan := t.planSlotBundled(rates, requiredDuration, targetTime)

	// if precondition is enabled, ensure we have charging during the precondition window
	if precondition > 0 {
		plan = t.ensurePreconditionSlot(plan, preCondRates, precondition, targetTime)
	}

	// sort plan by time
	plan.Sort()

	return plan
}

// ensurePreconditionSlot ensures that the plan includes at least one slot during the precondition window.
// This is mandatory when precondition is enabled, to allow the vehicle to use grid power for climate control.
func (t *Planner) ensurePreconditionSlot(plan api.Rates, preCondRates api.Rates, precondition time.Duration, targetTime time.Time) api.Rates {
	if len(preCondRates) == 0 {
		return plan // No precondition slots available
	}

	preCondStart := targetTime.Add(-precondition)

	// Check if plan already includes slots in the precondition window
	for _, slot := range plan {
		// If any slot overlaps with the precondition window, we're good
		if slot.Start.Before(targetTime) && slot.End.After(preCondStart) {
			return plan
		}
	}

	// No precondition slot found, we need to add one
	// Filter slots that are valid (after current time)
	var validSlots api.Rates
	for _, slot := range preCondRates {
		if slot.End.After(t.clock.Now()) {
			// Adjust slot if it starts before now
			if slot.Start.Before(t.clock.Now()) {
				slot.Start = t.clock.Now()
			}
			validSlots = append(validSlots, slot)
		}
	}

	if len(validSlots) == 0 {
		return plan // No valid slots available
	}

	// Sort by cost and pick the cheapest
	slices.SortFunc(validSlots, sortByCost)

	// Add the cheapest precondition slot to the plan
	plan = append(plan, validSlots[0])

	t.log.DEBUG.Printf("added mandatory precondition slot: %v - %v (cost: %.3f)",
		validSlots[0].Start.Format("15:04"), validSlots[0].End.Format("15:04"), validSlots[0].Value)

	return plan
}

// extractPreconditionSlots extracts slots that fall within the precondition window.
// These slots will be used for mandatory precondition enforcement.
func extractPreconditionSlots(rates api.Rates, precondition time.Duration, targetTime time.Time) api.Rates {
	if precondition == 0 {
		return nil
	}

	preCondStart := targetTime.Add(-precondition)
	var preCondRates api.Rates

	for _, r := range rates {
		// Skip slots that don't overlap with precondition window
		if !r.End.After(preCondStart) || !r.Start.Before(targetTime) {
			continue
		}

		// Adjust slot boundaries to fit within precondition window
		slot := r
		if slot.Start.Before(preCondStart) {
			slot.Start = preCondStart
		}
		if slot.End.After(targetTime) {
			slot.End = targetTime
		}

		preCondRates = append(preCondRates, slot)
	}

	return preCondRates
}
