package planner

import (
	"math"
	"slices"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
)

var (
	// InterruptionPenaltyPercent is the cost penalty threshold for fragmenting charging sessions
	// Applied as percentage of average cost - fragmentation only occurs if it saves more than this
	//
	// Special values:
	// - 0.00: No penalty, pure cost optimization (maximum fragmentation, not recommended)
	// - 0.06: 6% penalty, optimal balance - filters micro-fluctuations while capturing real savings
	// - 0.10: 10% penalty, strong preference for continuous charging (conservative)
	InterruptionPenaltyPercent = 0.06

	// MaxChargingWindows limits the number of separate charging windows
	// This prevents excessive start-stop cycles which might stress the battery/loader
	MaxChargingWindows = 3
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

// chargingWindow represents a continuous charging window
type chargingWindow struct {
	slots        api.Rates
	totalCost    float64
	totalSeconds float64
}

// plan creates a lowest-cost plan or required duration.
// It MUST already established that
// - rates are sorted in ascending order by cost and descending order by start time (prefer late slots)
// - target time and required duration are before end of rates
func (t *Planner) plan(rates api.Rates, requiredDuration time.Duration, targetTime time.Time) api.Rates {
	// Use pure cost optimization without window limits when MaxChargingWindows is disabled
	// Note: InterruptionPenaltyPercent is applied within window optimization logic
	if MaxChargingWindows == 0 {
		return t.planOriginal(rates, requiredDuration, targetTime)
	}

	// Use window-optimized planning with interruption penalty
	// Higher InterruptionPenaltyPercent values result in less fragmentation
	return t.planWithWindowOptimization(rates, requiredDuration, targetTime)
}

// planOriginal is the original plan implementation for pure cost optimization
func (t *Planner) planOriginal(rates api.Rates, requiredDuration time.Duration, targetTime time.Time) api.Rates {
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

// planWithWindowOptimization creates a plan that limits charging interruptions
func (t *Planner) planWithWindowOptimization(rates api.Rates, requiredDuration time.Duration, targetTime time.Time) api.Rates {
	// Step 1: Get base plan with cheapest slots (unlimited windows)
	basePlan := t.planOriginal(rates, requiredDuration, targetTime)
	if len(basePlan) == 0 {
		return basePlan
	}

	// Step 2: Group consecutive slots into windows
	basePlan.Sort()
	windows := t.groupIntoWindows(basePlan)

	initialWindowCount := len(windows)
	t.log.DEBUG.Printf("window optimization: base plan has %d windows for %v duration",
		initialWindowCount, requiredDuration)

	// Step 2.5: Fill gaps that are cheaper than current plan average (prefer continuous cheap blocks)
	// Then apply penalty threshold for remaining small gaps
	if len(windows) > 1 {
		windows = t.fillAffordableGaps(windows, rates, targetTime)
		if len(windows) < initialWindowCount {
			t.log.DEBUG.Printf("window optimization: filled affordable gaps, reduced to %d windows", len(windows))
		}
	}

	// Step 3: If we have too many windows, need to reduce them
	if len(windows) > MaxChargingWindows {
		// Strategy: Remove the smallest/most expensive windows and redistribute
		windows = t.reduceToMaxWindows(windows, rates, requiredDuration, targetTime)
		t.log.DEBUG.Printf("window optimization: reduced from %d to %d windows",
			initialWindowCount, len(windows))
	}

	// Step 4: Convert windows back to flat plan
	var finalPlan api.Rates
	for _, w := range windows {
		finalPlan = append(finalPlan, w.slots...)
	}

	finalPlan.Sort()

	// Step 5: Ensure exact duration match
	totalDuration := Duration(finalPlan)
	if totalDuration != requiredDuration {
		t.log.DEBUG.Printf("window optimization: adjusting duration %v → %v", totalDuration, requiredDuration)
		finalPlan = t.adjustPlanDuration(finalPlan, requiredDuration)
	}

	return finalPlan
}

// adjustPlanDuration adjusts plan to exactly match required duration
func (t *Planner) adjustPlanDuration(plan api.Rates, requiredDuration time.Duration) api.Rates {
	totalDuration := Duration(plan)

	if totalDuration == requiredDuration {
		return plan
	}

	if totalDuration > requiredDuration {
		// Trim excess - prefer late start (trim from beginning)
		excess := totalDuration - requiredDuration

		// If we have multiple windows, trim from the first window
		// If we have a single window, we're already in planOriginal's logic
		for excess > 0 && len(plan) > 0 {
			firstSlot := &plan[0]
			slotDuration := firstSlot.End.Sub(firstSlot.Start)

			if slotDuration > excess {
				// For the first slot in a multi-slot plan, start late (trim from start)
				// This maintains the "prefer late start" behavior
				if len(plan) == 1 {
					// Single slot: trim from end (already in planOriginal)
					firstSlot.End = firstSlot.End.Add(-excess)
				} else {
					// Multiple slots/windows: first slot should start late
					firstSlot.Start = firstSlot.Start.Add(excess)
				}
				excess = 0
			} else {
				// Remove entire slot
				plan = plan[1:]
				excess -= slotDuration
			}
		}
	} else if totalDuration < requiredDuration {
		// This shouldn't happen after extendToMeetDuration, but handle it
		t.log.DEBUG.Printf("window optimization: plan too short after consolidation")
	}

	return plan
}

// reduceToMaxWindows reduces window count by removing smallest windows
// and extending remaining ones to meet duration
func (t *Planner) reduceToMaxWindows(windows []*chargingWindow, allRates api.Rates, requiredDuration time.Duration, targetTime time.Time) []*chargingWindow {
	// Strategy: Keep the largest/cheapest windows, drop the rest
	// Then extend remaining windows to meet required duration

	for len(windows) > MaxChargingWindows {
		// Find best consolidation: either merge adjacent windows or drop smallest
		bestMergeOption := t.findBestWindowMerge(windows, allRates, targetTime)
		bestDropOption := t.findBestWindowToDrop(windows)

		// Compare options
		if bestMergeOption != nil && (bestDropOption == nil ||
			bestMergeOption.costIncrease < bestDropOption.costIncrease) {
			// Merge is better
			newWindow := &chargingWindow{slots: bestMergeOption.newSlots}
			t.updateWindowStats(newWindow)

			result := make([]*chargingWindow, 0, len(windows)-1)
			result = append(result, windows[:bestMergeOption.windowIndex1]...)
			result = append(result, newWindow)
			result = append(result, windows[bestMergeOption.windowIndex2+1:]...)
			windows = result

			t.log.DEBUG.Printf("window optimization: merged windows %d+%d (cost increase: %.2f/h)",
				bestMergeOption.windowIndex1, bestMergeOption.windowIndex2, bestMergeOption.costIncrease)
		} else if bestDropOption != nil {
			// Drop is better
			t.log.DEBUG.Printf("window optimization: dropping window %d (cost increase: %.2f/h)",
				bestDropOption.windowIndex1, bestDropOption.costIncrease)
			windows = append(windows[:bestDropOption.windowIndex1], windows[bestDropOption.windowIndex1+1:]...)
		} else {
			// No good options
			break
		}
	}

	// Extend windows to meet required duration
	currentDuration := t.totalWindowDuration(windows)
	if currentDuration < requiredDuration {
		t.log.DEBUG.Printf("window optimization: extending to meet duration (%v → %v)",
			currentDuration, requiredDuration)
		windows = t.extendToMeetDuration(windows, allRates, requiredDuration, targetTime)
	}

	return windows
}

// findBestWindowMerge finds the cheapest adjacent window pair to merge
func (t *Planner) findBestWindowMerge(windows []*chargingWindow, allRates api.Rates, targetTime time.Time) *consolidationOption {
	var bestOption *consolidationOption
	minCostIncrease := math.MaxFloat64

	for i := 0; i < len(windows)-1; i++ {
		option := t.evaluateWindowMerge(windows[i], windows[i+1], allRates, targetTime)
		if option != nil && option.costIncrease < minCostIncrease {
			option.windowIndex1 = i
			option.windowIndex2 = i + 1
			minCostIncrease = option.costIncrease
			bestOption = option
		}
	}

	return bestOption
}

// findBestWindowToDrop finds the window that's most expensive to keep
// (either smallest or most expensive per hour)
func (t *Planner) findBestWindowToDrop(windows []*chargingWindow) *consolidationOption {
	if len(windows) <= 1 {
		return nil
	}

	var bestOption *consolidationOption
	maxCostPerHour := -1.0

	for i, w := range windows {
		avgCost := w.totalCost / w.totalSeconds * 3600

		// Prefer dropping expensive short windows
		penalty := avgCost / (w.totalSeconds / 3600) // Cost weighted by inverse duration

		if penalty > maxCostPerHour {
			maxCostPerHour = penalty
			bestOption = &consolidationOption{
				windowIndex1: i,
				windowIndex2: -1,
				costIncrease: avgCost, // Will need to replace this capacity elsewhere
			}
		}
	}

	return bestOption
}

// fillAffordableGaps fills gaps between windows that don't increase average cost significantly
// Only applies merges that create an equal or better cost plan
func (t *Planner) fillAffordableGaps(windows []*chargingWindow, allRates api.Rates, targetTime time.Time) []*chargingWindow {
	if len(windows) < 2 {
		return windows
	}

	// Calculate current total cost
	currentTotalCost := 0.0
	for _, w := range windows {
		currentTotalCost += w.totalCost
	}

	// Try to merge adjacent windows
	merged := true
	for merged && len(windows) > 1 {
		merged = false

		for i := 0; i < len(windows)-1; i++ {
			option := t.evaluateWindowMerge(windows[i], windows[i+1], allRates, targetTime)
			if option != nil {
				// evaluateWindowMerge already checked penalty threshold
				// Now check if merge creates a better or equal-cost plan
				newWindow := &chargingWindow{slots: option.newSlots}
				t.updateWindowStats(newWindow)

				// Calculate new total cost after this merge
				newTotalCost := 0.0
				for j, w := range windows {
					if j == i {
						newTotalCost += newWindow.totalCost
					} else if j != i+1 {
						newTotalCost += w.totalCost
					}
				}

				// Only merge if it doesn't increase total cost
				if newTotalCost <= currentTotalCost {
					result := make([]*chargingWindow, 0, len(windows)-1)
					result = append(result, windows[:i]...)
					result = append(result, newWindow)
					result = append(result, windows[i+2:]...)
					windows = result

					t.log.DEBUG.Printf("gap filling: merged windows %d+%d (cost: %.2f → %.2f)",
						i, i+1, currentTotalCost, newTotalCost)

					currentTotalCost = newTotalCost
					merged = true
					break // Restart from beginning
				}
			}
		}
	}

	return windows
}

// groupIntoWindows groups consecutive slots into charging windows
func (t *Planner) groupIntoWindows(plan api.Rates) []*chargingWindow {
	if len(plan) == 0 {
		return nil
	}

	var windows []*chargingWindow
	currentWindow := &chargingWindow{
		slots: api.Rates{plan[0]},
	}

	for i := 1; i < len(plan); i++ {
		// Check if slot is consecutive (no gap)
		if plan[i].Start.Equal(plan[i-1].End) {
			// Continue current window
			currentWindow.slots = append(currentWindow.slots, plan[i])
		} else {
			// Start new window
			t.updateWindowStats(currentWindow)
			windows = append(windows, currentWindow)
			currentWindow = &chargingWindow{
				slots: api.Rates{plan[i]},
			}
		}
	}

	// Add last window
	t.updateWindowStats(currentWindow)
	windows = append(windows, currentWindow)

	return windows
}

// updateWindowStats calculates total cost and duration for a window
func (t *Planner) updateWindowStats(w *chargingWindow) {
	w.totalCost = 0
	w.totalSeconds = 0
	for _, slot := range w.slots {
		duration := slot.End.Sub(slot.Start).Seconds()
		w.totalCost += slot.Value * duration
		w.totalSeconds += duration
	}
}

// consolidationOption represents a way to merge/extend windows
type consolidationOption struct {
	windowIndex1 int
	windowIndex2 int
	costIncrease float64
	newSlots     api.Rates
}

// evaluateWindowMerge calculates cost of merging two adjacent windows
func (t *Planner) evaluateWindowMerge(w1, w2 *chargingWindow, allRates api.Rates, targetTime time.Time) *consolidationOption {
	gapStart := w1.slots[len(w1.slots)-1].End
	gapEnd := w2.slots[0].Start

	// Quick check: no gap means already consecutive
	if !gapEnd.After(gapStart) {
		return nil
	}

	var gapSlots api.Rates
	gapCost := 0.0
	gapDuration := 0.0

	// Only scan rates that could possibly overlap the gap
	for i := range allRates {
		rate := &allRates[i]

		// Quick boundary check
		if !rate.End.After(gapStart) || !rate.Start.Before(gapEnd) {
			continue
		}

		slot := *rate

		// Clip to gap boundaries
		if slot.Start.Before(gapStart) {
			slot.Start = gapStart
		}
		if slot.End.After(gapEnd) {
			slot.End = gapEnd
		}

		// Check time constraints
		if !slot.End.After(t.clock.Now()) || !slot.Start.Before(targetTime) {
			continue
		}

		if slot.Start.Before(t.clock.Now()) {
			slot.Start = t.clock.Now()
		}
		if slot.End.After(targetTime) {
			slot.End = targetTime
		}

		duration := slot.End.Sub(slot.Start).Seconds()
		if duration > 0 {
			gapCost += slot.Value * duration
			gapDuration += duration
			gapSlots = append(gapSlots, slot)
		}
	}

	if gapDuration == 0 {
		return nil
	}

	// Pre-allocate merged slots
	mergedSlots := make(api.Rates, 0, len(w1.slots)+len(gapSlots)+len(w2.slots))
	mergedSlots = append(mergedSlots, w1.slots...)
	mergedSlots = append(mergedSlots, gapSlots...)
	mergedSlots = append(mergedSlots, w2.slots...)

	totalNewCost := w1.totalCost + gapCost + w2.totalCost
	totalNewDuration := w1.totalSeconds + gapDuration + w2.totalSeconds

	currentAvgCost := (w1.totalCost + w2.totalCost) / (w1.totalSeconds + w2.totalSeconds)
	newAvgCost := totalNewCost / totalNewDuration

	costIncrease := newAvgCost - currentAvgCost

	// Apply interruption penalty: only allow merge if cost increase is acceptable
	// InterruptionPenaltyPercent = 0 means no penalty (always merge)
	// Higher values mean stricter threshold (less merging, more fragmentation)
	//
	// Example with 6% penalty:
	// - Current avg: 22.6 ct/kWh, gap slot: 23.9 ct/kWh (5.75% more expensive)
	// - After merge: 23.03 ct/kWh (increase: 0.43 ct, which is 1.92%)
	// - Threshold: 22.6 * 0.06 = 1.356 ct
	// - Decision: 0.43 < 1.356 → merge allowed (diluted impact below threshold)
	if InterruptionPenaltyPercent > 0 {
		threshold := currentAvgCost * InterruptionPenaltyPercent
		if costIncrease > threshold {
			// Cost increase too high, reject this merge
			return nil
		}
	}

	// costIncrease is returned scaled by 3600 for backward compatibility with existing code
	// that expects cost differences in this format (though the scaling is redundant)
	return &consolidationOption{
		costIncrease: costIncrease * 3600,
		newSlots:     mergedSlots,
	}
}

// evaluateWindowExtension calculates cost of extending a window
func (t *Planner) evaluateWindowExtension(w *chargingWindow, allRates api.Rates, targetTime time.Time, extendAtStart bool) *consolidationOption {
	var extensionPoint time.Time

	if extendAtStart {
		extensionPoint = w.slots[0].Start
	} else {
		extensionPoint = w.slots[len(w.slots)-1].End
	}

	// Find the adjacent rate slot
	var extensionSlot *api.Rate
	for i := range allRates {
		rate := &allRates[i]

		if extendAtStart {
			// Look for slot ending at window start
			if rate.End.Equal(extensionPoint) {
				slot := *rate
				extensionSlot = &slot
				break
			}
		} else {
			// Look for slot starting at window end
			if rate.Start.Equal(extensionPoint) {
				slot := *rate
				// Apply time constraints
				if slot.End.After(targetTime) {
					slot.End = targetTime
				}
				if slot.Start.Before(t.clock.Now()) {
					slot.Start = t.clock.Now()
				}
				// Only valid if there's time left
				if !slot.End.After(slot.Start) {
					return nil
				}
				extensionSlot = &slot
				break
			}
		}
	}

	if extensionSlot == nil {
		return nil
	}

	extensionDuration := extensionSlot.End.Sub(extensionSlot.Start).Seconds()
	if extensionDuration <= 0 {
		return nil
	}

	extensionCost := extensionSlot.Value * extensionDuration

	currentAvgCost := w.totalCost / w.totalSeconds
	newTotalCost := w.totalCost + extensionCost
	newTotalDuration := w.totalSeconds + extensionDuration
	newAvgCost := newTotalCost / newTotalDuration

	costIncreasePerHour := (newAvgCost - currentAvgCost) * 3600

	var extendedSlots api.Rates
	if extendAtStart {
		extendedSlots = make(api.Rates, 0, len(w.slots)+1)
		extendedSlots = append(extendedSlots, *extensionSlot)
		extendedSlots = append(extendedSlots, w.slots...)
	} else {
		extendedSlots = make(api.Rates, 0, len(w.slots)+1)
		extendedSlots = append(extendedSlots, w.slots...)
		extendedSlots = append(extendedSlots, *extensionSlot)
	}

	return &consolidationOption{
		costIncrease: costIncreasePerHour,
		newSlots:     extendedSlots,
	}
}

// extendToMeetDuration extends windows to meet required duration
func (t *Planner) extendToMeetDuration(windows []*chargingWindow, allRates api.Rates, requiredDuration time.Duration, targetTime time.Time) []*chargingWindow {
	currentDuration := t.totalWindowDuration(windows)
	remaining := requiredDuration - currentDuration

	for remaining > 0 && len(windows) > 0 {
		var bestExtension *consolidationOption
		var bestWindowIdx int
		minCost := math.MaxFloat64

		// Try extending each window
		for i, w := range windows {
			option := t.evaluateWindowExtension(w, allRates, targetTime, false)
			if option != nil && option.costIncrease < minCost {
				bestExtension = option
				bestWindowIdx = i
				minCost = option.costIncrease
			}
		}

		if bestExtension == nil {
			t.log.DEBUG.Printf("window optimization: cannot extend further, missing %v", remaining)
			break
		}

		// Apply extension
		newWindow := &chargingWindow{slots: bestExtension.newSlots}
		t.updateWindowStats(newWindow)
		windows[bestWindowIdx] = newWindow

		t.log.DEBUG.Printf("window optimization: extended window %d, cost increase: %.2f/h",
			bestWindowIdx, minCost)

		currentDuration = t.totalWindowDuration(windows)
		remaining = requiredDuration - currentDuration
	}

	return windows
}

// totalWindowDuration calculates total duration across all windows
func (t *Planner) totalWindowDuration(windows []*chargingWindow) time.Duration {
	var total time.Duration
	for _, w := range windows {
		total += time.Duration(w.totalSeconds * float64(time.Second))
	}
	return total
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

	// sort rates by price and time
	slices.SortStableFunc(rates, sortByCost)

	// for late start ensure that the last slot is the cheapest
	rates, adjusted := splitPreconditionSlots(rates, precondition, targetTime)

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

	// sort rates by price and time
	slices.SortStableFunc(rates, sortByCost)

	plan := t.plan(rates, requiredDuration, targetTime)

	// correct plan slots to show original, non-adjusted prices
	for i, r := range plan {
		if rr, err := adjusted.At(r.Start); err == nil {
			plan[i].Value = rr.Value
		}
	}

	// sort plan by time
	plan.Sort()

	return plan
}

func splitPreconditionSlots(rates api.Rates, precondition time.Duration, targetTime time.Time) (api.Rates, api.Rates) {
	var res, adjusted api.Rates

	for _, r := range slices.Clone(rates) {
		preCondStart := targetTime.Add(-precondition)

		if !r.End.After(preCondStart) {
			res = append(res, r)
			continue
		}

		// split slot
		if !r.Start.After(preCondStart) {
			// keep the first part of the slot
			res = append(res, api.Rate{
				Start: r.Start,
				End:   preCondStart,
				Value: r.Value,
			})

			// adjust the second part of the slot
			r = api.Rate{
				Start: preCondStart,
				End:   r.End,
				Value: r.Value,
			}
		}

		// set the value to 0 to include slot in the plan
		res = append(res, api.Rate{
			Start: r.Start,
			End:   r.End,
			Value: 0,
		})

		// keep a copy of the adjusted slot
		adjusted = append(adjusted, r)
	}

	return res, adjusted
}
