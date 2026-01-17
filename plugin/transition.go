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
	resetModes  map[int64]bool
	currentMode *int64
	pendingMode *int64
	timer       *time.Timer
}

func init() {
	registry.AddCtx("transition", NewTransitionFromConfig)
}

func NewTransitionFromConfig(ctx context.Context, other map[string]any) (Plugin, error) {
	var cc struct {
		Timeout time.Duration
		Reset   any
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

	// parse reset modes from config or auto-detect from watchdog
	resetVal := cc.Reset
	if resetVal == nil && cc.Set.Source == "watchdog" {
		resetVal = cc.Set.Other["reset"]
	}

	if resetVal != nil {
		if arr, ok := resetVal.([]any); ok {
			for _, v := range arr {
				if mode, err := toInt64(v); err == nil {
					p.resetModes[mode] = true
				}
			}
		} else if mode, err := toInt64(resetVal); err == nil {
			p.resetModes[mode] = true
		}
	}

	return p, nil
}

func toInt64(val any) (int64, error) {
	switch v := val.(type) {
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	}
	return 0, fmt.Errorf("unsupported type: %T", val)
}

func (p *transitionPlugin) handleTransition(newMode int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// already transitioning to same mode
	if p.pendingMode != nil && *p.pendingMode == newMode {
		return nil
	}

	// cancel pending transition
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
		p.pendingMode = nil
	}

	// delay when going TO non-reset mode
	needsDelay := p.currentMode != nil && p.timeout > 0 &&
		(len(p.resetModes) == 0 || !p.resetModes[newMode])

	if !needsDelay {
		return p.applyMode(newMode)
	}

	// delayed transition: stop watchdog first if in non-reset mode
	p.pendingMode = &newMode

	if len(p.resetModes) > 0 && !p.resetModes[*p.currentMode] {
		for resetMode := range p.resetModes {
			if err := p.applyMode(resetMode); err != nil {
				return err
			}
			break
		}
	}

	p.timer = time.AfterFunc(p.timeout, func() {
		p.mu.Lock()
		defer p.mu.Unlock()

		if p.pendingMode == nil {
			return
		}

		if err := p.applyMode(*p.pendingMode); err != nil {
			p.log.ERROR.Printf("apply mode: %v", err)
		}
		p.pendingMode = nil
		p.timer = nil
	})

	return nil
}

func (p *transitionPlugin) applyMode(mode int64) error {
	set, err := p.set.IntSetter(p.ctx, "")
	if err != nil {
		return err
	}
	if err := set(mode); err != nil {
		return err
	}
	p.currentMode = &mode
	return nil
}

var _ IntSetter = (*transitionPlugin)(nil)

func (p *transitionPlugin) IntSetter(param string) (func(int64) error, error) {
	return p.handleTransition, nil
}
