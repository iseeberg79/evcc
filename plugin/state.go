package plugin

import (
	"context"
	"errors"

	"github.com/evcc-io/evcc/util"
)

// State reads a published value from the process-wide value cache (the same store
// that backs /api/state) and forwards it to a nested setter. This lets templates
// write a site-computed value (e.g. batteryHoldChargePower) to a device register
// in-process, without an HTTP round-trip to the local API.
type State struct {
	ctx       context.Context
	key       string
	index     int
	scale     float64
	setConfig *Config
}

func init() {
	registry.AddCtx("state", NewStateFromConfig)
}

// NewStateFromConfig creates a state provider
func NewStateFromConfig(ctx context.Context, other map[string]any) (Plugin, error) {
	cc := struct {
		Key   string
		Index int
		Scale float64
		Set   *Config
	}{
		Scale: 1,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.Key == "" {
		return nil, errors.New("missing key")
	}

	return &State{
		ctx:       ctx,
		key:       cc.Key,
		index:     cc.Index,
		scale:     cc.Scale,
		setConfig: cc.Set,
	}, nil
}

// value reads the cached value, selecting index for array values. Returns 0 when
// the key is unknown or the index is out of range.
func (p *State) value() float64 {
	switch v := util.DefaultParamCacheValue(p.key).(type) {
	case []int64:
		if p.index >= 0 && p.index < len(v) {
			return float64(v[p.index]) * p.scale
		}
	case []float64:
		if p.index >= 0 && p.index < len(v) {
			return v[p.index] * p.scale
		}
	case int64:
		return float64(v) * p.scale
	case float64:
		return v * p.scale
	}
	return 0
}

var _ FloatGetter = (*State)(nil)

func (p *State) FloatGetter() (func() (float64, error), error) {
	return func() (float64, error) {
		return p.value(), nil
	}, nil
}

func (p *State) forward(param string) (func() error, error) {
	if p.setConfig == nil {
		return nil, errors.New("missing set config")
	}
	set, err := p.setConfig.FloatSetter(p.ctx, param)
	if err != nil {
		return nil, err
	}
	return func() error {
		return set(p.value())
	}, nil
}

var _ IntSetter = (*State)(nil)

// IntSetter ignores the input and forwards the cached value to the nested setter.
func (p *State) IntSetter(param string) (func(int64) error, error) {
	fwd, err := p.forward(param)
	if err != nil {
		return nil, err
	}
	return func(int64) error { return fwd() }, nil
}

var _ FloatSetter = (*State)(nil)

// FloatSetter ignores the input and forwards the cached value to the nested setter.
func (p *State) FloatSetter(param string) (func(float64) error, error) {
	fwd, err := p.forward(param)
	if err != nil {
		return nil, err
	}
	return func(float64) error { return fwd() }, nil
}
