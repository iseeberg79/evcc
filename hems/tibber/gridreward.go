package tibber

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/hems/config"
	"github.com/evcc-io/evcc/util"
)

func init() {
	config.AddCtx("tibbergridreward", NewFromConfig)
}

const (
	loginURL             = "https://app.tibber.com/v1/login.credentials"
	gqlURL               = "https://app.tibber.com/v4/gql"
	wsURL                = "wss://app.tibber.com/v4/gql/ws"
	subprotocol          = "graphql-transport-ws"
	batteryRenewInterval = 30 * time.Second
)

// gqlMessage is a graphql-transport-ws protocol message.
type gqlMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// GridReward subscribes to the Tibber Grid Reward status and controls
// a loadpoint via external control when the grid reward is delivering.
type GridReward struct {
	log      *util.Logger
	username string
	password string
	homeId   string
	lp       loadpoint.API
	site     site.API
	refresh  time.Duration

	token       string
	tokenExpiry time.Time

	// tibberVehicles maps lowercase vehicle name to Tibber vehicle ID,
	// only for vehicles where canReadLevel=false (Tibber needs SoC from us).
	tibberVehicles map[string]string

	// vehicle state push tracking
	lastVehicle   api.Vehicle
	lastPushedSoc int
	lastPushedCap int
}

// NewFromConfig creates a GridReward HEMS from generic config.
func NewFromConfig(ctx context.Context, other map[string]any, s site.API) (*GridReward, error) {
	cc := struct {
		Username  string
		Password  string
		HomeId    string
		Loadpoint int
		Refresh   time.Duration
	}{
		Loadpoint: 1,
		Refresh:   300 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	lps := s.Loadpoints()
	if cc.Loadpoint < 1 || cc.Loadpoint > len(lps) {
		return nil, fmt.Errorf("invalid loadpoint index %d (have %d)", cc.Loadpoint, len(lps))
	}

	return &GridReward{
		log:      util.NewLogger("tibber-gridreward").Redact(cc.Password, cc.HomeId),
		username: cc.Username,
		password: cc.Password,
		homeId:   cc.HomeId,
		lp:       lps[cc.Loadpoint-1],
		site:     s,
		refresh:  cc.Refresh,
	}, nil
}

// SetUpdated implements api.HEMS.
func (g *GridReward) SetUpdated(func()) {
}

// Curtailed implements hems.API. Tibber grid reward does not curtail.
func (g *GridReward) Curtailed() *bool {
    return nil
}

// Dimmed implements hems.API.
func (g *GridReward) Dimmed() *bool {
    return nil
}

// MaxConsumptionPower implements api.HEMS.
func (g *GridReward) MaxConsumptionPower() float64 {
    return 0
}

// MaxProductionPower implements api.HEMS.
func (g *GridReward) MaxProductionPower() *float64 {
    return nil
}

// Run implements hems.API.
func (g *GridReward) Run() {
	ctx := context.Background()

	if err := g.fetchTibberVehicles(ctx); err != nil {
		g.log.WARN.Printf("fetch vehicles: %v", err)
	}

	bo := backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(0))

	for {
		if err := g.connect(ctx); err != nil && ctx.Err() == nil {
			g.log.ERROR.Println(err)
		} else {
			bo.Reset()
		}

		// Release control immediately when the connection is lost so evcc
		// resumes its own mode without waiting for the lease to expire.
		g.lp.SetExternalControl(0)
		g.setBatteryHold(false)

		time.Sleep(bo.NextBackOff())
	}
}

// fetchTibberVehicles builds a map of lowercase vehicle name → Tibber vehicle ID
// for all vehicles where canReadLevel=false (i.e., Tibber needs SoC from us).
// MyVehicle (from myVehicles) has no name field — IDs are listed first, then
// each vehicle is queried individually via vehicle(id:) which returns the Vehicle type.
func (g *GridReward) fetchTibberVehicles(ctx context.Context) error {
	data, err := g.gqlPost(ctx, `{ me { myVehicles { vehicles { id } } } }`)
	if err != nil {
		return fmt.Errorf("list vehicles: %w", err)
	}

	var res struct {
		Me struct {
			MyVehicles struct {
				Vehicles []struct {
					ID string `json:"id"`
				} `json:"vehicles"`
			} `json:"myVehicles"`
		} `json:"me"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return err
	}

	vehicles := make(map[string]string)
	for _, v := range res.Me.MyVehicles.Vehicles {
		vdata, err := g.gqlPost(ctx, fmt.Sprintf(
			`{ me { vehicle(id: %q) { name battery { canReadLevel } } } }`, v.ID,
		))
		if err != nil {
			g.log.DEBUG.Printf("vehicle %s: %v", v.ID, err)
			continue
		}

		var vres struct {
			Me struct {
				Vehicle struct {
					Name    string `json:"name"`
					Battery struct {
						CanReadLevel bool `json:"canReadLevel"`
					} `json:"battery"`
				} `json:"vehicle"`
			} `json:"me"`
		}
		if err := json.Unmarshal(vdata, &vres); err != nil {
			continue
		}

		name := vres.Me.Vehicle.Name
		if vres.Me.Vehicle.Battery.CanReadLevel {
			g.log.DEBUG.Printf("vehicle %q: canReadLevel=true, skipping SoC push", name)
			continue
		}

		vehicles[strings.ToLower(name)] = v.ID
		g.log.DEBUG.Printf("vehicle %q registered for SoC push", name)
	}

	g.tibberVehicles = vehicles
	return nil
}

// pushVehicleState sends SoC (and optionally capacity) to the Tibber offline vehicle
// matching the currently connected evcc vehicle by title.
// Only pushes when SoC-based planning is active and values have changed.
func (g *GridReward) pushVehicleState(ctx context.Context) {
	if len(g.tibberVehicles) == 0 || !g.lp.SocBasedPlanning() {
		return
	}

	vehicle := g.lp.GetVehicle()
	if vehicle == nil {
		return
	}

	tibberID, ok := g.tibberVehicles[strings.ToLower(vehicle.GetTitle())]
	if !ok {
		return
	}

	soc := g.lp.GetSoc()
	if soc <= 0 {
		return
	}

	socInt := int(math.Round(soc))

	if vehicle != g.lastVehicle {
		// vehicle changed — reset tracking so capacity gets re-pushed
		g.lastVehicle = vehicle
		g.lastPushedCap = 0
	}

	var capInt int
	if cap := vehicle.Capacity(); cap > 0 {
		capInt = int(math.Round(cap))
	}

	if socInt == g.lastPushedSoc && capInt == g.lastPushedCap {
		return
	}

	settings := fmt.Sprintf(`{ key: "offline.vehicle.batteryLevel", value: %d }`, socInt)
	if capInt > 0 && capInt != g.lastPushedCap {
		settings += fmt.Sprintf(`, { key: "offline.vehicle.batteryCapacity", value: %d }`, capInt)
	}

	mutation := fmt.Sprintf(
		`mutation { me { setVehicleSettings(id: %q settings: [%s]) { id } } }`,
		tibberID, settings,
	)

	if _, err := g.gqlPost(ctx, mutation); err != nil {
		g.log.WARN.Printf("push vehicle state: %v", err)
		return
	}

	g.log.DEBUG.Printf("pushed SoC %d%% to Tibber vehicle %q", socInt, vehicle.GetTitle())
	g.lastPushedSoc = socInt
	if capInt > 0 {
		g.lastPushedCap = capInt
	}
}

// setBatteryHold sets or clears the external battery hold mode.
// Errors are silently ignored (e.g. no battery configured).
func (g *GridReward) setBatteryHold(hold bool) {
	mode := api.BatteryUnknown
	if hold {
		mode = api.BatteryHold
	}
	_ = g.site.SetBatteryModeExternal(mode)
}

// gqlPost sends a GraphQL query/mutation to the Tibber app API.
func (g *GridReward) gqlPost(ctx context.Context, query string) (json.RawMessage, error) {
	token, err := g.fetchToken(ctx)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Errors != nil {
		return nil, fmt.Errorf("%s", result.Errors)
	}

	return result.Data, nil
}

// fetchToken returns a cached Tibber app JWT, refreshing when near expiry.
func (g *GridReward) fetchToken(ctx context.Context) (string, error) {
	if g.token != "" && time.Now().Before(g.tokenExpiry) {
		return g.token, nil
	}

	body := strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, g.username, g.password))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", fmt.Errorf("tibber authentication failed")
	}

	g.token = result.Token
	g.tokenExpiry = time.Now().Add(25 * time.Minute)

	return g.token, nil
}

// connect performs one full WebSocket session: dial → handshake → subscribe → event loop.
// Returns when the connection drops or an unrecoverable error occurs.
func (g *GridReward) connect(ctx context.Context) error {
	token, err := g.fetchToken(ctx)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{subprotocol},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	// graphql-transport-ws handshake
	if err := g.writeMsg(ctx, conn, map[string]any{"type": "connection_init"}); err != nil {
		return fmt.Errorf("connection_init: %w", err)
	}

	var ack gqlMessage
	if err := g.readMsg(ctx, conn, &ack); err != nil {
		return fmt.Errorf("connection_ack: %w", err)
	}
	if ack.Type != "connection_ack" {
		return fmt.Errorf("expected connection_ack, got %q", ack.Type)
	}

	// Subscribe to grid reward status
	query := fmt.Sprintf(`subscription { gridRewardStatus(homeId: %q) { state { __typename } } }`, g.homeId)
	if err := g.writeMsg(ctx, conn, map[string]any{
		"id":      "1",
		"type":    "subscribe",
		"payload": map[string]any{"query": query},
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	g.log.DEBUG.Println("subscribed to grid reward status")

	// Spawn reader goroutine so we can interleave renewal ticks.
	type result struct {
		msg gqlMessage
		err error
	}
	msgCh := make(chan result, 1)
	go func() {
		for {
			var msg gqlMessage
			err := g.readMsg(ctx, conn, &msg)
			msgCh <- result{msg, err}
			if err != nil {
				return
			}
		}
	}()

	lease := g.refresh + min(g.refresh/2, 60*time.Second)

	delivering := false
	stateChangedAt := time.Now()
	renewTicker := time.NewTicker(g.refresh)
	defer renewTicker.Stop()
	batteryTicker := time.NewTicker(batteryRenewInterval)
	defer batteryTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-renewTicker.C:
			since := time.Since(stateChangedAt).Round(time.Second)
			if delivering {
				g.log.DEBUG.Printf("grid reward delivering for %v, renewing lease (%v)", since, lease)
				g.lp.SetExternalControl(lease)
			} else {
				g.log.DEBUG.Printf("grid reward unavailable for %v", since)
			}
			g.pushVehicleState(ctx)

		case <-batteryTicker.C:
			if delivering {
				g.setBatteryHold(true)
			}

		case r := <-msgCh:
			if r.err != nil {
				return r.err
			}

			switch r.msg.Type {
			case "next":
				typename, err := parseGridRewardTypename(r.msg.Payload)
				if err != nil {
					g.log.WARN.Printf("parse grid reward message: %v", err)
					continue
				}

				g.log.DEBUG.Printf("grid reward state: %s", typename)

				if typename == "GridRewardDelivering" {
					delivering = true
					stateChangedAt = time.Now()
					g.lp.SetExternalControl(lease)
					g.setBatteryHold(true)
				} else {
					delivering = false
					stateChangedAt = time.Now()
					g.lp.SetExternalControl(0)
					g.setBatteryHold(false)
				}
				g.pushVehicleState(ctx)

			case "ping":
				// respond to server keepalive
				_ = g.writeMsg(ctx, conn, map[string]any{"type": "pong"})

			case "complete":
				return fmt.Errorf("subscription completed by server")

			case "error":
				return fmt.Errorf("subscription error: %s", r.msg.Payload)
			}
		}
	}
}

func (g *GridReward) writeMsg(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func (g *GridReward) readMsg(ctx context.Context, conn *websocket.Conn, msg *gqlMessage) error {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, msg)
}

// parseGridRewardTypename extracts the state __typename from a "next" message payload.
func parseGridRewardTypename(payload json.RawMessage) (string, error) {
	var p struct {
		Data struct {
			GridRewardStatus struct {
				State struct {
					Typename string `json:"__typename"`
				} `json:"state"`
			} `json:"gridRewardStatus"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", err
	}
	return p.Data.GridRewardStatus.State.Typename, nil
}
