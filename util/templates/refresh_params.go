package templates

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
)

// RefreshableParam represents a parameter that can be periodically refreshed from a device
type RefreshableParam struct {
	Name     string
	InitCfg  map[string]any
	Interval time.Duration
	Modbus   modbus.Settings

	mu          sync.RWMutex
	value       any
	lastUpdated time.Time
	cancel      context.CancelFunc
	log         *util.Logger
}

// SetValue sets the value (for testing)
func (r *RefreshableParam) SetValue(val any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.value = val
	r.lastUpdated = time.Now()
}

// NewRefreshableParam creates a new refreshable parameter
func NewRefreshableParam(name string, initCfg map[string]any, modbusSettings modbus.Settings) (*RefreshableParam, error) {
	// Parse interval from init config
	interval := time.Duration(0)
	if intervalCfg, ok := initCfg["interval"].(string); ok {
		parsed, err := time.ParseDuration(intervalCfg)
		if err != nil {
			return nil, fmt.Errorf("invalid interval format: %w", err)
		}
		interval = parsed
	} else if intervalCfg, ok := initCfg["interval"].(int); ok {
		// Support interval in seconds for convenience
		interval = time.Duration(intervalCfg) * time.Second
	}

	return &RefreshableParam{
		Name:     name,
		InitCfg:  initCfg,
		Interval: interval,
		Modbus:   modbusSettings,
		log:      util.NewLogger(fmt.Sprintf("refresh:%s", name)),
	}, nil
}

// Start begins periodic refresh (if interval is set)
func (r *RefreshableParam) Start(ctx context.Context, loggerName string) error {
	// Update logger to use meter name
	r.log = util.NewLogger(loggerName)

	// Read initial value
	val, err := readInitValue(ctx, r.InitCfg, r.Modbus, r.log)
	if err != nil {
		return fmt.Errorf("initial read failed: %w", err)
	}

	r.mu.Lock()
	r.value = val
	r.lastUpdated = time.Now()
	r.mu.Unlock()

	r.log.DEBUG.Printf("%s initial value: %v", r.Name, val)

	// Start periodic refresh if interval is configured
	if r.Interval > 0 {
		ctx, cancel := context.WithCancel(ctx)
		r.cancel = cancel

		go r.refreshLoop(ctx)
		r.log.TRACE.Printf("%s refresh started (interval: %v)", r.Name, r.Interval)
	}

	return nil
}

// Stop stops the periodic refresh
func (r *RefreshableParam) Stop() {
	if r.cancel != nil {
		r.cancel()
		r.log.DEBUG.Printf("Stopped periodic refresh")
	}
}

// Value returns the current value (thread-safe)
func (r *RefreshableParam) Value() any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.value
}

// LastUpdated returns when the value was last updated
func (r *RefreshableParam) LastUpdated() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastUpdated
}

// refreshLoop periodically reads the value
func (r *RefreshableParam) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

// refresh reads and updates the value
func (r *RefreshableParam) refresh(ctx context.Context) {
	val, err := readInitValue(ctx, r.InitCfg, r.Modbus, r.log)
	if err != nil {
		r.log.WARN.Printf("%s refresh failed: %v (keeping previous value)", r.Name, err)
		return
	}

	r.mu.Lock()
	oldValue := r.value
	r.value = val
	r.lastUpdated = time.Now()
	r.mu.Unlock()

	if oldValue != val {
		r.log.INFO.Printf("%s changed: %v -> %v", r.Name, oldValue, val)
	} else {
		r.log.TRACE.Printf("%s: %v (unchanged)", r.Name, val)
	}
}

// RefreshableParams manages multiple refreshable parameters
type RefreshableParams struct {
	params []*RefreshableParam
	mu     sync.RWMutex
}

// NewRefreshableParams creates a new manager for refreshable parameters
func NewRefreshableParams() *RefreshableParams {
	return &RefreshableParams{
		params: make([]*RefreshableParam, 0),
	}
}

// Count returns the number of refreshable parameters
func (rp *RefreshableParams) Count() int {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return len(rp.params)
}

// Add adds a refreshable parameter
func (rp *RefreshableParams) Add(param *RefreshableParam) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.params = append(rp.params, param)
}

// StartAll starts all refreshable parameters
func (rp *RefreshableParams) StartAll(ctx context.Context, loggerName string) error {
	rp.mu.RLock()
	defer rp.mu.RUnlock()

	for _, param := range rp.params {
		if err := param.Start(ctx, loggerName); err != nil {
			return fmt.Errorf("failed to start %s: %w", param.Name, err)
		}
	}

	return nil
}

// StopAll stops all refreshable parameters
func (rp *RefreshableParams) StopAll() {
	rp.mu.RLock()
	defer rp.mu.RUnlock()

	for _, param := range rp.params {
		param.Stop()
	}
}

// Get returns the current value of a parameter by name
func (rp *RefreshableParams) Get(name string) (any, bool) {
	rp.mu.RLock()
	defer rp.mu.RUnlock()

	for _, param := range rp.params {
		if param.Name == name {
			return param.Value(), true
		}
	}

	return nil, false
}

// GetParam returns the RefreshableParam by name
func (rp *RefreshableParams) GetParam(name string) (*RefreshableParam, bool) {
	rp.mu.RLock()
	defer rp.mu.RUnlock()

	for _, param := range rp.params {
		if param.Name == name {
			return param, true
		}
	}

	return nil, false
}
