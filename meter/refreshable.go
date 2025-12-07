package meter

import (
	"context"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/cmd/shutdown"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/templates"
)

// RefreshableMeter wraps a meter and manages refreshable parameters
type RefreshableMeter struct {
	api.Meter
	refreshParams *templates.RefreshableParams
	log           *util.Logger
}

// NewRefreshableMeter creates a meter wrapper with refreshable parameters
func NewRefreshableMeter(ctx context.Context, base api.Meter, refreshParams *templates.RefreshableParams) (*RefreshableMeter, error) {
	m := &RefreshableMeter{
		Meter:         base,
		refreshParams: refreshParams,
		log:           util.NewLogger("refreshable"),
	}

	// Start all refreshable parameters
	if err := refreshParams.StartAll(ctx); err != nil {
		return nil, err
	}

	m.log.DEBUG.Printf("Started refreshable meter with %d params", refreshParams.Count())

	// Register shutdown hook to stop refresh loops
	shutdown.Register(m.Shutdown)

	return m, nil
}

// Shutdown stops all periodic refreshes
func (m *RefreshableMeter) Shutdown() {
	if m.refreshParams != nil {
		m.log.DEBUG.Println("Stopping refreshable parameters")
		m.refreshParams.StopAll()
	}
}

// BatteryPowerLimiter implementation with dynamic values
var _ api.BatteryPowerLimiter = (*RefreshableMeter)(nil)

func (m *RefreshableMeter) GetPowerLimits() (charge, discharge float64) {
	// Try to get refreshed values first
	if val, ok := m.refreshParams.Get("maxchargepower"); ok {
		charge = toFloat64(val)
	}
	if val, ok := m.refreshParams.Get("maxdischargepower"); ok {
		discharge = toFloat64(val)
	}

	// If we got both values from refresh, return them
	if charge > 0 && discharge > 0 {
		m.log.TRACE.Printf("Using refreshed power limits: charge=%.0fW discharge=%.0fW", charge, discharge)
		return charge, discharge
	}

	// Fallback to base implementation if available
	if limiter, ok := m.Meter.(api.BatteryPowerLimiter); ok {
		return limiter.GetPowerLimits()
	}

	return 0, 0
}

// BatteryCapacity implementation with dynamic value
var _ api.BatteryCapacity = (*RefreshableMeter)(nil)

func (m *RefreshableMeter) Capacity() float64 {
	// Try to get refreshed value first
	if val, ok := m.refreshParams.Get("capacity"); ok {
		cap := toFloat64(val)
		if cap > 0 {
			m.log.TRACE.Printf("Using refreshed capacity: %.1f kWh", cap)
			return cap
		}
	}

	// Fallback to base implementation if available
	if capacitor, ok := m.Meter.(api.BatteryCapacity); ok {
		return capacitor.Capacity()
	}

	return 0
}

// BatterySocLimiter implementation with dynamic values
var _ api.BatterySocLimiter = (*RefreshableMeter)(nil)

func (m *RefreshableMeter) GetSocLimits() (min, max float64) {
	// Try to get refreshed values first
	var hasRefreshedMin, hasRefreshedMax bool
	if val, ok := m.refreshParams.Get("minsoc"); ok {
		min = toFloat64(val)
		hasRefreshedMin = true
	}
	if val, ok := m.refreshParams.Get("maxsoc"); ok {
		max = toFloat64(val)
		hasRefreshedMax = true
	}

	// If we got both values from refresh, return them
	if min > 0 || max > 0 {
		m.log.DEBUG.Printf("Using refreshed soc limits: min=%.0f%% (refreshed: %v) max=%.0f%% (refreshed: %v)",
			min, hasRefreshedMin, max, hasRefreshedMax)
		return min, max
	}

	// Fallback to base implementation if available
	if limiter, ok := m.Meter.(api.BatterySocLimiter); ok {
		min, max = limiter.GetSocLimits()
		m.log.DEBUG.Printf("Using base meter soc limits: min=%.0f%% max=%.0f%%", min, max)
		return min, max
	}

	return 0, 0
}

// MaxACPowerGetter implementation with dynamic value
var _ api.MaxACPowerGetter = (*RefreshableMeter)(nil)

func (m *RefreshableMeter) MaxACPower() float64 {
	// Try to get refreshed value first
	if val, ok := m.refreshParams.Get("maxacpower"); ok {
		power := toFloat64(val)
		if power > 0 {
			m.log.TRACE.Printf("Using refreshed max AC power: %.0fW", power)
			return power
		}
	}

	// Fallback to base implementation if available
	if powerGetter, ok := m.Meter.(api.MaxACPowerGetter); ok {
		return powerGetter.MaxACPower()
	}

	return 0
}

// Helper to convert any numeric type to float64
func toFloat64(val any) float64 {
	switch v := val.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint:
		return float64(v)
	case uint16:
		return float64(v)
	case uint32:
		return float64(v)
	default:
		return 0
	}
}
