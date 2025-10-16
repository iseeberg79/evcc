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

func TestContinuousModeWithRealTariff(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// Tarifdaten: 24 Stunden mit 15-Minuten-Slots (96 Slots)
	// Simuliert realistische Preisstruktur mit günstigeren Nachtzeiten
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		hour := i / 4
		// Günstigere Preise nachts (0-6 Uhr), teurer tagsüber
		if hour >= 0 && hour < 6 {
			tariffRates[i] = 0.20 + float64(i%4)*0.01
		} else if hour >= 6 && hour < 18 {
			tariffRates[i] = 0.28 + float64(i%4)*0.01
		} else {
			tariffRates[i] = 0.25 + float64(i%4)*0.01
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

	// Szenario: 5kWh @ 11kW = ~27 Minuten Ladezeit
	// Zielzeit: 22:50 Stunden in der Zukunft
	targetTime := clock.Now().Add(22*time.Hour + 50*time.Minute)
	requiredDuration := 27 * time.Minute
	precondition := 0 * time.Minute

	t.Logf("Test parameters:")
	t.Logf("  now: %v", clock.Now())
	t.Logf("  targetTime: %v", targetTime)
	t.Logf("  requiredDuration: %v", requiredDuration)
	t.Logf("  precondition: %v", precondition)
	t.Logf("  rates count: %d", len(tariffRates))

	// Continuous Mode aktivieren
	plan := p.Plan(requiredDuration, precondition, targetTime, true)

	// Assertions
	require.NotNil(t, plan, "plan should not be nil")
	require.NotEmpty(t, plan, "plan should not be empty")

	t.Logf("Plan has %d slots:", len(plan))
	for i, slot := range plan {
		t.Logf("  Slot %d: %v - %v (duration: %v, price: %.3f)",
			i, slot.Start.Format("15:04"), slot.End.Format("15:04"),
			slot.End.Sub(slot.Start), slot.Value)
	}

	// Prüfe, dass alle Slots gültig sind
	for i, slot := range plan {
		assert.False(t, slot.Start.IsZero(), "slot %d: start time should not be zero", i)
		assert.False(t, slot.End.IsZero(), "slot %d: end time should not be zero", i)
		assert.True(t, slot.Start.Before(slot.End), "slot %d: start should be before end", i)
		assert.False(t, slot.Start.Before(clock.Now()), "slot %d: start should not be in the past", i)
		assert.False(t, slot.End.After(targetTime), "slot %d: end should not be after target time", i)
	}

	// Prüfe Gesamtdauer
	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}
	assert.Equal(t, requiredDuration, totalDuration, "total plan duration should match required duration")
}

func TestContinuousModeShortDuration(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// 24 Stunden mit 15-Minuten-Slots
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		hour := i / 4
		if hour >= 0 && hour < 6 {
			tariffRates[i] = 0.20
		} else if hour >= 6 && hour < 18 {
			tariffRates[i] = 0.28
		} else {
			tariffRates[i] = 0.25
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

	// Szenario: Sehr kurze Ladezeit (10 Minuten)
	targetTime := clock.Now().Add(23 * time.Hour)
	requiredDuration := 10 * time.Minute

	plan := p.Plan(requiredDuration, 0, targetTime, true)

	require.NotNil(t, plan, "plan should not be nil")
	require.NotEmpty(t, plan, "plan should not be empty for short duration")

	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}
	assert.Equal(t, requiredDuration, totalDuration, "total plan duration should match required duration")
}

func TestContinuousModeLongDuration(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// 24 Stunden mit 15-Minuten-Slots
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		hour := i / 4
		if hour >= 0 && hour < 6 {
			tariffRates[i] = 0.20
		} else if hour >= 6 && hour < 18 {
			tariffRates[i] = 0.28
		} else {
			tariffRates[i] = 0.25
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

	// Szenario: Längere Ladezeit (6 Stunden)
	targetTime := clock.Now().Add(23 * time.Hour)
	requiredDuration := 6 * time.Hour

	plan := p.Plan(requiredDuration, 0, targetTime, true)

	require.NotNil(t, plan, "plan should not be nil")
	require.NotEmpty(t, plan, "plan should not be empty for long duration")

	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}
	assert.Equal(t, requiredDuration, totalDuration, "total plan duration should match required duration")
}

func TestContinuousModeNearTargetTime(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// 24 Stunden mit 15-Minuten-Slots
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		hour := i / 4
		if hour >= 0 && hour < 6 {
			tariffRates[i] = 0.20
		} else if hour >= 6 && hour < 18 {
			tariffRates[i] = 0.28
		} else {
			tariffRates[i] = 0.25
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

	// Szenario: Zielzeit ist bald (2 Stunden), Ladezeit 45 Minuten
	targetTime := clock.Now().Add(2 * time.Hour)
	requiredDuration := 45 * time.Minute

	plan := p.Plan(requiredDuration, 0, targetTime, true)

	require.NotNil(t, plan, "plan should not be nil")
	require.NotEmpty(t, plan, "plan should not be empty when target time is near")

	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}
	assert.Equal(t, requiredDuration, totalDuration, "total plan duration should match required duration")
}

func TestContinuousModeWithPreconditioning(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// 24 Stunden mit 15-Minuten-Slots
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		hour := i / 4
		if hour >= 0 && hour < 6 {
			tariffRates[i] = 0.20
		} else if hour >= 6 && hour < 18 {
			tariffRates[i] = 0.28
		} else {
			tariffRates[i] = 0.25
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

	// Szenario: Mit Preconditioning
	targetTime := clock.Now().Add(23 * time.Hour)
	requiredDuration := 2 * time.Hour
	precondition := 30 * time.Minute

	plan := p.Plan(requiredDuration, precondition, targetTime, true)

	require.NotNil(t, plan, "plan should not be nil")
	require.NotEmpty(t, plan, "plan should not be empty with preconditioning")

	// Prüfe, dass mindestens ein Slot vor Preconditioning und Preconditioning selbst existiert
	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}
	assert.Equal(t, requiredDuration, totalDuration, "total plan duration should match required duration including preconditioning")

	// Prüfe, dass letzter Slot bis zur Zielzeit geht
	assert.Equal(t, targetTime, plan[len(plan)-1].End, "last slot should end at target time")
}

func TestContinuousModeVariablePrices(t *testing.T) {
	clock := clock.NewMock()
	ctrl := gomock.NewController(t)

	// 24 Stunden mit stark variierenden Preisen
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		// Simuliere realistische Preisschwankungen
		tariffRates[i] = 0.15 + float64(i%12)*0.03
	}

	trf := api.NewMockTariff(ctrl)
	trf.EXPECT().Rates().AnyTimes().Return(rates(tariffRates, clock.Now(), 15*time.Minute), nil)

	log := util.NewLogger("test")
	p := &Planner{
		log:    log,
		clock:  clock,
		tariff: trf,
	}

	// Szenario: 1 Stunde Ladezeit mit variablen Preisen
	targetTime := clock.Now().Add(23 * time.Hour)
	requiredDuration := 1 * time.Hour

	plan := p.Plan(requiredDuration, 0, targetTime, true)

	require.NotNil(t, plan, "plan should not be nil")
	require.NotEmpty(t, plan, "plan should not be empty with variable prices")

	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}
	assert.Equal(t, requiredDuration, totalDuration, "total plan duration should match required duration")

	t.Logf("Selected slot: %v - %v (price: %.3f)",
		plan[0].Start.Format("15:04"), plan[0].End.Format("15:04"), plan[0].Value)
}

func TestContinuousModeAtDayBoundary(t *testing.T) {
	// Set clock to 23:00
	clock := clock.NewMock()
	clock.Set(clock.Now().Add(23 * time.Hour))

	ctrl := gomock.NewController(t)

	// 24 Stunden mit 15-Minuten-Slots ab 23:00
	tariffRates := make([]float64, 96)
	for i := 0; i < 96; i++ {
		hour := (23 + i/4) % 24
		if hour >= 0 && hour < 6 {
			tariffRates[i] = 0.20
		} else if hour >= 6 && hour < 18 {
			tariffRates[i] = 0.28
		} else {
			tariffRates[i] = 0.25
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

	// Szenario: Zielzeit ist morgen früh 7:00 (8 Stunden), Ladezeit 2 Stunden
	targetTime := clock.Now().Add(8 * time.Hour) // 7:00 am nächsten Tag
	requiredDuration := 2 * time.Hour

	plan := p.Plan(requiredDuration, 0, targetTime, true)

	require.NotNil(t, plan, "plan should not be nil")
	require.NotEmpty(t, plan, "plan should not be empty at day boundary")

	totalDuration := time.Duration(0)
	for _, slot := range plan {
		totalDuration += slot.End.Sub(slot.Start)
	}
	assert.Equal(t, requiredDuration, totalDuration, "total plan duration should match required duration")
}
