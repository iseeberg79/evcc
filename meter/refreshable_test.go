package meter

import (
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/stretchr/testify/assert"
)

// MockMeterWithCapacity implements api.Meter and api.BatteryCapacity
type MockMeterWithCapacity struct {
	capacity float64
}

func (m *MockMeterWithCapacity) CurrentPower() (float64, error) {
	return 1000, nil
}

func (m *MockMeterWithCapacity) Capacity() float64 {
	return m.capacity
}

// MockMeterWithPowerLimits implements api.Meter and api.BatteryPowerLimiter
type MockMeterWithPowerLimits struct {
	charge    float64
	discharge float64
}

func (m *MockMeterWithPowerLimits) CurrentPower() (float64, error) {
	return 1000, nil
}

func (m *MockMeterWithPowerLimits) GetPowerLimits() (charge, discharge float64) {
	return m.charge, m.discharge
}

// Helper to create RefreshableMeter for testing
func newTestRefreshableMeter(baseMeter api.Meter, refreshParams *templates.RefreshableParams) *RefreshableMeter {
	return &RefreshableMeter{
		Meter:         baseMeter,
		refreshParams: refreshParams,
		log:           util.NewLogger("test"),
	}
}

func TestRefreshableMeter_Capacity(t *testing.T) {
	baseMeter := &MockMeterWithCapacity{capacity: 10.0}
	refreshParams := templates.NewRefreshableParams()

	rp := &templates.RefreshableParam{Name: "capacity"}
	rp.SetValue(12.5)
	refreshParams.Add(rp)

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	capacity := rm.Capacity()
	assert.Equal(t, 12.5, capacity, "Should use refreshed value")
}

func TestRefreshableMeter_CapacityFallback(t *testing.T) {
	baseMeter := &MockMeterWithCapacity{capacity: 10.0}
	refreshParams := templates.NewRefreshableParams()

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	capacity := rm.Capacity()
	assert.Equal(t, 10.0, capacity, "Should fall back to base meter")
}

func TestRefreshableMeter_PowerLimits(t *testing.T) {
	baseMeter := &MockMeterWithPowerLimits{charge: 5000, discharge: 5000}
	refreshParams := templates.NewRefreshableParams()

	rpCharge := &templates.RefreshableParam{Name: "maxchargepower"}
	rpCharge.SetValue(6000)
	rpDischarge := &templates.RefreshableParam{Name: "maxdischargepower"}
	rpDischarge.SetValue(5500)

	refreshParams.Add(rpCharge)
	refreshParams.Add(rpDischarge)

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	charge, discharge := rm.GetPowerLimits()
	assert.Equal(t, 6000.0, charge)
	assert.Equal(t, 5500.0, discharge)
}

func TestRefreshableMeter_PowerLimitsFallback(t *testing.T) {
	baseMeter := &MockMeterWithPowerLimits{charge: 5000, discharge: 5000}
	refreshParams := templates.NewRefreshableParams()

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	charge, discharge := rm.GetPowerLimits()
	assert.Equal(t, 5000.0, charge)
	assert.Equal(t, 5000.0, discharge)
}

func TestToFloat64(t *testing.T) {
	assert.Equal(t, 42.5, toFloat64(42.5))
	assert.Equal(t, 42.0, toFloat64(42))
	assert.Equal(t, 42.0, toFloat64(int64(42)))
	assert.Equal(t, 42.0, toFloat64(uint16(42)))
	assert.Equal(t, 0.0, toFloat64("string"))
	assert.Equal(t, 0.0, toFloat64(nil))
}
