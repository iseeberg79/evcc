package plugin

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/evcc-io/evcc/util"
)

type transitionPlugin struct {
	mu          sync.Mutex
	ctx         context.Context
	log         *util.Logger
	set         Config
	timeout     time.Duration
	resetModes  map[int64]bool // modes that don't need cooldown (from watchdog reset values)
	currentMode *int64
	pendingMode *int64
	timer       *time.Timer
	cancel      context.CancelFunc
}

func init() {
	registry.AddCtx("transition", NewTransitionFromConfig)
}

// NewTransitionFromConfig creates transition provider
func NewTransitionFromConfig(ctx context.Context, other map[string]any) (Plugin, error) {
	var cc struct {
		Timeout time.Duration
		Reset   any // Can be single value or array
		Set     Config
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	p := &transitionPlugin{
		ctx:        ctx,
		log:        util.ContextLoggerWithDefault(ctx, util.NewLogger("transition")),
		set:        cc.Set,
		timeout:    cc.Timeout,
		resetModes: make(map[int64]bool),
	}

	// Parse reset modes from direct config
	if cc.Reset != nil {
		if resetVals, ok := cc.Reset.([]any); ok {
			for _, val := range resetVals {
				if mode, err := parseIntValue(val); err == nil {
					p.resetModes[mode] = true
				}
			}
		} else {
			if mode, err := parseIntValue(cc.Reset); err == nil {
				p.resetModes[mode] = true
			}
		}
	}

	// If wrapping a watchdog and no reset specified, auto-detect from watchdog config
	if len(p.resetModes) == 0 && cc.Set.Source == "watchdog" {
		if resetVals, ok := cc.Set.Other["reset"].([]any); ok {
			for _, val := range resetVals {
				if mode, err := parseIntValue(val); err == nil {
					p.resetModes[mode] = true
				}
			}
		} else if resetVal, ok := cc.Set.Other["reset"]; ok {
			if mode, err := parseIntValue(resetVal); err == nil {
				p.resetModes[mode] = true
			}
		}
	}

	if len(p.resetModes) > 0 {
		p.log.DEBUG.Printf("reset modes (no delay when leaving these): %v", p.resetModes)
	}

	return p, nil
}

// parseIntValue converts various types to int64
func parseIntValue(val any) (int64, error) {
	switch v := val.(type) {
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	default:
		return 0, fmt.Errorf("unsupported type: %T", val)
	}
}

// handleTransition manages the transition logic
func (p *transitionPlugin) handleTransition(newMode int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// If already transitioning to the same mode, do nothing
	if p.pendingMode != nil && *p.pendingMode == newMode {
		p.log.DEBUG.Printf("already transitioning to mode %d, skipping", newMode)
		return nil
	}

	// If timer is active, cancel it (mode change during transition)
	if p.cancel != nil {
		p.log.DEBUG.Printf("cancelling pending transition to mode %d", *p.pendingMode)
		p.cancel()
		p.cancel = nil
		p.timer = nil
		p.pendingMode = nil
	}

	// Check if we need a delayed transition
	needsDelay := false

	if p.currentMode != nil && p.timeout > 0 {
		// If we have reset modes (from watchdog), delay unless going TO reset mode
		if len(p.resetModes) > 0 {
			needsDelay = !p.resetModes[newMode]
			p.log.DEBUG.Printf("transition from %d to %d: newMode in reset=%v, delay=%v",
				*p.currentMode, newMode, p.resetModes[newMode], needsDelay)
		} else {
			// No reset modes defined, always delay (simple timeout mode)
			needsDelay = true
			p.log.DEBUG.Printf("transition from %d to %d: delay=%v", *p.currentMode, newMode, p.timeout)
		}
	}

	// No delay needed or first call - apply immediately
	if !needsDelay || p.currentMode == nil {
		set, err := p.set.IntSetter(p.ctx, "")
		if err != nil {
			return err
		}

		if err := set(newMode); err != nil {
			return err
		}

		p.currentMode = &newMode
		p.log.DEBUG.Printf("immediate transition to mode %d", newMode)
		return nil
	}

	// Delayed transition - stop watchdog if needed and start timer
	p.pendingMode = &newMode
	p.log.DEBUG.Printf("delayed transition to mode %d, waiting %v", newMode, p.timeout)

	// Stop the watchdog by applying a reset mode (if not already in reset mode)
	// This prevents the old mode from being written during the delay
	if len(p.resetModes) > 0 && !p.resetModes[*p.currentMode] {
		// Find first reset mode to apply
		var resetMode int64
		for mode := range p.resetModes {
			resetMode = mode
			break
		}

		set, err := p.set.IntSetter(p.ctx, "")
		if err != nil {
			return err
		}

		p.log.DEBUG.Printf("stopping watchdog by applying reset mode %d (from non-reset mode %d)",
			resetMode, *p.currentMode)
		if err := set(resetMode); err != nil {
			return err
		}
		// Update currentMode to reset since we just applied it
		p.currentMode = &resetMode
	}

	_, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	p.timer = time.AfterFunc(p.timeout, func() {
		p.mu.Lock()
		defer p.mu.Unlock()

		if p.pendingMode == nil {
			// Transition was cancelled
			return
		}

		mode := *p.pendingMode
		p.log.DEBUG.Printf("applying delayed transition to mode %d", mode)

		set, err := p.set.IntSetter(p.ctx, "")
		if err != nil {
			p.log.ERROR.Printf("failed to get setter: %v", err)
			return
		}

		if err := set(mode); err != nil {
			p.log.ERROR.Printf("failed to apply mode %d: %v", mode, err)
			return
		}

		p.currentMode = &mode
		p.pendingMode = nil
		p.timer = nil
		p.cancel = nil
	})

	// Return success immediately (skip writes during transition)
	return nil
}

var _ IntSetter = (*transitionPlugin)(nil)

// IntSetter sends int request
func (p *transitionPlugin) IntSetter(param string) (func(int64) error, error) {
	return func(val int64) error {
		return p.handleTransition(val)
	}, nil
}
