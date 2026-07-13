package plugin

import (
	"context"
	"errors"
	"sync"

	"github.com/evcc-io/evcc/util"
)

// Memory holds a single named value in a device-local store. It is the in-process,
// typed counterpart to the state plugin: instead of reading a process-wide published
// value navigated by jq, both ends share a per-device cell addressed by name.
//
// It has two roles, selected by whether a nested setter is configured:
//   - sink (no set): its Setter stores the incoming value. This is the endpoint a
//     site-driven capability writes to (e.g. BatteryChargePowerLimiter).
//   - forward (set): its Setter ignores the incoming value, reads the stored one and
//     forwards it to the nested setter. This is used inside a device's control path
//     (e.g. a batterymode switch case that writes the stored value to a register),
//     so the value rides the same watchdog/reset lifecycle as the mode itself.
type Memory struct {
	ctx       context.Context
	store     *memoryStore
	name      string
	setConfig *Config
}

// memoryStore is a device-local, name-addressed value store shared by all Memory
// plugins built from the same context.
type memoryStore struct {
	mu sync.RWMutex
	m  map[string]float64
}

func (s *memoryStore) set(name string, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = v
}

func (s *memoryStore) get(name string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[name]
}

type memoryStoreKey struct{}

// WithMemoryStore returns a context carrying a fresh device-local memory store.
// Callers that build a device from plugins (e.g. a configurable meter) scope this
// once per device so Memory plugins of the same device share a store, while
// different devices stay isolated.
func WithMemoryStore(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoryStoreKey{}, &memoryStore{m: make(map[string]float64)})
}

// memoryStoreFromContext returns the device-local store, or a standalone one if the
// context carries none. A standalone store is not shared, so sink and forward ends
// only interoperate when WithMemoryStore scoped their common context.
func memoryStoreFromContext(ctx context.Context) *memoryStore {
	if s, ok := ctx.Value(memoryStoreKey{}).(*memoryStore); ok {
		return s
	}
	return &memoryStore{m: make(map[string]float64)}
}

func init() {
	registry.AddCtx("memory", NewMemoryFromConfig)
}

// NewMemoryFromConfig creates a memory provider
func NewMemoryFromConfig(ctx context.Context, other map[string]any) (Plugin, error) {
	cc := struct {
		Name string
		Set  *Config
	}{}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.Name == "" {
		return nil, errors.New("missing name")
	}

	return &Memory{
		ctx:       ctx,
		store:     memoryStoreFromContext(ctx),
		name:      cc.Name,
		setConfig: cc.Set,
	}, nil
}

var _ FloatGetter = (*Memory)(nil)

func (p *Memory) FloatGetter() (func() (float64, error), error) {
	return func() (float64, error) {
		return p.store.get(p.name), nil
	}, nil
}

func (p *Memory) forward(param string) (func() error, error) {
	set, err := p.setConfig.FloatSetter(p.ctx, param)
	if err != nil {
		return nil, err
	}
	return func() error {
		return set(p.store.get(p.name))
	}, nil
}

var _ FloatSetter = (*Memory)(nil)

// FloatSetter stores the incoming value (sink), or forwards the stored value to the
// nested setter ignoring the input (forward), depending on whether set is configured.
func (p *Memory) FloatSetter(param string) (func(float64) error, error) {
	if p.setConfig == nil {
		return func(val float64) error {
			p.store.set(p.name, val)
			return nil
		}, nil
	}

	fwd, err := p.forward(param)
	if err != nil {
		return nil, err
	}
	return func(float64) error { return fwd() }, nil
}

var _ IntSetter = (*Memory)(nil)

// IntSetter mirrors FloatSetter for int-typed control paths (e.g. a batterymode switch).
func (p *Memory) IntSetter(param string) (func(int64) error, error) {
	if p.setConfig == nil {
		return func(val int64) error {
			p.store.set(p.name, float64(val))
			return nil
		}, nil
	}

	fwd, err := p.forward(param)
	if err != nil {
		return nil, err
	}
	return func(int64) error { return fwd() }, nil
}
