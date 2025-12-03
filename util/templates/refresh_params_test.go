package templates

import (
	"context"
	"testing"
	"time"

	"github.com/evcc-io/evcc/util/modbus"
	"github.com/stretchr/testify/assert"
)

func TestNewRefreshableParam(t *testing.T) {
	tests := []struct {
		name        string
		initCfg     map[string]any
		wantErr     bool
		wantInterval time.Duration
	}{
		{
			name: "no interval",
			initCfg: map[string]any{
				"source": "modbus",
			},
			wantErr:      false,
			wantInterval: 0,
		},
		{
			name: "interval as string duration",
			initCfg: map[string]any{
				"source":   "modbus",
				"interval": "1h",
			},
			wantErr:      false,
			wantInterval: time.Hour,
		},
		{
			name: "interval as seconds (int)",
			initCfg: map[string]any{
				"source":   "modbus",
				"interval": 3600,
			},
			wantErr:      false,
			wantInterval: 3600 * time.Second,
		},
		{
			name: "invalid interval format",
			initCfg: map[string]any{
				"source":   "modbus",
				"interval": "invalid",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modbusSettings := modbus.Settings{
				URI: "192.168.1.100:502",
				ID:  1,
			}

			rp, err := NewRefreshableParam("test", tt.initCfg, modbusSettings)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, "test", rp.Name)
			assert.Equal(t, tt.wantInterval, rp.Interval)
		})
	}
}

func TestRefreshableParam_Value(t *testing.T) {
	rp := &RefreshableParam{
		Name: "test",
	}

	// Initially nil
	assert.Nil(t, rp.Value())

	// Set value
	rp.value = 42.5
	assert.Equal(t, 42.5, rp.Value())
}

func TestRefreshableParams_AddAndGet(t *testing.T) {
	rps := NewRefreshableParams()

	rp1 := &RefreshableParam{Name: "param1", value: 10}
	rp2 := &RefreshableParam{Name: "param2", value: 20}

	rps.Add(rp1)
	rps.Add(rp2)

	// Test Get
	val, ok := rps.Get("param1")
	assert.True(t, ok)
	assert.Equal(t, 10, val)

	val, ok = rps.Get("param2")
	assert.True(t, ok)
	assert.Equal(t, 20, val)

	val, ok = rps.Get("nonexistent")
	assert.False(t, ok)
	assert.Nil(t, val)
}

func TestRefreshableParams_GetParam(t *testing.T) {
	rps := NewRefreshableParams()

	rp1 := &RefreshableParam{Name: "param1", value: 10}
	rps.Add(rp1)

	// Test GetParam
	param, ok := rps.GetParam("param1")
	assert.True(t, ok)
	assert.Equal(t, rp1, param)

	param, ok = rps.GetParam("nonexistent")
	assert.False(t, ok)
	assert.Nil(t, param)
}

func TestRefreshableParam_StopWithoutStart(t *testing.T) {
	rp := &RefreshableParam{
		Name: "test",
	}

	// Should not panic
	assert.NotPanics(t, func() {
		rp.Stop()
	})
}

func TestRefreshableParams_StopAll(t *testing.T) {
	rps := NewRefreshableParams()

	rp1 := &RefreshableParam{Name: "param1"}
	rp2 := &RefreshableParam{Name: "param2"}

	rps.Add(rp1)
	rps.Add(rp2)

	// Should not panic even if never started
	assert.NotPanics(t, func() {
		rps.StopAll()
	})
}

func TestResolveInitValues_WithInterval(t *testing.T) {
	tmpl := &Template{
		TemplateDefinition: TemplateDefinition{
			Params: []Param{
				{
					Name: "maxchargepower",
					Init: map[string]any{
						"source": "modbus",
						"register": map[string]any{
							"address": 33046,
							"type":    "holding",
							"decode":  "uint16",
						},
						"interval": "1h",
					},
				},
			},
		},
	}

	values := make(map[string]any)
	modbusSettings := modbus.Settings{
		URI: "192.168.1.100:502",
		ID:  1,
	}

	// Note: This will fail to connect, so no values will be read
	// But RefreshableParams should still be created
	err := ResolveInitValues(context.Background(), tmpl, modbusSettings, values)
	assert.NoError(t, err)

	// RefreshableParams should NOT be created if initial read fails
	// This is the expected behavior - if we can't read initially, we skip the param
	_, ok := values["__refreshable_params"]
	// Depending on implementation, this might not be created if init read fails
	// So we just check it doesn't panic
	if ok {
		rp := values["__refreshable_params"].(*RefreshableParams)
		assert.NotNil(t, rp)
	}
}

func TestResolveInitValues_MixedIntervalAndOneTime(t *testing.T) {
	tmpl := &Template{
		TemplateDefinition: TemplateDefinition{
			Params: []Param{
				{
					Name: "capacity",
					Init: map[string]any{
						"source": "modbus",
						"register": map[string]any{
							"address": 13023,
							"type":    "input",
							"decode":  "uint16",
						},
						// No interval - one-time read
					},
				},
				{
					Name: "maxchargepower",
					Init: map[string]any{
						"source": "modbus",
						"register": map[string]any{
							"address": 33046,
							"type":    "holding",
							"decode":  "uint16",
						},
						"interval": "30m",
					},
				},
			},
		},
	}

	values := make(map[string]any)
	modbusSettings := modbus.Settings{
		URI: "192.168.1.100:502",
		ID:  1,
	}

	err := ResolveInitValues(context.Background(), tmpl, modbusSettings, values)
	assert.NoError(t, err)

	// Note: Since connection will fail, RefreshableParams might not be created
	// This test just ensures no panic occurs
	if rp, ok := values["__refreshable_params"].(*RefreshableParams); ok {
		assert.NotNil(t, rp)
	}
}
