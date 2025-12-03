package templates

import (
	"context"
	"testing"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	"github.com/stretchr/testify/assert"
)

func TestHasInitParams(t *testing.T) {
	tests := []struct {
		name     string
		template *Template
		want     bool
	}{
		{
			name: "no init params",
			template: &Template{
				TemplateDefinition: TemplateDefinition{
					Params: []Param{
						{Name: "capacity", Default: "10"},
						{Name: "maxpower", Default: "5000"},
					},
				},
			},
			want: false,
		},
		{
			name: "has init params",
			template: &Template{
				TemplateDefinition: TemplateDefinition{
					Params: []Param{
						{Name: "capacity", Init: map[string]any{"source": "modbus"}},
						{Name: "maxpower", Default: "5000"},
					},
				},
			},
			want: true,
		},
		{
			name: "empty template",
			template: &Template{
				TemplateDefinition: TemplateDefinition{
					Params: []Param{},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasInitParams(tt.template)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveInitValues_NoInitParams(t *testing.T) {
	tmpl := &Template{
		TemplateDefinition: TemplateDefinition{
			Params: []Param{
				{Name: "capacity", Default: "10"},
			},
		},
	}

	values := make(map[string]any)
	modbusSettings := modbus.Settings{}

	err := ResolveInitValues(context.Background(), tmpl, modbusSettings, values)
	assert.NoError(t, err)
	assert.Empty(t, values)
}

func TestResolveInitValues_UserValueNotOverwritten(t *testing.T) {
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
						"scale": 0.1,
					},
				},
			},
		},
	}

	// User provided value
	values := map[string]any{
		"capacity": 15.5,
	}

	modbusSettings := modbus.Settings{
		URI: "192.168.1.100:502",
		ID:  1,
	}

	err := ResolveInitValues(context.Background(), tmpl, modbusSettings, values)
	assert.NoError(t, err)

	// User value should not be overwritten
	assert.Equal(t, 15.5, values["capacity"])
}

func TestResolveInitValues_InvalidSource(t *testing.T) {
	tmpl := &Template{
		TemplateDefinition: TemplateDefinition{
			Params: []Param{
				{
					Name: "capacity",
					Init: map[string]any{
						"source": "http", // unsupported source
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

	// Should not fail, just log warning and continue
	err := ResolveInitValues(context.Background(), tmpl, modbusSettings, values)
	assert.NoError(t, err)
	assert.Empty(t, values) // no value should be set
}

func TestResolveInitValues_MissingSource(t *testing.T) {
	tmpl := &Template{
		TemplateDefinition: TemplateDefinition{
			Params: []Param{
				{
					Name: "capacity",
					Init: map[string]any{
						// missing "source" key
						"register": map[string]any{
							"address": 13023,
						},
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

	// Should not fail, just log warning and continue
	err := ResolveInitValues(context.Background(), tmpl, modbusSettings, values)
	assert.NoError(t, err)
	assert.Empty(t, values)
}

func TestReadInitValue_UnsupportedSource(t *testing.T) {
	initCfg := map[string]any{
		"source": "mqtt",
	}

	modbusSettings := modbus.Settings{}
	log := newTestLogger()

	val, err := readInitValue(context.Background(), initCfg, modbusSettings, log)
	assert.Error(t, err)
	assert.Nil(t, val)
	assert.Contains(t, err.Error(), "unsupported init source")
}

func TestReadModbusInitValue_InvalidRegisterConfig(t *testing.T) {
	initCfg := map[string]any{
		"source": "modbus",
		// missing register configuration
	}

	modbusSettings := modbus.Settings{
		URI: "192.168.1.100:502",
		ID:  1,
	}

	log := newTestLogger()

	val, err := readModbusInitValue(context.Background(), initCfg, modbusSettings, log)
	assert.Error(t, err)
	assert.Nil(t, val)
}

func TestReadModbusInitValue_ConnectionFails(t *testing.T) {
	initCfg := map[string]any{
		"register": map[string]any{
			"address": 13023,
			"type":    "input",
			"decode":  "uint16",
		},
		"scale": 0.1,
	}

	// Invalid modbus settings - connection will fail
	modbusSettings := modbus.Settings{
		URI: "invalid:host:9999",
		ID:  1,
	}

	log := newTestLogger()

	val, err := readModbusInitValue(context.Background(), initCfg, modbusSettings, log)
	assert.Error(t, err)
	assert.Nil(t, val)
	// Error could be decode error or connection error depending on implementation
	assert.NotEmpty(t, err.Error())
}

// Helper function to create a test logger
func newTestLogger() *util.Logger {
	return util.NewLogger("test")
}
