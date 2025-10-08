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

// helper function to create 15-minute slots
func rates15min(prices []float64, start time.Time) api.Rates {
	res := make(api.Rates, 0, len(prices))
	slotDuration := 15 * time.Minute

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

	// Test with optimized planning enabled
	p := &Planner{
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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

func TestOptimizedPlannerVsStandard(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Pattern designed to show difference between algorithms
	// Alternating cheap/expensive slots
	prices := []float64{0.10, 0.30, 0.10, 0.30, 0.10, 0.30, 0.15, 0.15, 0.15, 0.15}

	trf := api.NewMockTariff(ctrl)
	// Use DoAndReturn to create a fresh copy of rates on each call
	trf.EXPECT().Rates().AnyTimes().DoAndReturn(func() (api.Rates, error) {
		return rates15min(prices, clock.Now()), nil
	})

	// Standard planner (optimized disabled)
	standard := &Planner{
		log:              util.NewLogger("test-standard"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: false,
	}

	// Optimized planner
	optimized := &Planner{
		log:              util.NewLogger("test-optimized"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
	}

	// Request 1 hour (4 slots)
	targetTime := clock.Now().Add(150 * time.Minute)
	standardPlan := standard.Plan(time.Hour, 0, targetTime)
	optimizedPlan := optimized.Plan(time.Hour, 0, targetTime)

	require.NotEmpty(t, standardPlan, "standard plan should not be empty")
	require.NotEmpty(t, optimizedPlan, "optimized plan should not be empty")

	// Count disruptions in each plan
	countDisruptions := func(plan api.Rates) int {
		count := 0
		sorted := plan
		sorted.Sort()
		for i := 1; i < len(sorted); i++ {
			if !sorted[i].Start.Equal(sorted[i-1].End) {
				count++
			}
		}
		return count
	}

	standardDisruptions := countDisruptions(standardPlan)
	optimizedDisruptions := countDisruptions(optimizedPlan)

	t.Logf("Standard plan disruptions: %d", standardDisruptions)
	t.Logf("Optimized plan disruptions: %d", optimizedDisruptions)

	// Optimized should have fewer disruptions
	assert.LessOrEqual(t, optimizedDisruptions, standardDisruptions,
		"optimized plan should have equal or fewer disruptions")

	// Both should have same total duration
	assert.Equal(t, Duration(standardPlan), Duration(optimizedPlan),
		"both plans should cover requested duration")
}

func TestOptimizedPlannerMinSessionDuration(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Single cheap 15-min slot surrounded by expensive ones
	prices := []float64{0.30, 0.30, 0.05, 0.30, 0.30, 0.25, 0.25}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates15min(prices, clock.Now()), nil)

	p := &Planner{
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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
		log:              util.NewLogger("test"),
		clock:            clock,
		tariff:           trf,
		slotBundlingEnabled: true,
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
