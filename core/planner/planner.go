package planner

import (
	"math"
	"slices"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
)

const (
	// making unvisible constraints from loadpoint_plan.go available
	smallSlotDuration = 15 * time.Minute // minimum slot duration to keep
	smallGapDuration  = 30 * time.Minute // maximum gap duration to merge
)

// Planner plans a series of charging slots for a given (variable) tariff
type Planner struct {
	log    *util.Logger
	clock  clock.Clock // mockable time
	tariff api.Tariff
}

// New creates a price planner
func New(log *util.Logger, tariff api.Tariff, opt ...func(t *Planner)) *Planner {
	p := &Planner{
		log:    log,
		clock:  clock.New(),
		tariff: tariff,
	}

	for _, o := range opt {
		o(p)
	}

	return p
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

	// if no slots remain, create a full slot
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

// findOptimalContinuousWindow finds the cheapest continuous time window of the given duration
// within the available rates, ending no later than targetTime.
// 
// The algorithm:
// 1. Generates all possible window start points from rate boundaries
// 2. For each valid window, calculates the total cost based on overlapping rate slots
// 3. Returns the window with minimal cost as a single merged slot with weighted average price
//
// Returns: best plan as a single rate slot with weighted average price, and the total cost
func (t *Planner) findOptimalContinuousWindow(rates api.Rates, effectiveDuration time.Duration, targetTime time.Time) (api.Rates, float64) {
	now := t.clock.Now()
	
	// Validate inputs
	if len(rates) == 0 || effectiveDuration <= 0 {
		return nil, 0
	}

	rates.Sort() // sort slots by start time

	// Collect all relevant time points (rate boundaries + now + target)
	points := make([]time.Time, 0, 2*len(rates)+2)
	for _, r := range rates {
		points = append(points, r.Start, r.End)
	}
	points = append(points, now, targetTime)
	
	// Sort and remove duplicates
	slices.SortFunc(points, func(a, b time.Time) int { 
		return a.Compare(b) 
	})
	points = slices.Compact(points)

	var bestPlan api.Rates
	minCost := math.Inf(1)

	// Try each possible window start position
	for _, windowStart := range points {
		windowEnd := windowStart.Add(effectiveDuration)

		// Allow windowEnd == targetTime
		if windowEnd.After(targetTime) {
			break
		}

		// Calculate cost for this window by examining all rates
		totalCost := 0.0
		windowPlan := make(api.Rates, 0)

		for _, rate := range rates {
			// Skip rates that don't overlap with this window
			if rate.End.Before(windowStart) || rate.Start.After(windowEnd) {
				continue
			}

			// Calculate the overlapping portion
			overlapStart := rate.Start
			if windowStart.After(overlapStart) {
				overlapStart = windowStart
			}
			overlapEnd := rate.End
			if windowEnd.Before(overlapEnd) {
				overlapEnd = windowEnd
			}

			if overlapStart.Before(overlapEnd) {
				duration := overlapEnd.Sub(overlapStart).Hours()
				totalCost += rate.Value * duration

				windowPlan = append(windowPlan, api.Rate{
					Start: overlapStart,
					End:   overlapEnd,
					Value: rate.Value,
				})
			}
		}

		// Check if this window has the minimal cost
		if len(windowPlan) > 0 && totalCost < minCost {
			minCost = totalCost
			bestPlan = make(api.Rates, len(windowPlan))
			copy(bestPlan, windowPlan)
			bestPlan.Sort()
		}
	}

	// Merge individual slots into a single continuous slot with weighted average price
	if len(bestPlan) > 0 {
		// Calculate weighted average price per kWh using the already computed minCost
		avgPrice := minCost / effectiveDuration.Hours()

		mergedSlot := api.Rate{
			Start: bestPlan[0].Start,
			End:   bestPlan[len(bestPlan)-1].End,
			Value: avgPrice,
		}

		bestPlan = api.Rates{mergedSlot}
	}

	return bestPlan, minCost
}

// Plan creates a charging plan based on the configured or passed-in mode
// supports a continuous boolean flag to use single cheapest window mode
func (t *Planner) Plan(requiredDuration, precondition time.Duration, targetTime time.Time, continuous ...bool) api.Rates {
	if t == nil || requiredDuration <= 0 {
		return nil
	}

	useContinuous := len(continuous) > 0 && continuous[0]

	latestStart := targetTime.Add(-requiredDuration)
	if latestStart.Before(t.clock.Now()) {
		latestStart = t.clock.Now()
		targetTime = latestStart.Add(requiredDuration)
	}

	// simplePlan only considers time, but not cost
	simplePlan := api.Rates{
		{
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

	// reduce planning horizon to available rates
	if targetTime.After(last) {
		durationAfterRates := targetTime.Sub(last)
		if durationAfterRates >= requiredDuration {
			return nil
		}

		// need to use some of the available slots

		targetTime = last
		requiredDuration -= durationAfterRates

		// recalculate latestStart after adjusting targetTime and requiredDuration
		latestStart = targetTime.Add(-requiredDuration)
		if latestStart.Before(t.clock.Now()) {
			latestStart = t.clock.Now()
		}
	}

	// Calculate effective duration (excluding preconditioning)
	effectiveDuration := requiredDuration
	if precondition > 0 {
		effectiveDuration -= precondition
	}

	// Plan for effective duration (without preconditioning window)
	preCondWindow := targetTime
	if precondition > 0 {
		preCondWindow = targetTime.Add(-precondition)
	}

	// use continuous window mode if selected
	if useContinuous {
		plan, _ := t.findOptimalContinuousWindow(rates, effectiveDuration, preCondWindow)

		if plan == nil {
			return t.continuousPlan(rates, latestStart, targetTime)
		}

		// add preconditioning at the end
		plan = t.addPreconditioningWindow(plan, rates, precondition, targetTime)

		// sort plan by time
		plan.Sort()

		return plan
	}

	// default mode: cheapest combination of slots

	slices.SortStableFunc(rates, sortByCost)
	plan := t.plan(rates, effectiveDuration, preCondWindow)

	// If we have a single slot with preconditioning, shift it to the end of its rate slot
	// to minimize the gap before preconditioning
	if len(plan) == 1 && precondition > 0 {
		slot := plan[0]
		slotDuration := slot.End.Sub(slot.Start)

		// Find the original rate slot that contains this plan slot
		for _, rate := range rates {
			if !rate.Start.After(slot.Start) && rate.End.After(slot.Start) {
				// Shift slot to end at the rate's end (or preCondWindow, whichever is earlier)
				newEnd := rate.End
				if newEnd.After(preCondWindow) {
					newEnd = preCondWindow
				}
				slot.End = newEnd
				slot.Start = slot.End.Add(-slotDuration)
				plan[0] = slot
				break
			}
		}
	}

	// sort plan by time
	plan.Sort()

	// Apply gap constraints
	plan = t.applyGapConstraints(plan, smallSlotDuration, smallGapDuration)

	// Trim excess duration from window edges
	plan = t.trimExcessDuration(plan, effectiveDuration, smallSlotDuration)

	// Recalculate prices after gap merging and trimming
	// This ensures prices reflect the actual tariff composition after slot modifications
	plan = t.recalculatePrices(plan, rates)

	// Add preconditioning at the end
	plan = t.addPreconditioningWindow(plan, rates, precondition, targetTime)

	// sort plan by time
	plan.Sort()

	return plan
}

// addPreconditioningWindow appends a preconditioning window to the plan
func (t *Planner) addPreconditioningWindow(plan api.Rates, rates api.Rates, precondition time.Duration, targetTime time.Time) api.Rates {
	if precondition <= 0 {
		return plan
	}

	preCondStart := targetTime.Add(-precondition)
	preCondPlan := t.continuousPlan(rates, preCondStart, targetTime)
	return append(plan, preCondPlan...)
}

// applyGapConstraints applies gap and slot duration constraints to a plan
// smallSlotDuration: minimum slot duration to keep (e.g., 15 minutes)
// smallGapDuration: maximum gap to merge (e.g., 30 minutes)
// Returns the adjusted plan with updated costs
func (t *Planner) applyGapConstraints(plan api.Rates, smallSlotDuration, smallGapDuration time.Duration) api.Rates {
	if len(plan) == 0 {
		return plan
	}

	// Step 1: Remove slots that are too short
	filtered := make(api.Rates, 0, len(plan))
	for _, slot := range plan {
		duration := slot.End.Sub(slot.Start)
		if duration >= smallSlotDuration {
			filtered = append(filtered, slot)
		} else {
			//	slot.Start.Round(time.Second), duration.Round(time.Second), smallSlotDuration.Round(time.Second))
		}
	}

	if len(filtered) == 0 {
		return nil
	}

	// Step 2: Merge slots with small gaps between them
	merged := make(api.Rates, 0, len(filtered))
	current := filtered[0]

	for i := 1; i < len(filtered); i++ {
		gap := filtered[i].Start.Sub(current.End)

		if gap <= smallGapDuration {
			// Merge slots by filling the gap

			// Calculate weighted average cost for the merged slot
			duration1 := current.End.Sub(current.Start).Hours()
			duration2 := filtered[i].End.Sub(filtered[i].Start).Hours()
			gapDuration := gap.Hours()

			// For the gap, use the higher cost of the two adjacent slots (conservative approach)
			gapCost := math.Max(current.Value, filtered[i].Value)

			totalCost := current.Value*duration1 + gapCost*gapDuration + filtered[i].Value*duration2
			totalDuration := duration1 + gapDuration + duration2

			current = api.Rate{
				Start: current.Start,
				End:   filtered[i].End,
				Value: totalCost / totalDuration,
			}
		} else {
			// Gap is too large, finalize current slot and start a new one
			merged = append(merged, current)
			current = filtered[i]
		}
	}

	// Add the last slot
	merged = append(merged, current)

	return merged
}

// trimExcessDuration removes excess duration from window edges, preferring to trim from the highest-cost edge
// A window is a group of consecutive slots (no gaps between them)
// requiredDuration: the actual charging duration needed
// smallSlotDuration: minimum slot duration (e.g., 15 minutes) - slots below this will be removed entirely
func (t *Planner) trimExcessDuration(plan api.Rates, requiredDuration time.Duration, smallSlotDuration time.Duration) api.Rates {
	if len(plan) == 0 || requiredDuration <= 0 {
		return plan
	}

	// Calculate total duration in the plan
	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}

	// If plan duration matches or is less than required, no trimming needed
	excessDuration := totalDuration - requiredDuration
	if excessDuration <= 0 {
		return plan
	}

	// Clone the plan to avoid modifying the original
	result := make(api.Rates, len(plan))
	copy(result, plan)

	// Recursively trim until we've removed all excess
	for excessDuration > 0 && len(result) > 0 {
		// Find all window edges (slots at the start or end of each charging window)
		type edge struct {
			slotIdx  int
			isStart  bool // true if this is the start of a window
			cost     float64
			duration time.Duration
		}

		var edges []edge

		for i := 0; i < len(result); i++ {
			slot := result[i]
			slotDuration := slot.End.Sub(slot.Start)

			// Check if this is the start of a window (no previous slot or gap before it)
			isWindowStart := i == 0
			if i > 0 {
				prevSlot := result[i-1]
				if !prevSlot.End.Equal(slot.Start) {
					isWindowStart = true
				}
			}

			// Check if this is the end of a window (no next slot or gap after it)
			isWindowEnd := i == len(result)-1
			if i < len(result)-1 {
				nextSlot := result[i+1]
				if !slot.End.Equal(nextSlot.Start) {
					isWindowEnd = true
				}
			}

			if isWindowStart {
				edges = append(edges, edge{
					slotIdx:  i,
					isStart:  true,
					cost:     slot.Value,
					duration: slotDuration,
				})
			}
			if isWindowEnd {
				edges = append(edges, edge{
					slotIdx:  i,
					isStart:  false,
					cost:     slot.Value,
					duration: slotDuration,
				})
			}
		}

		if len(edges) == 0 {
			break
		}

		// Find the edge with the highest cost
		maxCostIdx := 0
		for i := 1; i < len(edges); i++ {
			if edges[i].cost > edges[maxCostIdx].cost {
				maxCostIdx = i
			}
		}

		selectedEdge := edges[maxCostIdx]
		slot := result[selectedEdge.slotIdx]
		currentSlotDuration := slot.End.Sub(slot.Start)

		//	selectedEdge.slotIdx, selectedEdge.isStart, selectedEdge.cost, currentSlotDuration.Round(time.Second))

		// Calculate what would remain after trimming
		remainingSlotDuration := currentSlotDuration - excessDuration

		// If trimming would leave a slot smaller than the minimum constraint, remove it entirely
		if remainingSlotDuration > 0 && remainingSlotDuration < smallSlotDuration {
			result = append(result[:selectedEdge.slotIdx], result[selectedEdge.slotIdx+1:]...)
			excessDuration -= currentSlotDuration
		} else if currentSlotDuration > excessDuration {
			// Shorten the slot (remaining duration will be >= smallSlotDuration)
			if selectedEdge.isStart {
				// At window START: trim from the beginning (start later)
				result[selectedEdge.slotIdx].Start = slot.Start.Add(excessDuration)
			} else {
				// At window END: trim from the end (end earlier)
				result[selectedEdge.slotIdx].End = slot.End.Add(-excessDuration)
			}
			excessDuration = 0
		} else {
			// Remove the entire slot
			result = append(result[:selectedEdge.slotIdx], result[selectedEdge.slotIdx+1:]...)
			excessDuration -= currentSlotDuration
		}
	}

	return result
}

// recalculatePrices recalculates slot prices based on original tariff rates
// This is necessary after operations that modify slot boundaries (like merging or trimming)
// because those operations use weighted averages that may no longer reflect the actual tariff composition
func (t *Planner) recalculatePrices(plan api.Rates, rates api.Rates) api.Rates {
	if len(plan) == 0 || len(rates) == 0 {
		return plan
	}

	for i := range plan {
		slot := &plan[i]
		totalCost := 0.0
		totalDuration := 0.0

		// Find overlapping rates and calculate weighted average
		for _, rate := range rates {
			// Skip rates that don't overlap with this slot
			if rate.End.Before(slot.Start) || rate.End.Equal(slot.Start) {
				continue
			}
			if rate.Start.After(slot.End) || rate.Start.Equal(slot.End) {
				continue
			}

			// Calculate overlap boundaries
			overlapStart := rate.Start
			if slot.Start.After(overlapStart) {
				overlapStart = slot.Start
			}
			overlapEnd := rate.End
			if slot.End.Before(overlapEnd) {
				overlapEnd = slot.End
			}

			// Add to weighted calculation
			if overlapStart.Before(overlapEnd) {
				duration := overlapEnd.Sub(overlapStart).Hours()
				totalCost += rate.Value * duration
				totalDuration += duration
			}
		}

		// Update slot price with recalculated weighted average
		if totalDuration > 0 {
			slot.Value = totalCost / totalDuration
		}
	}

	return plan
}
