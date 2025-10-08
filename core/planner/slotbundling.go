package planner

import (
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
)

// Configuration constants for charge bundling
const (
	// DisruptionPenalty is the cost penalty per charging interruption (in price units per kWh)
	// This value is added to the average cost for each gap in the charging plan
	DisruptionPenalty = 0.05

	// MaxDisruptions is the maximum allowed number of charging interruptions
	MaxDisruptions = 3

	// MinSessionDuration is the minimum duration for a single charging session
	// Prevents very short charging sessions that could harm hardware
	MinSessionDuration = 30 * time.Minute
)

// chargingWindow represents a continuous block of charging slots
type chargingWindow struct {
	slots     api.Rates
	start     time.Time
	end       time.Time
	duration  time.Duration
	avgCost   float64
	gapBefore time.Duration // gap before this window starts (from previous window end)
}

// planCandidate represents a potential charging plan
type planCandidate struct {
	windows      []chargingWindow
	totalCost    float64
	disruptions  int
	score        float64 // lower is better
	plan         api.Rates
}

// planSlotBundled creates a charging plan using slot bundling to minimize interruptions.
// It groups consecutive slots into charging windows and finds the combination with the best score.
func (t *Planner) planSlotBundled(rates api.Rates, requiredDuration time.Duration, targetTime time.Time) api.Rates {
	// Filter and adjust slots to valid time range
	validSlots := t.filterValidSlots(rates, requiredDuration, targetTime)
	if len(validSlots) == 0 {
		return nil
	}

	// Generate all possible charging windows
	windows := t.generateChargingWindows(validSlots, targetTime)
	if len(windows) == 0 {
		return nil
	}

	// Find best combination of windows
	bestCandidate := t.findBestWindowCombination(windows, requiredDuration)
	if bestCandidate == nil {
		return nil
	}

	return bestCandidate.plan
}

// filterValidSlots filters and adjusts slots to the valid time range
func (t *Planner) filterValidSlots(rates api.Rates, requiredDuration time.Duration, targetTime time.Time) api.Rates {
	var validSlots api.Rates

	for _, rate := range rates {
		// Skip slots outside valid time range
		if !rate.End.After(t.clock.Now()) || !rate.Start.Before(targetTime) {
			continue
		}

		// Adjust slot boundaries
		slot := rate
		if slot.Start.Before(t.clock.Now()) {
			slot.Start = t.clock.Now()
		}
		if slot.End.After(targetTime) {
			slot.End = targetTime
		}

		validSlots = append(validSlots, slot)
	}

	return validSlots
}

// generateChargingWindows creates all possible continuous charging windows
func (t *Planner) generateChargingWindows(slots api.Rates, targetTime time.Time) []chargingWindow {
	if len(slots) == 0 {
		return nil
	}

	// Sort slots by start time
	slices.SortFunc(slots, func(a, b api.Rate) int {
		return a.Start.Compare(b.Start)
	})

	var windows []chargingWindow

	// Generate windows of different lengths starting from each position
	for i := 0; i < len(slots); i++ {
		window := chargingWindow{
			start: slots[i].Start,
		}

		for j := i; j < len(slots); j++ {
			// Check if slots are consecutive
			if j > i {
				prevSlot := slots[j-1]
				currSlot := slots[j]

				// If there's a gap, break this window
				if !currSlot.Start.Equal(prevSlot.End) {
					break
				}
			}

			// Add slot to window
			window.slots = append(window.slots, slots[j])
			window.end = slots[j].End
			window.duration = window.end.Sub(window.start)

			// Calculate average cost for this window
			window.avgCost = AverageCost(window.slots)

			windows = append(windows, window)
		}
	}

	return windows
}

// findBestWindowCombination finds the optimal combination of windows
func (t *Planner) findBestWindowCombination(windows []chargingWindow, requiredDuration time.Duration) *planCandidate {
	var bestCandidate *planCandidate

	// Try to find valid combinations using recursive backtracking
	t.findWindowCombinations(windows, requiredDuration, []chargingWindow{}, 0, func(combination []chargingWindow) {
		candidate := t.evaluateWindowCombination(combination, requiredDuration)
		if candidate == nil {
			return
		}

		// Keep the best candidate (lowest score)
		if bestCandidate == nil || candidate.score < bestCandidate.score {
			bestCandidate = candidate
		}
	})

	return bestCandidate
}

// findWindowCombinations recursively finds all valid window combinations
func (t *Planner) findWindowCombinations(
	windows []chargingWindow,
	remainingDuration time.Duration,
	current []chargingWindow,
	startIdx int,
	callback func([]chargingWindow),
) {
	// If we've met the duration requirement, evaluate this combination
	if remainingDuration <= 0 {
		callback(slices.Clone(current))
		return
	}

	// Try adding each remaining window
	for i := startIdx; i < len(windows); i++ {
		window := windows[i]

		// Skip if window overlaps with already selected windows
		if t.hasOverlap(current, window) {
			continue
		}

		// Skip if adding this window would exceed max disruptions
		if len(current) >= MaxDisruptions+1 {
			continue
		}

		// Add window and recurse
		newCurrent := append(current, window)
		newRemaining := remainingDuration - window.duration

		t.findWindowCombinations(windows, newRemaining, newCurrent, i+1, callback)
	}
}

// hasOverlap checks if a window overlaps with any in the current selection
func (t *Planner) hasOverlap(current []chargingWindow, window chargingWindow) bool {
	for _, w := range current {
		// Check for time overlap
		if window.start.Before(w.end) && window.end.After(w.start) {
			return true
		}
	}
	return false
}

// evaluateWindowCombination calculates the score for a window combination
func (t *Planner) evaluateWindowCombination(windows []chargingWindow, requiredDuration time.Duration) *planCandidate {
	if len(windows) == 0 {
		return nil
	}

	// Sort windows by start time
	slices.SortFunc(windows, func(a, b chargingWindow) int {
		return a.start.Compare(b.start)
	})

	// Calculate total duration and ensure we have enough
	var totalDuration time.Duration
	var allSlots api.Rates
	for _, w := range windows {
		totalDuration += w.duration
		allSlots = append(allSlots, w.slots...)
	}

	// Check if we have enough duration to meet the requirement
	if totalDuration < requiredDuration {
		return nil
	}

	// Adjust last window if we have too much duration
	if totalDuration > requiredDuration {
		excess := totalDuration - requiredDuration
		lastWindow := &windows[len(windows)-1]

		// Try to shorten the last window
		if lastWindow.duration > excess {
			lastWindow.duration -= excess
			lastWindow.end = lastWindow.end.Add(-excess)

			// Remove excess slots from the end
			var adjustedSlots api.Rates
			for _, slot := range lastWindow.slots {
				if slot.Start.Before(lastWindow.end) {
					if slot.End.After(lastWindow.end) {
						slot.End = lastWindow.end
					}
					adjustedSlots = append(adjustedSlots, slot)
				}
			}
			lastWindow.slots = adjustedSlots

			// Recalculate average cost
			if len(lastWindow.slots) > 0 {
				lastWindow.avgCost = AverageCost(lastWindow.slots)
			}

			// Rebuild allSlots
			allSlots = allSlots[:0]
			for _, w := range windows {
				allSlots = append(allSlots, w.slots...)
			}
			totalDuration = requiredDuration
		}
	}

	// Count disruptions (gaps between windows)
	disruptions := len(windows) - 1

	// Calculate weighted average cost across all windows
	var totalCost float64
	for _, w := range windows {
		totalCost += w.avgCost * float64(w.duration)
	}
	avgCost := totalCost / float64(totalDuration)

	// Calculate score: average cost + penalty for disruptions
	score := avgCost + float64(disruptions)*DisruptionPenalty

	return &planCandidate{
		windows:     windows,
		totalCost:   totalCost,
		disruptions: disruptions,
		score:       score,
		plan:        allSlots,
	}
}
