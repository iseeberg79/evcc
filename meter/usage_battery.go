package meter

import (
	"context"
	"github.com/evcc-io/evcc/plugin"
	"github.com/evcc-io/evcc/api"
)

type batteryCapacity struct {
	Capacity float64
}

// var _ api.BatteryCapacity = (*batteryCapacity)(nil)

// Decorator returns an api.BatteryCapacity decorator
func (m *batteryCapacity) Decorator() func() float64 {
	if m.Capacity == 0 {
		return nil
	}
	return func() float64 {
		return m.Capacity
	}
}

type batteryPowerLimits struct {
	MaxChargePower    float64
	MaxDischargePower float64
}

// var _ api.BatteryPowerLimiter = (*batteryPowerLimits)(nil)

// Decorator returns an api.BatteryPowerLimiter decorator
func (m *batteryPowerLimits) Decorator() func() (float64, float64) {
	if m.MaxChargePower == 0 || m.MaxDischargePower == 0 {
		return nil
	}
	return func() (float64, float64) {
		return m.MaxChargePower, m.MaxDischargePower
	}
}

// extended structure to enable plugin-config
type batterySocLimits struct {
        MinSoc float64 `mapstructure:"minsoc"`
        MaxSoc float64 `mapstructure:"maxsoc"`

        // dynamic plugin configuration (optional, if not filled by mapstructure)
        MinSocSource *plugin.Config `mapstructure:"-"`
        MaxSocSource *plugin.Config `mapstructure:"-"`

        // cached getter functions (initialized once)
        minSocGetter func() (float64, error)
        maxSocGetter func() (float64, error)
}

// Decorator returns an api.BatterySocLimiter decorator
func (m *batterySocLimits) Decorator() func() (float64, float64) {
	if m.MinSoc == 0 && m.MaxSoc == 0 {
		return nil
	}
	return func() (float64, float64) {
		return m.MinSoc, m.MaxSoc
	}
}

// Init initializes the dynamic getter functions (call once during setup)
func (m *batterySocLimits) Init(ctx context.Context) error {
	if m.MinSocSource != nil {
		getter, err := m.MinSocSource.FloatGetter(ctx)
		if err != nil {
			return err
		}
		m.minSocGetter = getter
	}

	if m.MaxSocSource != nil {
		getter, err := m.MaxSocSource.FloatGetter(ctx)
		if err != nil {
			return err
		}
		m.maxSocGetter = getter
	}

	return nil
}

// LimitController returns an api.BatteryController decorator with support for dynamic SoC limits
func (m *batterySocLimits) LimitController(
	socG func() (float64, error),
	limitSocS func(float64) error,
) func(api.BatteryMode) error {

	return func(mode api.BatteryMode) error {
		// Start with static default values
		minSoc := m.MinSoc
		maxSoc := m.MaxSoc

		// Prefer dynamic sources if available (use cached getters)
		if m.minSocGetter != nil {
			if val, err := m.minSocGetter(); err == nil {
				minSoc = val
			}
		}

		if m.maxSocGetter != nil {
			if val, err := m.maxSocGetter(); err == nil {
				maxSoc = val
			}
		}

		switch mode {
		case api.BatteryNormal:
			// Reset to minimum value
			return limitSocS(minSoc)

		case api.BatteryHold:
			soc, err := socG()
			if err != nil {
				return err
			}

			// Keep SOC between Min and 100%
			target := min(100, max(soc, minSoc))
			return limitSocS(target)

		case api.BatteryCharge:
			// Allow charging up to MaxSoC
			return limitSocS(maxSoc)

		default:
			return api.ErrNotAvailable
		}
	}
}

// GetSocLimits returns the current min and max SoC limits.
// If dynamic sources are configured, it will read them; otherwise static defaults are returned.
func (b *batterySocLimits) GetSocLimits() (float64, float64) {
	min, max := b.MinSoc, b.MaxSoc

	// Use cached getters (already initialized)
	if b.minSocGetter != nil {
		if val, err := b.minSocGetter(); err == nil {
			min = val
		}
	}

	if b.maxSocGetter != nil {
		if val, err := b.maxSocGetter(); err == nil {
			max = val
		}
	}

	return min, max
}
