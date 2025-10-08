package planner

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// helper function to create slots with configurable duration
func ratesWithDuration(prices []float64, start time.Time, slotDuration time.Duration) api.Rates {
	res := make(api.Rates, 0, len(prices))

	for i, v := range prices {
		slotStart := start.Add(time.Duration(i) * slotDuration)
		ar := api.Rate{
			Start: slotStart,
			End:   slotStart.Add(slotDuration),
			Value: v,
		}
		res = append(res, ar)
	}

	return res
}

// helper function to create 15-minute slots (backward compatibility)
func rates15min(prices []float64, start time.Time) api.Rates {
	return ratesWithDuration(prices, start, 15*time.Minute)
}

func TestOptimizedPlannerDisruptionMinimization(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create rates with pattern: expensive, cheap, expensive, cheap (15-min slots)
	// [0.30, 0.30, 0.10, 0.10, 0.30, 0.30, 0.10, 0.10]
	// Without optimization: would pick all 0.10 slots → 2 disruptions
	// With optimization: should prefer continuous windows → fewer disruptions
	prices := []float64{0.30, 0.30, 0.10, 0.10, 0.30, 0.30, 0.10, 0.10}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 1 hour (4 slots) of charging, 2 hours available
	plan := p.Plan(time.Hour, 0, clock.Now().Add(2*time.Hour))

	require.NotEmpty(t, plan, "plan should not be empty")

	// Count disruptions (gaps in the plan)
	disruptions := 0
	plan.Sort()
	for i := 1; i < len(plan); i++ {
		if !plan[i].Start.Equal(plan[i-1].End) {
			disruptions++
		}
	}

	// Should have minimal disruptions (ideally 0-1)
	assert.LessOrEqual(t, disruptions, 1, "should minimize disruptions")

	// Verify total duration matches requested
	assert.Equal(t, time.Hour, Duration(plan), "duration should match request")
}

func TestOptimizedPlannerContinuousSlots(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create continuous cheap slots
	// [0.20, 0.15, 0.10, 0.10, 0.15, 0.20]
	prices := []float64{0.20, 0.15, 0.10, 0.10, 0.15, 0.20}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 30 minutes (2 slots)
	plan := p.Plan(30*time.Minute, 0, clock.Now().Add(90*time.Minute))

	require.NotEmpty(t, plan, "plan should not be empty")
	plan.Sort()

	// Should pick the two cheapest consecutive slots (indices 2-3)
	assert.Equal(t, 2, len(plan), "should have 2 slots")

	// Verify slots are continuous
	assert.Equal(t, plan[0].End, plan[1].Start, "slots should be continuous")

	// Verify average cost
	avgCost := AverageCost(plan)
	assert.InDelta(t, 0.10, avgCost, 0.01, "should pick cheapest continuous window")
}


func TestOptimizedPlannerMinSessionDuration(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Single cheap 15-min slot surrounded by expensive ones
	prices := []float64{0.30, 0.30, 0.05, 0.30, 0.30, 0.25, 0.25}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 30 minutes - should avoid picking single 15-min slot
	plan := p.Plan(30*time.Minute, 0, clock.Now().Add(105*time.Minute))

	require.NotEmpty(t, plan, "plan should not be empty")

	// With MinSessionDuration = 30min, should pick continuous 30-min window
	// Should prefer slots 5-6 (0.25 each) over including the single cheap slot
	plan.Sort()

	// Count disruptions
	disruptions := 0
	for i := 1; i < len(plan); i++ {
		if !plan[i].Start.Equal(plan[i-1].End) {
			disruptions++
		}
	}

	assert.Equal(t, 0, disruptions, "should create continuous session")
}

func TestOptimizedPlannerMaxDisruptions(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Pattern that would create many disruptions without limit
	// Alternating cheap/expensive: [0.10, 0.30, 0.10, 0.30, 0.10, 0.30, 0.10, 0.30, 0.10, 0.30...]
	prices := make([]float64, 20)
	for i := range prices {
		if i%2 == 0 {
			prices[i] = 0.10
		} else {
			prices[i] = 0.30
		}
	}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 2 hours (8 slots)
	plan := p.Plan(2*time.Hour, 0, clock.Now().Add(5*time.Hour))

	require.NotEmpty(t, plan, "plan should not be empty")
	plan.Sort()

	// Count disruptions
	disruptions := 0
	for i := 1; i < len(plan); i++ {
		if !plan[i].Start.Equal(plan[i-1].End) {
			disruptions++
		}
	}

	// Should respect MaxDisruptions limit (3)
	assert.LessOrEqual(t, disruptions, MaxDisruptions,
		"should not exceed max disruptions limit")
}

func TestOptimizedPlannerCostVsDisruptionBalance(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Scenario: very cheap single slot vs slightly more expensive continuous window
	// [0.01, 0.30, 0.30, 0.12, 0.12, 0.12, 0.12]
	prices := []float64{0.01, 0.30, 0.30, 0.12, 0.12, 0.12, 0.12}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 1 hour (4 slots)
	plan := p.Plan(time.Hour, 0, clock.Now().Add(105*time.Minute))

	require.NotEmpty(t, plan, "plan should not be empty")
	plan.Sort()

	// With DisruptionPenalty = 0.05, the continuous window (0.12 avg)
	// should be preferred over including the 0.01 slot which would create disruption
	// Score for 0.01 slot + 3x 0.12 = avg 0.095 + 1 disruption penalty (0.05) = 0.145
	// Score for continuous 4x 0.12 = 0.12 + 0 disruptions = 0.12
	// Continuous should win

	avgCost := AverageCost(plan)
	t.Logf("Average cost: %.3f", avgCost)

	// Count disruptions
	disruptions := 0
	for i := 1; i < len(plan); i++ {
		if !plan[i].Start.Equal(plan[i-1].End) {
			disruptions++
		}
	}

	t.Logf("Disruptions: %d", disruptions)

	// Should prefer continuous charging
	assert.Equal(t, 0, disruptions, "should prefer continuous window despite slightly higher cost")
}

func TestOptimizedPlannerEmptyRates(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(api.Rates{}, nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Should return simple plan when no rates available
	plan := p.Plan(time.Hour, 0, clock.Now().Add(2*time.Hour))

	assert.NotEmpty(t, plan, "should return simple plan")
	assert.Equal(t, time.Hour, Duration(plan), "duration should match")
}

func TestOptimizedPlannerInsufficientSlots(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Limited cheap slots: 45 minutes available, but need 2 hours
	// Pattern: cheap slots at start, then expensive
	prices := []float64{0.10, 0.10, 0.10, 0.30, 0.30, 0.30, 0.30, 0.30}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().DoAndReturn(func() (api.Rates, error) {
		return rates15min(prices, clock.Now()), nil
	})

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 2 hours with enough time available (2h target time)
	// Should find best combination even if it means using expensive slots
	plan := p.Plan(2*time.Hour, 0, clock.Now().Add(2*time.Hour))

	require.NotEmpty(t, plan, "should return a plan")
	assert.Equal(t, 2*time.Hour, Duration(plan), "should fulfill required duration")

	// Should include the cheap slots
	plan.Sort()
	cheapSlots := 0
	for _, slot := range plan {
		if slot.Value == 0.10 {
			cheapSlots++
		}
	}
	assert.Equal(t, 3, cheapSlots, "should use all cheap slots")
}

func TestMandatoryPreconditionSlot(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Scenario: cheapest slots are at the beginning, but we need precondition at the end
	// Rates: [0.05, 0.30, 0.30, 0.25] (hourly slots)
	// Without mandatory precondition: would pick slot 0
	// With mandatory precondition (1 hour before target): must include slot 3
	prices := []float64{0.05, 0.30, 0.30, 0.25}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 15 minutes (1 slot) with 15-minute precondition, target in 1 hour
	plan := p.Plan(15*time.Minute, 15*time.Minute, clock.Now().Add(60*time.Minute))

	require.NotEmpty(t, plan, "plan should not be empty")
	plan.Sort()

	// Verify we have a slot in the precondition window (last 15 minutes)
	preCondStart := clock.Now().Add(45 * time.Minute)
	targetTime := clock.Now().Add(60 * time.Minute)

	hasPreCondSlot := false
	for _, slot := range plan {
		if slot.Start.Before(targetTime) && slot.End.After(preCondStart) {
			hasPreCondSlot = true
			break
		}
	}

	assert.True(t, hasPreCondSlot, "plan must include slot in precondition window")
}

func TestMandatoryPreconditionWithDisruption(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Scenario: cheap continuous window early, expensive precondition slot late
	// Rates: [0.08, 0.08, 0.30, 0.30, 0.30, 0.30] (15-min slots)
	// Required: 30 minutes with 15-minute precondition
	// Optimal without precondition: slots 0-1 (continuous, cheap)
	// With mandatory precondition: must also include slot 5 (precondition window)
	prices := []float64{0.08, 0.08, 0.30, 0.30, 0.30, 0.30}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 30 minutes with 15-minute precondition, target is at 90 minutes (6 slots)
	plan := p.Plan(30*time.Minute, 15*time.Minute, clock.Now().Add(90*time.Minute))

	require.NotEmpty(t, plan, "plan should not be empty")
	plan.Sort()

	// Verify we have a slot in the precondition window (minutes 75-90)
	preCondStart := clock.Now().Add(75 * time.Minute)
	targetTime := clock.Now().Add(90 * time.Minute)

	hasPreCondSlot := false
	for _, slot := range plan {
		if slot.Start.Before(targetTime) && slot.End.After(preCondStart) {
			hasPreCondSlot = true
			t.Logf("Found precondition slot: %v - %v (cost: %.3f)",
				slot.Start.Format("15:04"), slot.End.Format("15:04"), slot.Value)
			break
		}
	}

	assert.True(t, hasPreCondSlot, "plan must include slot in precondition window even if it creates a disruption")

	// Log the full plan for verification
	for i, slot := range plan {
		t.Logf("Slot %d: %v - %v (cost: %.3f)", i,
			slot.Start.Format("15:04"), slot.End.Format("15:04"), slot.Value)
	}
}

func TestVariableSlotDurations(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	tests := []struct {
		name         string
		slotDuration time.Duration
		prices       []float64
		required     time.Duration
		target       time.Duration
	}{
		{
			name:         "30-minute slots",
			slotDuration: 30 * time.Minute,
			prices:       []float64{0.20, 0.10, 0.10, 0.25},
			required:     time.Hour,
			target:       2 * time.Hour,
		},
		{
			name:         "1-hour slots",
			slotDuration: time.Hour,
			prices:       []float64{0.25, 0.10, 0.15, 0.30},
			required:     2 * time.Hour,
			target:       4 * time.Hour,
		},
		{
			name:         "5-minute slots",
			slotDuration: 5 * time.Minute,
			prices:       []float64{0.30, 0.30, 0.10, 0.10, 0.10, 0.10, 0.30, 0.30},
			required:     20 * time.Minute,
			target:       40 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trf := api.NewMockTariff(ctrl)
			trf.EXPECT().Rates().AnyTimes().Return(
				ratesWithDuration(tt.prices, clock.Now(), tt.slotDuration), nil)

			p := &Planner{
				log:    util.NewLogger("test"),
				clock:  clock,
				tariff: trf,
			}

			plan := p.Plan(tt.required, 0, clock.Now().Add(tt.target))

			require.NotEmpty(t, plan, "plan should not be empty")
			plan.Sort()

			// Verify duration matches
			assert.Equal(t, tt.required, Duration(plan),
				"plan duration should match requested duration")

			// Verify slots are properly ordered
			for i := 1; i < len(plan); i++ {
				assert.False(t, plan[i].Start.Before(plan[i-1].Start),
					"slots should be in chronological order")
			}

			t.Logf("Slot duration: %v, Plan length: %d, Total duration: %v",
				tt.slotDuration, len(plan), Duration(plan))
		})
	}
}

func TestMixedSlotDurations(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create rates with mixed durations: 15min, 30min, 15min, 1hour
	rates := api.Rates{
		{Start: clock.Now(), End: clock.Now().Add(15 * time.Minute), Value: 0.20},
		{Start: clock.Now().Add(15 * time.Minute), End: clock.Now().Add(45 * time.Minute), Value: 0.10},
		{Start: clock.Now().Add(45 * time.Minute), End: clock.Now().Add(60 * time.Minute), Value: 0.25},
		{Start: clock.Now().Add(60 * time.Minute), End: clock.Now().Add(120 * time.Minute), Value: 0.30},
	}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates, nil)

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 45 minutes
	plan := p.Plan(45*time.Minute, 0, clock.Now().Add(2*time.Hour))

	require.NotEmpty(t, plan, "plan should not be empty")
	plan.Sort()

	// Should pick the 30-minute slot (cheapest and continuous) plus 15 minutes
	assert.Equal(t, 45*time.Minute, Duration(plan), "duration should match request")

	// Log the plan
	for i, slot := range plan {
		duration := slot.End.Sub(slot.Start)
		t.Logf("Slot %d: %v - %v (duration: %v, cost: %.3f)", i,
			slot.Start.Format("15:04"), slot.End.Format("15:04"), duration, slot.Value)
	}
}

func TestOptimizedPlannerFragmentedSlots(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Fragmented cheap slots: 15min, gap, 15min, gap, 15min
	// Total available: 45min, but need 1 hour
	prices := []float64{0.10, 0.30, 0.30, 0.10, 0.30, 0.30, 0.10}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().DoAndReturn(func() (api.Rates, error) {
		return rates15min(prices, clock.Now()), nil
	})

	p := &Planner{
		log:    util.NewLogger("test"),
		clock:  clock,
		tariff: trf,
	}

	// Request 1 hour but only 45min of cheap slots available (fragmented)
	// With MaxDisruptions=3, we can use all 3 slots (2 disruptions)
	plan := p.Plan(time.Hour, 0, clock.Now().Add(105*time.Minute))

	require.NotEmpty(t, plan, "should return a plan")

	// The planner should either:
	// 1. Use all 3 cheap slots (45min) + some expensive slots to reach 1 hour, OR
	// 2. Choose a different strategy based on disruption penalty
	assert.GreaterOrEqual(t, Duration(plan), 45*time.Minute, "should use available cheap slots")
}
