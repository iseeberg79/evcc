package templates

import (
	"context"
	"fmt"
	"reflect"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	gridx "github.com/grid-x/modbus"
)

// isZeroOrEmpty checks if a value is nil, empty string, or numeric zero
func isZeroOrEmpty(val any) bool {
	if val == nil {
		return true
	}

	switch v := val.(type) {
	case string:
		return v == "" || v == "0"
	case int, int8, int16, int32, int64:
		return reflect.ValueOf(v).Int() == 0
	case uint, uint8, uint16, uint32, uint64:
		return reflect.ValueOf(v).Uint() == 0
	case float32, float64:
		return reflect.ValueOf(v).Float() == 0
	default:
		return false
	}
}

// ResolveInitValues reads parameter values from configured sources during initialization.
// It updates the values map with read values, which will then be used as defaults during
// template rendering. If a value is already present in the values map (user-provided),
// it will not be overwritten. If reading fails, a warning is logged and the default
// value from the template is used instead.
//
// For parameters with refresh intervals, RefreshableParam objects are created and stored
// in the values map with key "__refresh_<paramname>", which can be started later by the caller.
func ResolveInitValues(ctx context.Context, tmpl *Template, modbusSettings modbus.Settings, values map[string]any) error {
	if !hasInitParams(tmpl) {
		return nil
	}

	log := util.NewLogger("init")
	refreshParams := NewRefreshableParams()

	// Get current usage value for usage-specific parameters
	currentUsage, _ := values["usage"].(string)
	log.DEBUG.Printf("Current usage: '%s'", currentUsage)

	for _, p := range tmpl.Params {
		// Skip if no init config
		if p.Init == nil {
			continue
		}

		// Skip if param has usages defined and current usage doesn't match
		if len(p.Usages) > 0 {
			if currentUsage == "" {
				log.WARN.Printf("Parameter %s has usages defined %v but current usage is empty - skipping", p.Name, p.Usages)
				continue
			}

			found := false
			for _, usage := range p.Usages {
				if usage == currentUsage {
					found = true
					break
				}
			}
			if !found {
				log.DEBUG.Printf("Skipping init for %s (usage '%s' not in %v)", p.Name, currentUsage, p.Usages)
				continue
			}
		}

		// If init is specified, always use it (ignore user values)
		hasInterval := p.Init["interval"] != nil

		if hasInterval {
			// Create refreshable param
			rp, err := NewRefreshableParam(p.Name, p.Init, modbusSettings)
			if err != nil {
				log.WARN.Printf("Failed to create refreshable param %s: %v", p.Name, err)
				continue
			}

			// Read initial value
			val, err := readInitValue(ctx, p.Init, modbusSettings, log)
			if err != nil {
				log.WARN.Printf("Failed to read init value for %s: %v", p.Name, err)
				continue
			}

			log.DEBUG.Printf("Init read for %s: %v (refresh every %v)", p.Name, val, rp.Interval)
			values[p.Name] = val

			// Store refreshable param for later use
			refreshParams.Add(rp)
		} else {
			// One-time read
			val, err := readInitValue(ctx, p.Init, modbusSettings, log)
			if err != nil {
				log.WARN.Printf("Failed to read init value for %s: %v", p.Name, err)
				continue
			}

			log.DEBUG.Printf("Init read for %s: %v", p.Name, val)
			values[p.Name] = val
		}
	}

	// Store RefreshableParams in values map for caller to start
	if len(refreshParams.params) > 0 {
		values["__refreshable_params"] = refreshParams
	}

	return nil
}

// hasInitParams checks if any parameter has init configuration
func hasInitParams(tmpl *Template) bool {
	for _, p := range tmpl.Params {
		if p.Init != nil {
			return true
		}
	}
	return false
}

// readInitValue reads a single value from a configured source
func readInitValue(ctx context.Context, initCfg map[string]any, modbusSettings modbus.Settings, log *util.Logger) (any, error) {
	source, ok := initCfg["source"].(string)
	if !ok {
		return nil, fmt.Errorf("missing 'source' in init config")
	}

	switch source {
	case "modbus":
		return readModbusInitValue(ctx, initCfg, modbusSettings, log)
	default:
		return nil, fmt.Errorf("unsupported init source: %s", source)
	}
}

// readModbusInitValue reads a value from a modbus register
func readModbusInitValue(ctx context.Context, initCfg map[string]any, modbusSettings modbus.Settings, log *util.Logger) (any, error) {
	// Parse register configuration
	var cfg struct {
		Register modbus.Register
		Scale    float64
		Cast     string // Optional: int, float, string
		Absolute bool   // Optional: take absolute value
	}
	cfg.Scale = 1.0

	// Extract only register, scale, cast, and absolute fields
	registerCfg := make(map[string]any)
	for _, key := range []string{"register", "scale", "cast", "absolute"} {
		if val, ok := initCfg[key]; ok {
			registerCfg[key] = val
		}
	}

	if err := util.DecodeOther(registerCfg, &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode modbus init config: %w", err)
	}

	if err := cfg.Register.Error(); err != nil {
		return nil, fmt.Errorf("invalid register config: %w", err)
	}

	// Create temporary modbus connection for init read
	modbus.Lock()
	defer modbus.Unlock()

	conn, err := modbus.NewConnection(ctx, modbusSettings.URI, modbusSettings.Device,
		modbusSettings.Comset, modbusSettings.Baudrate, modbusSettings.Protocol(), modbusSettings.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to create modbus connection: %w", err)
	}

	// Get register operation and decoder
	op, err := cfg.Register.Operation()
	if err != nil {
		return nil, fmt.Errorf("failed to get register operation: %w", err)
	}

	decode, err := cfg.Register.DecodeFunc()
	if err != nil {
		return nil, fmt.Errorf("failed to get decode function: %w", err)
	}

	// Read from register
	var bytes []byte
	switch op.FuncCode {
	case gridx.FuncCodeReadHoldingRegisters:
		bytes, err = conn.ReadHoldingRegisters(op.Addr, op.Length)
	case gridx.FuncCodeReadInputRegisters:
		bytes, err = conn.ReadInputRegisters(op.Addr, op.Length)
	default:
		return nil, fmt.Errorf("unsupported function code for init read: %d (only holding and input registers supported)", op.FuncCode)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to read register %d: %w", op.Addr, err)
	}

	// Decode and scale value
	floatValue := decode(bytes) * cfg.Scale

	log.TRACE.Printf("Read modbus register %d (func=%d, length=%d): raw=%v scaled=%v",
		op.Addr, op.FuncCode, op.Length, decode(bytes), floatValue)

	// Apply absolute value if requested
	if cfg.Absolute && floatValue < 0 {
		floatValue = -floatValue
	}

	// Apply cast if specified
	var result any = floatValue
	if cfg.Cast != "" {
		switch cfg.Cast {
		case "int":
			result = int64(floatValue + 0.5) // Round to nearest integer
		case "float":
			result = floatValue // Already float64
		case "string":
			result = fmt.Sprintf("%v", floatValue)
		default:
			return nil, fmt.Errorf("unsupported cast type: %s (supported: int, float, string)", cfg.Cast)
		}
	}

	return result, nil
}
