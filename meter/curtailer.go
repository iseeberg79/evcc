package meter

import (
	"sync"

	"github.com/evcc-io/evcc/api"
)

var _ api.Curtailer = (*Curtailer)(nil)

// Curtailer implements api.Curtailer for plugin-based meters.
type Curtailer struct {
	mu        sync.Mutex
	active    float64               // 1.0 = not curtailed
	curtailed func() (float64, error)
	curtail   func(float64) error
}

// NewCurtailer creates a Curtailer. If curtailed is nil, in-memory state is used.
func NewCurtailer(curtail func(float64) error, curtailed func() (float64, error)) *Curtailer {
	return &Curtailer{curtail: curtail, curtailed: curtailed, active: 1.0}
}

// Curtailed implements api.Curtailer.
func (m *Curtailer) Curtailed() (float64, error) {
	if m.curtailed != nil {
		return m.curtailed()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active, nil
}

// Curtail implements api.Curtailer.
func (m *Curtailer) Curtail(rate float64) error {
	err := m.curtail(rate)
	if err == nil {
		m.mu.Lock()
		m.active = rate
		m.mu.Unlock()
	}
	return err
}
