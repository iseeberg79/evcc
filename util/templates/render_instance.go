package templates

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	"go.yaml.in/yaml/v4"
)

// Instance is an actual instantiated template
type Instance struct {
	Type  string
	Other map[string]any `yaml:",inline"`
}

// RenderInstance renders an actual configuration instance
func RenderInstance(class Class, other map[string]any) (*Instance, error) {
	return RenderInstanceWithContext(context.Background(), class, other)
}

// RenderInstanceWithContext renders an actual configuration instance with context support
func RenderInstanceWithContext(ctx context.Context, class Class, other map[string]any) (*Instance, error) {
	var cc struct {
		Template string
		Other    map[string]any `mapstructure:",remain"`
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	tmpl, err := ByName(class, cc.Template)
	if err != nil {
		return nil, err
	}

	// Resolve init values before rendering if template has init params
	if hasInitParams(&tmpl) {
		log := util.NewLogger("init")
		log.DEBUG.Printf("Template %s has init params", cc.Template)

		// Extract only modbus-relevant keys to avoid decode errors
		modbusOther := make(map[string]any)
		for _, key := range []string{"id", "uri", "device", "baudrate", "comset", "rtu", "udp"} {
			if val, ok := other[key]; ok {
				modbusOther[key] = val
			}
		}

		// Convert host+port to URI before decode (matches modbus.tpl behavior)
		if uri, hasURI := other["uri"].(string); hasURI {
			modbusOther["uri"] = uri
		} else if host, hasHost := other["host"].(string); hasHost {
			if port, hasPort := other["port"]; hasPort {
				modbusOther["uri"] = fmt.Sprintf("%s:%v", host, port)
				log.DEBUG.Printf("Converted host:port to URI: %s", modbusOther["uri"])
			}
		}

		var modbusSettings modbus.Settings
		if err := util.DecodeOther(modbusOther, &modbusSettings); err == nil {
			log.DEBUG.Printf("Decoded modbus settings: URI=%s, Device=%s, ID=%d", modbusSettings.URI, modbusSettings.Device, modbusSettings.ID)

			// Only attempt init reads if modbus settings are present
			if modbusSettings.URI != "" || modbusSettings.Device != "" {
				log.DEBUG.Printf("Calling ResolveInitValues with URI=%s", modbusSettings.URI)
				if err := ResolveInitValues(ctx, &tmpl, modbusSettings, other); err != nil {
					// Log warning but continue with rendering
					log.WARN.Printf("Failed to resolve init values: %v", err)
				}
			} else {
				log.DEBUG.Printf("No modbus URI or Device found, skipping init")
			}
		} else {
			log.DEBUG.Printf("Failed to decode modbus settings: %v", err)
		}
	}

	// Extract __refreshable_params before template rendering
	// (it's not a valid template parameter, but needs to be preserved)
	var refreshParams any
	if rp, ok := other["__refreshable_params"]; ok {
		refreshParams = rp
		delete(other, "__refreshable_params")
	}

	b, _, err := tmpl.RenderResult(RenderModeInstance, other)
	if err != nil {
		return nil, util.NewConfigError(err)
	}

	if os.Getenv("EVCC_TEMPLATE_RENDER") == cc.Template {
		fmt.Println(string(b))
	}

	var instance Instance
	if err := yaml.Unmarshal(b, &instance); err != nil {
		return nil, fmt.Errorf("%w:\n%s", err, string(b))
	}

	// Restore __refreshable_params after template rendering
	if refreshParams != nil {
		if instance.Other == nil {
			instance.Other = make(map[string]any)
		}
		instance.Other["__refreshable_params"] = refreshParams
	}

	if instance.Type == "" {
		return nil, errors.New("empty instance type- check for missing usage")
	}

	return &instance, nil
}
