package templates

import (
	"context"

	"github.com/evcc-io/evcc/api"
)

// RefreshableDecorator wraps a meter/charger and provides dynamic parameter updates
type RefreshableDecorator struct {
	api.Meter // or api.Charger
	refreshParams *RefreshableParams
	ctx           context.Context
}

// NewRefreshableDecorator creates a decorator that manages refreshable parameters
func NewRefreshableDecorator(ctx context.Context, base api.Meter, refreshParams *RefreshableParams) (*RefreshableDecorator, error) {
	decorator := &RefreshableDecorator{
		Meter:         base,
		refreshParams: refreshParams,
		ctx:           ctx,
	}

	// Start all refreshable parameters
	if err := refreshParams.StartAll(ctx); err != nil {
		return nil, err
	}

	return decorator, nil
}

// Shutdown stops all periodic refreshes
func (d *RefreshableDecorator) Shutdown() {
	if d.refreshParams != nil {
		d.refreshParams.StopAll()
	}
}

// GetRefreshableValue returns the current value of a refreshable parameter
func (d *RefreshableDecorator) GetRefreshableValue(name string) (any, bool) {
	if d.refreshParams == nil {
		return nil, false
	}
	return d.refreshParams.Get(name)
}

// StartRefreshableParams extracts the refreshable parameters from the instance configuration
// This should be called after creating a meter/charger from a template
// NOTE: Does NOT start the params - the caller is responsible for starting them
func StartRefreshableParams(ctx context.Context, instance *Instance) (*RefreshableParams, error) {
	if rp, ok := instance.Other["__refreshable_params"].(*RefreshableParams); ok {
		// Remove from Other map to avoid serialization issues
		delete(instance.Other, "__refreshable_params")
		return rp, nil
	}

	return nil, nil
}

// RefreshableBatteryController wraps a battery controller and provides dynamic max power updates
type RefreshableBatteryController struct {
	api.BatteryController
	refreshParams *RefreshableParams
}

// NewRefreshableBatteryController wraps a battery controller with refreshable params
func NewRefreshableBatteryController(base api.BatteryController, refreshParams *RefreshableParams) *RefreshableBatteryController {
	return &RefreshableBatteryController{
		BatteryController: base,
		refreshParams:     refreshParams,
	}
}

// GetMaxChargePower returns the current max charge power (dynamically updated)
func (r *RefreshableBatteryController) GetMaxChargePower() (float64, error) {
	// Try to get refreshed value first
	if val, ok := r.refreshParams.Get("maxchargepower"); ok {
		switch v := val.(type) {
		case float64:
			return v, nil
		case int:
			return float64(v), nil
		}
	}

	// Fallback to base implementation
	if getter, ok := r.BatteryController.(interface{ GetMaxChargePower() (float64, error) }); ok {
		return getter.GetMaxChargePower()
	}

	return 0, api.ErrNotAvailable
}

// GetMaxDischargePower returns the current max discharge power (dynamically updated)
func (r *RefreshableBatteryController) GetMaxDischargePower() (float64, error) {
	// Try to get refreshed value first
	if val, ok := r.refreshParams.Get("maxdischargepower"); ok {
		switch v := val.(type) {
		case float64:
			return v, nil
		case int:
			return float64(v), nil
		}
	}

	// Fallback to base implementation
	if getter, ok := r.BatteryController.(interface{ GetMaxDischargePower() (float64, error) }); ok {
		return getter.GetMaxDischargePower()
	}

	return 0, api.ErrNotAvailable
}
