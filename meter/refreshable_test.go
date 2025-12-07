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

// MockMeterWithSocLimits implements api.Meter and api.BatterySocLimiter
type MockMeterWithSocLimits struct {
	minSoc float64
	maxSoc float64
}

func (m *MockMeterWithSocLimits) CurrentPower() (float64, error) {
	return 1000, nil
}

func (m *MockMeterWithSocLimits) GetSocLimits() (min, max float64) {
	return m.minSoc, m.maxSoc
}

// MockMeterWithMaxACPower implements api.Meter and api.MaxACPowerGetter
type MockMeterWithMaxACPower struct {
	maxACPower float64
}

func (m *MockMeterWithMaxACPower) CurrentPower() (float64, error) {
	return 1000, nil
}

func (m *MockMeterWithMaxACPower) MaxACPower() float64 {
	return m.maxACPower
}

func TestRefreshableMeter_SocLimits(t *testing.T) {
	baseMeter := &MockMeterWithSocLimits{minSoc: 10, maxSoc: 90}
	refreshParams := templates.NewRefreshableParams()

	rpMinSoc := &templates.RefreshableParam{Name: "minsoc"}
	rpMinSoc.SetValue(15.0)
	rpMaxSoc := &templates.RefreshableParam{Name: "maxsoc"}
	rpMaxSoc.SetValue(95.0)

	refreshParams.Add(rpMinSoc)
	refreshParams.Add(rpMaxSoc)

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	min, max := rm.GetSocLimits()
	assert.Equal(t, 15.0, min, "Should use refreshed min soc")
	assert.Equal(t, 95.0, max, "Should use refreshed max soc")
}

func TestRefreshableMeter_SocLimitsFallback(t *testing.T) {
	baseMeter := &MockMeterWithSocLimits{minSoc: 10, maxSoc: 90}
	refreshParams := templates.NewRefreshableParams()

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	min, max := rm.GetSocLimits()
	assert.Equal(t, 10.0, min, "Should fall back to base meter min soc")
	assert.Equal(t, 90.0, max, "Should fall back to base meter max soc")
}

func TestRefreshableMeter_MaxACPower(t *testing.T) {
	baseMeter := &MockMeterWithMaxACPower{maxACPower: 10000}
	refreshParams := templates.NewRefreshableParams()

	rpMaxACPower := &templates.RefreshableParam{Name: "maxacpower"}
	rpMaxACPower.SetValue(12000.0)

	refreshParams.Add(rpMaxACPower)

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	maxACPower := rm.MaxACPower()
	assert.Equal(t, 12000.0, maxACPower, "Should use refreshed max AC power")
}

func TestRefreshableMeter_MaxACPowerFallback(t *testing.T) {
	baseMeter := &MockMeterWithMaxACPower{maxACPower: 10000}
	refreshParams := templates.NewRefreshableParams()

	rm := newTestRefreshableMeter(baseMeter, refreshParams)

	maxACPower := rm.MaxACPower()
	assert.Equal(t, 10000.0, maxACPower, "Should fall back to base meter max AC power")
}

func TestToFloat64(t *testing.T) {
	assert.Equal(t, 42.5, toFloat64(42.5))
	assert.Equal(t, 42.0, toFloat64(42))
	assert.Equal(t, 42.0, toFloat64(int64(42)))
	assert.Equal(t, 42.0, toFloat64(uint16(42)))
	assert.Equal(t, 0.0, toFloat64("string"))
	assert.Equal(t, 0.0, toFloat64(nil))
}
