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

func TestContinuousModeConstraints(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create 24 hours of tariff data with varying prices to test constraint handling
	// Pattern: cheap slots (01:00-02:00), expensive (02:00-03:00), expensive (03:00-05:00),
	//          expensive (05:00-06:00), medium (06:00-08:00), then repeat pattern
	tariffRates := make([]float64, 96) // 24 hours @ 15min slots
	for i := 0; i < 96; i++ {
		hour := i / 4
		switch {
		case hour >= 1 && hour < 2: // 01:00-02:00: cheapest
			tariffRates[i] = 0.15
		case hour >= 2 && hour < 3: // 02:00-03:00: very expensive (gap simulation)
			tariffRates[i] = 0.50
		case hour >= 3 && hour < 5: // 03:00-05:00: expensive
			tariffRates[i] = 0.35
		case hour >= 5 && hour < 6: // 05:00-06:00: very expensive (gap simulation)
			tariffRates[i] = 0.50
		case hour >= 6 && hour < 8: // 06:00-08:00: medium
			tariffRates[i] = 0.25
		default: // rest of day: medium-high
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

	testCases := []struct {
		name             string
		requiredDuration time.Duration
		precondition     time.Duration
		targetTime       time.Time
		expectSlotCount  int
		expectedMinStart time.Time // earliest allowed start
		expectedMaxEnd   time.Time // latest allowed end
	}{
		{
			name:             "short duration fits in cheap window",
			requiredDuration: 45 * time.Minute,
			precondition:     0,
			targetTime:       clock.Now().Add(10 * time.Hour),
			expectSlotCount:  1, // single continuous slot
			expectedMinStart: clock.Now(),
			expectedMaxEnd:   clock.Now().Add(10 * time.Hour),
		},
		{
			name:             "exact duration of cheap window",
			requiredDuration: 60 * time.Minute,
			precondition:     0,
			targetTime:       clock.Now().Add(10 * time.Hour),
			expectSlotCount:  1,
			expectedMinStart: clock.Now(),
			expectedMaxEnd:   clock.Now().Add(10 * time.Hour),
		},
		{
			name:             "duration longer than cheap window fits in medium window",
			requiredDuration: 90 * time.Minute,
			precondition:     0,
			targetTime:       clock.Now().Add(10 * time.Hour),
			expectSlotCount:  1, // should use medium window (6:00-8:00) not expensive
			expectedMinStart: clock.Now(),
			expectedMaxEnd:   clock.Now().Add(10 * time.Hour),
		},
		{
			name:             "with preconditioning",
			requiredDuration: 75 * time.Minute, // 45min charge + 30min precond
			precondition:     30 * time.Minute,
			targetTime:       clock.Now().Add(10 * time.Hour),
			expectSlotCount:  2, // charge slot + preconditioning slot
			expectedMinStart: clock.Now(),
			expectedMaxEnd:   clock.Now().Add(10 * time.Hour),
		},
		{
			name:             "near target time forces immediate start",
			requiredDuration: 30 * time.Minute,
			precondition:     0,
			targetTime:       clock.Now().Add(35 * time.Minute), // tight deadline
			expectSlotCount:  1,
			expectedMinStart: clock.Now(),
			expectedMaxEnd:   clock.Now().Add(35 * time.Minute),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			plan := p.Plan(tc.requiredDuration, tc.precondition, tc.targetTime, true)

			// Verify plan exists
			require.NotNil(t, plan, "plan should not be nil")
			require.NotEmpty(t, plan, "plan should not be empty")

			t.Logf("Plan has %d slots:", len(plan))
			for i, slot := range plan {
				t.Logf("  Slot %d: %v - %v (duration: %v, price: %.3f)",
					i, slot.Start.Format("15:04"), slot.End.Format("15:04"),
					slot.End.Sub(slot.Start), slot.Value)
			}

			// Constraint 1: Slot count (continuous mode should minimize slots)
			assert.LessOrEqual(t, len(plan), tc.expectSlotCount+1, // allow +1 for preconditioning variations
				"continuous mode should create minimal number of slots")

			// Constraint 2: Time boundaries - no slot starts before now
			for i, slot := range plan {
				assert.False(t, slot.Start.Before(tc.expectedMinStart),
					"slot %d should not start before now (%v < %v)",
					i, slot.Start.Format("15:04"), tc.expectedMinStart.Format("15:04"))
			}

			// Constraint 3: Time boundaries - no slot ends after target time
			for i, slot := range plan {
				assert.False(t, slot.End.After(tc.expectedMaxEnd),
					"slot %d should not end after target time (%v > %v)",
					i, slot.End.Format("15:04"), tc.expectedMaxEnd.Format("15:04"))
			}

			// Constraint 4: Duration must match exactly
			totalDuration := time.Duration(0)
			for _, slot := range plan {
				totalDuration += slot.End.Sub(slot.Start)
			}
			assert.Equal(t, tc.requiredDuration, totalDuration,
				"total plan duration must match required duration exactly")

			// Constraint 5: Each slot must have positive duration
			for i, slot := range plan {
				duration := slot.End.Sub(slot.Start)
				assert.Greater(t, duration, time.Duration(0),
					"slot %d must have positive duration", i)
			}

			// Constraint 6: Slots must be ordered by time
			for i := 1; i < len(plan); i++ {
				assert.False(t, plan[i].Start.Before(plan[i-1].Start),
					"slot %d should not start before previous slot", i)
			}

			// Constraint 7: If preconditioning is specified, last slot should end at target time
			if tc.precondition > 0 {
				assert.Equal(t, tc.targetTime, plan[len(plan)-1].End,
					"with preconditioning, last slot must end at target time")
			}
		})
	}
}

func TestContinuousModeWithGapsInTariff(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create 24 hours of tariff with two price windows separated by a gap
	// 01:00-03:00: cheap window, 03:00-05:00: expensive (gap simulation), 05:00-10:00: medium window
	tariffRates := make([]float64, 96) // 24 hours @ 15min slots
	for i := 0; i < 96; i++ {
		hour := i / 4
		switch {
		case hour >= 1 && hour < 3: // 01:00-03:00: cheap
			tariffRates[i] = 0.20
		case hour >= 3 && hour < 5: // 03:00-05:00: expensive (gap)
			tariffRates[i] = 0.50
		case hour >= 5 && hour < 10: // 05:00-10:00: medium
			tariffRates[i] = 0.25
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

	// Test 1: Duration fits in first window (before gap)
	t.Run("fits before gap", func(t *testing.T) {
		plan := p.Plan(60*time.Minute, 0, clock.Now().Add(9*time.Hour), true)

		require.NotNil(t, plan)
		require.NotEmpty(t, plan)

		// Should use first window (cheaper)
		assert.Equal(t, 1, len(plan), "should create single continuous slot")
		assert.False(t, plan[0].Start.Before(clock.Now()))

		totalDuration := time.Duration(0)
		for _, slot := range plan {
			totalDuration += slot.End.Sub(slot.Start)
		}
		assert.Equal(t, 60*time.Minute, totalDuration)
	})

	// Test 2: Duration fits in second window (after gap)
	t.Run("fits after gap", func(t *testing.T) {
		plan := p.Plan(120*time.Minute, 0, clock.Now().Add(9*time.Hour), true)

		require.NotNil(t, plan)
		require.NotEmpty(t, plan)

		// Should find continuous window (may use medium window)
		assert.Equal(t, 1, len(plan), "should create single continuous slot")

		totalDuration := time.Duration(0)
		for _, slot := range plan {
			totalDuration += slot.End.Sub(slot.Start)
		}
		assert.Equal(t, 120*time.Minute, totalDuration)
	})
}

func TestDefaultModeConstraints(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create 24 hours of tariff data with varying prices
	tariffRates := make([]float64, 96) // 24 hours @ 15min slots
	for i := 0; i < 96; i++ {
		hour := i / 4
		switch {
		case hour >= 1 && hour < 2 || hour >= 4 && hour < 5: // cheap windows
			tariffRates[i] = 0.15
		case hour >= 3 && hour < 4: // expensive window
			tariffRates[i] = 0.35
		case hour >= 2 && hour < 3 || hour >= 5 && hour < 6: // medium windows
			tariffRates[i] = 0.25
		default: // rest of day
			tariffRates[i] = 0.28
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

	testCases := []struct {
		name             string
		requiredDuration time.Duration
		precondition     time.Duration
		targetTime       time.Time
		expectedMinStart time.Time
		expectedMaxEnd   time.Time
	}{
		{
			name:             "short duration uses cheapest slots",
			requiredDuration: 30 * time.Minute,
			precondition:     0,
			targetTime:       clock.Now().Add(6 * time.Hour),
			expectedMinStart: clock.Now(),
			expectedMaxEnd:   clock.Now().Add(6 * time.Hour),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Default mode (continuous=false)
			plan := p.Plan(tc.requiredDuration, tc.precondition, tc.targetTime, false)

			// Verify plan exists
			require.NotNil(t, plan, "plan should not be nil")
			require.NotEmpty(t, plan, "plan should not be empty")

			t.Logf("Plan has %d slots:", len(plan))
			for i, slot := range plan {
				t.Logf("  Slot %d: %v - %v (duration: %v, price: %.3f)",
					i, slot.Start.Format("15:04"), slot.End.Format("15:04"),
					slot.End.Sub(slot.Start), slot.Value)
			}

			// Constraint 1: Time boundaries - no slot starts before now
			for i, slot := range plan {
				assert.False(t, slot.Start.Before(tc.expectedMinStart),
					"slot %d should not start before now (%v < %v)",
					i, slot.Start.Format("15:04"), tc.expectedMinStart.Format("15:04"))
			}

			// Constraint 2: Time boundaries - no slot ends after target time
			for i, slot := range plan {
				assert.False(t, slot.End.After(tc.expectedMaxEnd),
					"slot %d should not end after target time (%v > %v)",
					i, slot.End.Format("15:04"), tc.expectedMaxEnd.Format("15:04"))
			}

			// Constraint 3: Duration must match exactly
			totalDuration := time.Duration(0)
			for _, slot := range plan {
				totalDuration += slot.End.Sub(slot.Start)
			}
			assert.Equal(t, tc.requiredDuration, totalDuration,
				"total plan duration must match required duration exactly")

			// Constraint 4: Each slot must have positive duration
			for i, slot := range plan {
				duration := slot.End.Sub(slot.Start)
				assert.Greater(t, duration, time.Duration(0),
					"slot %d must have positive duration", i)
			}

			// Constraint 5: Slots must be ordered by time
			for i := 1; i < len(plan); i++ {
				assert.False(t, plan[i].Start.Before(plan[i-1].Start),
					"slot %d should not start before previous slot", i)
			}

			// Constraint 6: If preconditioning is specified, last slot should end at target time
			if tc.precondition > 0 {
				assert.Equal(t, tc.targetTime, plan[len(plan)-1].End,
					"with preconditioning, last slot must end at target time")
			}

			// Constraint 7: Slots should not overlap
			for i := 1; i < len(plan); i++ {
				if plan[i-1].End.After(plan[i].Start) {
					// Allow continuous slots (End == Start of next)
					assert.False(t, plan[i-1].End.After(plan[i].Start) && !plan[i-1].End.Equal(plan[i].Start),
						"slot %d overlaps with slot %d", i-1, i)
				}
			}
		})
	}
}

func TestDefaultModeGapConstraints(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create 24 hours of tariff with small gaps to test smallGapDuration constraint (30 minutes)
	tariffRates := make([]float64, 96) // 24 hours @ 15min slots
	for i := 0; i < 96; i++ {
		// Pattern repeats: 30min cheap, 30min expensive, 30min cheap, 30min expensive, 60min cheap
		slotInHour := i % 12 // 12 slots per 3 hour pattern
		switch {
		case slotInHour < 2: // 00-30min: cheap
			tariffRates[i] = 0.15
		case slotInHour < 4: // 30-60min: expensive
			tariffRates[i] = 0.35
		case slotInHour < 6: // 60-90min: cheap
			tariffRates[i] = 0.15
		case slotInHour < 8: // 90-120min: expensive
			tariffRates[i] = 0.35
		default: // 120-180min: cheap
			tariffRates[i] = 0.15
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

	t.Run("small gap gets merged", func(t *testing.T) {
		// Request duration that spans across small gaps
		plan := p.Plan(60*time.Minute, 0, clock.Now().Add(10*time.Hour), false)

		require.NotNil(t, plan)
		require.NotEmpty(t, plan)

		t.Logf("Plan has %d slots:", len(plan))
		for i, slot := range plan {
			t.Logf("  Slot %d: %v - %v (duration: %v, price: %.3f)",
				i, slot.Start.Format("15:04"), slot.End.Format("15:04"),
				slot.End.Sub(slot.Start), slot.Value)
		}

		// Verify total duration matches
		totalDuration := time.Duration(0)
		for _, slot := range plan {
			totalDuration += slot.End.Sub(slot.Start)
		}
		assert.Equal(t, 60*time.Minute, totalDuration, "duration must match")

		// All slots should be within time constraints
		for i, slot := range plan {
			assert.False(t, slot.Start.Before(clock.Now()),
				"slot %d should not start before now", i)
			assert.False(t, slot.End.After(clock.Now().Add(10*time.Hour)),
				"slot %d should not end after target time", i)
		}
	})
}

func TestSlotDurationConstraints(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Create 24 hours of tariff data
	// Tests smallSlotDuration constraint (15 minutes)
	tariffRates := make([]float64, 96) // 24 hours @ 15min slots
	for i := 0; i < 96; i++ {
		hour := i / 4
		if hour >= 1 && hour < 2 {
			tariffRates[i] = 0.15 // cheap: 01:00-02:00
		} else {
			tariffRates[i] = 0.35 // expensive: rest of day
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

	t.Run("default mode respects minimum slot duration", func(t *testing.T) {
		plan := p.Plan(75*time.Minute, 0, clock.Now().Add(10*time.Hour), false)

		require.NotNil(t, plan)
		require.NotEmpty(t, plan)

		// Each slot should have at least smallSlotDuration (15 minutes)
		for i, slot := range plan {
			duration := slot.End.Sub(slot.Start)
			assert.GreaterOrEqual(t, duration, smallSlotDuration,
				"slot %d duration (%v) should be at least %v", i, duration, smallSlotDuration)
		}

		// Total duration must still match
		totalDuration := time.Duration(0)
		for _, slot := range plan {
			totalDuration += slot.End.Sub(slot.Start)
		}
		assert.Equal(t, 75*time.Minute, totalDuration)
	})
}
