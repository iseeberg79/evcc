package planner

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func TestPriceRecalculationAfterTrimming(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create test scenario with specific prices to verify correct calculation
	// Slot 1 (01:00-02:00): 0.15
	// Slot 2 (02:00-03:00): 0.25 (gap that gets merged)
	// Slot 3 (03:00-04:00): 0.15
	// Result should be weighted average based on actual used portions
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		hour := i / 4
		switch {
		case hour >= 1 && hour < 2:
			tariffRates[i] = 0.15
		case hour >= 2 && hour < 3:
			tariffRates[i] = 0.25 // gap
		case hour >= 3 && hour < 4:
			tariffRates[i] = 0.15
		default:
			tariffRates[i] = 0.30
		}
	}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates(tariffRates, clock.Now(), 15*time.Minute), nil)

	log := util.NewLogger("test")
	p := &Planner{
		log:    log,
		clock:  clock,
		tariff: trf,
	}

	// Request 90 minutes which will select slots at 01:00 and 03:00 (total 2h)
	// Gap at 02:00 (30min with price 0.25) gets merged
	// Then excess 30min gets trimmed
	plan := p.Plan(90*time.Minute, 0, clock.Now().Add(10*time.Hour), false)

	assert.NotNil(t, plan)
	assert.NotEmpty(t, plan)

	t.Logf("Plan has %d slots:", len(plan))
	for i, slot := range plan {
		t.Logf("  Slot %d: %v - %v (duration: %v, price: %.3f)",
			i, slot.Start.Format("15:04"), slot.End.Format("15:04"),
			slot.End.Sub(slot.Start), slot.Value)
	}

	// Calculate actual average cost
	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}

	avgCost := AverageCost(plan)
	t.Logf("Average cost: %.3f", avgCost)

	// Total duration should match
	assert.Equal(t, 90*time.Minute, totalDuration, "total duration must be 90 minutes")

	// The price should reflect the actual tariff composition, not the old merged weighted average
	// If trimming removed the expensive gap part, price should be closer to 0.15
	// If trimming removed cheap parts, price should reflect that
	// The exact value depends on what was trimmed, but it should NOT be the pre-trim weighted average

	// Basic sanity check: price should be within the range of used tariff rates
	assert.GreaterOrEqual(t, avgCost, 0.15, "price should not be below minimum tariff")
	assert.LessOrEqual(t, avgCost, 0.25, "price should not exceed maximum used tariff")
}
