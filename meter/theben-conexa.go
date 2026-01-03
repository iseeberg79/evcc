// Theben CONEXA Smart Meter Gateway
// Implementation based on https://github.com/jannickfahlbusch/ha-ppc-smgw
// NOTE: Theben CONEXA updates readings every 15-20 minutes, not in real-time
package meter

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/jpfielding/go-http-digest/pkg/digest"
)

func init() {
	registry.Add("theben-conexa", NewThebenConexaFromConfig)
}

// ThebenConexa is a meter implementation for Theben CONEXA Smart Meter Gateways
// Protocol: JSON-RPC over HTTPS (/smgw/m2m/)
// Standard: BSI TR-03109
// Note: Readings are updated by the gateway every 15-20 minutes, not in real-time
type ThebenConexa struct {
	*request.Helper
	uri           string
	username      string
	usagePointIDs []string
	valuesG       func() (map[string]float64, error)
	log           *util.Logger
}

// NewThebenConexaFromConfig creates a Theben CONEXA meter from config
func NewThebenConexaFromConfig(other map[string]any) (api.Meter, error) {
	cc := struct {
		URI      string
		User     string
		Password string
		Refresh  time.Duration
	}{
		Refresh: 60 * time.Second, // Gateway only updates every 15-20 min, but check frequently
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.URI == "" {
		return nil, fmt.Errorf("missing uri")
	}

	if cc.User == "" || cc.Password == "" {
		return nil, api.ErrMissingCredentials
	}

	return NewThebenConexa(cc.URI, cc.User, cc.Password, cc.Refresh)
}

// NewThebenConexa creates a Theben CONEXA meter
func NewThebenConexa(uri, user, password string, refresh time.Duration) (api.Meter, error) {
	log := util.NewLogger("theben-conexa")

	// Create custom transport with TLS verification disabled (SMGW uses self-signed certs)
	customTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		ForceAttemptHTTP2: false, // Force HTTP/1.1
	}

	helper := request.NewHelper(log)
	helper.Client.Transport = digest.NewTransport(user, password, customTransport)

	m := &ThebenConexa{
		Helper:   helper,
		uri:      util.DefaultScheme(uri, "https"),
		username: user,
		log:      log,
	}

	// Build endpoint URL
	m.uri = fmt.Sprintf("%s/smgw/m2m/%s.sm/json", m.uri, user)

	// Validate connection and get usage point IDs
	log.DEBUG.Printf("validating connection and discovering usage points...")
	if err := m.discoverUsagePoints(); err != nil {
		return nil, fmt.Errorf("failed to discover usage points: %w", err)
	}
	log.DEBUG.Printf("discovered %d usage point(s)", len(m.usagePointIDs))

	// Warn about update frequency
	log.WARN.Println("Note: Theben CONEXA updates readings every 15-20 minutes, not in real-time")

	// Validate we can get readings
	if _, err := m.getMeterValues(); err != nil {
		return nil, fmt.Errorf("failed to validate meter readings: %w", err)
	}

	m.valuesG = util.Cached(m.getMeterValues, refresh)

	return m, nil
}

// JSON-RPC request/response structures
type jsonRPCRequest struct {
	Method       string                 `json:"method"`
	Database     string                 `json:"database,omitempty"`
	UsagePointID string                 `json:"usage-point-id,omitempty"`
	LastReading  string                 `json:"last-reading,omitempty"`
	Params       map[string]interface{} `json:"params,omitempty"`
}

type userInfoResponse struct {
	UserInfo struct {
		UsagePoints []struct {
			UsagePointID string `json:"usage-point-id"`
			TafState     string `json:"taf-state"`
			TafNumber    string `json:"taf-number"`
		} `json:"usage-points"`
	} `json:"user-info"`
}

type readingsResponse struct {
	Readings struct {
		Channels []struct {
			Obis     string `json:"obis"` // Hex format: "0100010800ff"
			Readings []struct {
				Value       interface{} `json:"value"` // Can be int or string
				Unit        int         `json:"unit,omitempty"`   // 27=W, 30=Wh, 33=A, 35=V
				Scaler      int         `json:"scaler,omitempty"` // Power of 10 multiplier
				CaptureTime string      `json:"capture-time"`
			} `json:"readings"`
		} `json:"channels"`
	} `json:"readings"`
}

// callJSONRPC executes a JSON-RPC method call
func (m *ThebenConexa) callJSONRPC(req interface{}, resp interface{}) error {
	if err := m.PostJSON(m.uri, req, resp); err != nil {
		return fmt.Errorf("JSON-RPC request failed: %w", err)
	}
	return nil
}

// discoverUsagePoints finds usage point IDs (meters) connected to the gateway
// TAF-7 usage points typically provide more detailed metering data (including power)
func (m *ThebenConexa) discoverUsagePoints() error {
	req := jsonRPCRequest{
		Method: "user-info",
	}

	var resp userInfoResponse
	if err := m.callJSONRPC(req, &resp); err != nil {
		return err
	}

	usagePoints := resp.UserInfo.UsagePoints
	m.log.DEBUG.Printf("found %d usage point(s)", len(usagePoints))

	// Prefer TAF-7 usage points with state "running"
	// TAF-7 provides advanced metering data (potentially including power, currents, voltages)
	var taf7Points []string
	var runningPoints []string

	for _, up := range usagePoints {
		m.log.DEBUG.Printf("usage point: id=%s, taf=%s, state=%s", 
			up.UsagePointID, up.TafNumber, up.TafState)

		if up.TafState == "running" {
			if up.TafNumber == "7" {
				taf7Points = append(taf7Points, up.UsagePointID)
			} else {
				runningPoints = append(runningPoints, up.UsagePointID)
			}
		}
	}

	// Priority: TAF-7 running > any running > first available
	if len(taf7Points) > 0 {
		m.usagePointIDs = taf7Points
		m.log.DEBUG.Printf("using %d TAF-7 usage point(s) (advanced metering)", len(taf7Points))
	} else if len(runningPoints) > 0 {
		m.usagePointIDs = runningPoints
		m.log.DEBUG.Printf("using %d running usage point(s) (no TAF-7 found)", len(runningPoints))
		m.log.WARN.Printf("TAF-7 not found - advanced metering data (power, phases) may not be available")
	} else if len(usagePoints) > 0 {
		m.usagePointIDs = []string{usagePoints[0].UsagePointID}
		m.log.WARN.Printf("no running usage points found, using first available")
	} else {
		return fmt.Errorf("no usage points found")
	}

	return nil
}

// convertOBISHex converts Theben's hex OBIS format to standard OBIS notation
// Example: "0100010800ff" -> "1.8.0"
func convertOBISHex(obisHex string) (string, error) {
	// Decode hex string
	data, err := hex.DecodeString(obisHex)
	if err != nil || len(data) != 6 {
		return "", fmt.Errorf("invalid OBIS hex format: %s", obisHex)
	}

	// Format is: AA BB CC DD EE FF
	// We want: C.D.E (simplified OBIS notation)
	c := data[2]
	d := data[3]
	e := data[4]

	return fmt.Sprintf("%d.%d.%d", c, d, e), nil
}

// getMeterValues retrieves current meter readings from all usage points
func (m *ThebenConexa) getMeterValues() (map[string]float64, error) {
	values := make(map[string]float64)

	for _, usagePointID := range m.usagePointIDs {
		req := jsonRPCRequest{
			Method:       "readings",
			Database:     "origin",
			UsagePointID: usagePointID,
			LastReading:  "true",
		}

		var resp readingsResponse
		if err := m.callJSONRPC(req, &resp); err != nil {
			m.log.DEBUG.Printf("failed to get readings for usage point %s: %v", usagePointID, err)
			continue
		}

		// Parse all channels
		for _, channel := range resp.Readings.Channels {
			if len(channel.Readings) == 0 {
				continue
			}

			// Convert OBIS hex to standard notation
			obisCode, err := convertOBISHex(channel.Obis)
			if err != nil {
				m.log.DEBUG.Printf("skipping channel with invalid OBIS: %s", channel.Obis)
				continue
			}

			// Use latest reading
			reading := channel.Readings[0]

			// Parse value (can be int or string)
			var rawValue float64
			switch v := reading.Value.(type) {
			case float64:
				rawValue = v
			case int:
				rawValue = float64(v)
			case string:
				parsed, err := strconv.ParseFloat(v, 64)
				if err != nil {
					m.log.DEBUG.Printf("skipping unparseable value for %s: %v", obisCode, err)
					continue
				}
				rawValue = parsed
			default:
				m.log.DEBUG.Printf("skipping unknown value type for %s: %T", obisCode, v)
				continue
			}

			// Apply scaler if present (like EMH CASA)
			if reading.Scaler != 0 {
				rawValue = rawValue * math.Pow(10, float64(reading.Scaler))
			}

			// Convert based on unit (like EMH CASA)
			var finalValue float64
			switch reading.Unit {
			case 27: // W (Watt) - keep as-is
				finalValue = rawValue
				m.log.DEBUG.Printf("parsed %s = %.2f W (raw=%v, unit=27)", obisCode, finalValue, reading.Value)
			case 30: // Wh (Watthour) - convert to kWh
				finalValue = rawValue / 1000
				m.log.DEBUG.Printf("parsed %s = %.3f kWh (raw=%v, unit=30)", obisCode, finalValue, reading.Value)
			case 33: // A (Ampere) - keep as-is
				finalValue = rawValue
				m.log.DEBUG.Printf("parsed %s = %.2f A (raw=%v, unit=33)", obisCode, finalValue, reading.Value)
			case 35: // V (Volt) - keep as-is
				finalValue = rawValue
				m.log.DEBUG.Printf("parsed %s = %.2f V (raw=%v, unit=35)", obisCode, finalValue, reading.Value)
			case 0:
				// No unit specified - assume HomeAssistant format (deci-Watts / 10000)
				finalValue = rawValue / 10000.0
				m.log.DEBUG.Printf("parsed %s = %.3f (no unit, using /10000)", obisCode, finalValue)
			default:
				// Unknown unit - skip
				m.log.DEBUG.Printf("skipping unknown unit %d for %s", reading.Unit, obisCode)
				continue
			}

			values[obisCode] = finalValue
		}
	}

	if len(values) == 0 {
		return nil, fmt.Errorf("no meter values found")
	}

	return values, nil
}

// CurrentPower implements api.Meter
// Note: Only available if gateway provides OBIS 16.7.0 (TAF-7 usually does)
func (m *ThebenConexa) CurrentPower() (float64, error) {
	values, err := m.valuesG()
	if err != nil {
		return 0, err
	}

	power, ok := values["16.7.0"]
	if !ok {
		// Try to help with debugging
		m.log.DEBUG.Printf("available OBIS codes: %v", getKeys(values))
		return 0, fmt.Errorf("power value (16.7.0) not found - check if TAF-7 usage point is active")
	}

	return power, nil
}

// Helper to get map keys for debugging
func getKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TotalEnergy implements api.MeterEnergy (OBIS 1.8.0)
func (m *ThebenConexa) TotalEnergy() (float64, error) {
	values, err := m.valuesG()
	if err != nil {
		return 0, err
	}

	energy, ok := values["1.8.0"]
	if !ok {
		return 0, fmt.Errorf("energy value (1.8.0) not found")
	}

	return energy, nil
}

// getOBISValue is a helper to extract any OBIS value
func (m *ThebenConexa) getOBISValue(obis string) (float64, error) {
	values, err := m.valuesG()
	if err != nil {
		return 0, err
	}

	v, ok := values[obis]
	if !ok {
		return 0, fmt.Errorf("OBIS value (%s) not found", obis)
	}

	return v, nil
}

// GridProduction returns total grid feed-in (OBIS 2.8.0)
func (m *ThebenConexa) GridProduction() (float64, error) {
	return m.getOBISValue("2.8.0")
}

// Currents implements api.PhaseCurrents (if available from TAF-7)
func (m *ThebenConexa) Currents() (float64, float64, float64, error) {
	values, err := m.valuesG()
	if err != nil {
		return 0, 0, 0, err
	}

	// Return 0 for missing phases (gateway may not provide all)
	l1 := values["31.7.0"]
	l2 := values["51.7.0"]
	l3 := values["71.7.0"]

	return l1, l2, l3, nil
}

// Voltages implements api.PhaseVoltages (if available from TAF-7)
func (m *ThebenConexa) Voltages() (float64, float64, float64, error) {
	values, err := m.valuesG()
	if err != nil {
		return 0, 0, 0, err
	}

	// Return 0 for missing phases
	l1 := values["32.7.0"]
	l2 := values["52.7.0"]
	l3 := values["72.7.0"]

	return l1, l2, l3, nil
}

// Powers implements api.PhasePowers (if available from TAF-7)
func (m *ThebenConexa) Powers() (float64, float64, float64, error) {
	values, err := m.valuesG()
	if err != nil {
		return 0, 0, 0, err
	}

	// Return 0 for missing phases
	l1 := values["36.7.0"]
	l2 := values["56.7.0"]
	l3 := values["76.7.0"]

	return l1, l2, l3, nil
}

var _ api.Meter = (*ThebenConexa)(nil)
var _ api.MeterEnergy = (*ThebenConexa)(nil)
var _ api.PhaseCurrents = (*ThebenConexa)(nil)
var _ api.PhaseVoltages = (*ThebenConexa)(nil)
var _ api.PhasePowers = (*ThebenConexa)(nil)
