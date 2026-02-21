package tibber

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/util"
)

const (
	wsURL       = "wss://app.tibber.com/v4/gql/ws"
	subprotocol = "graphql-transport-ws"
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
	log     *util.Logger
	token   string
	homeId  string
	lp      loadpoint.API
	refresh time.Duration
}

// NewFromConfig creates a GridReward HEMS from generic config.
func NewFromConfig(ctx context.Context, other map[string]any, site site.API) (*GridReward, error) {
	cc := struct {
		Token     string
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

	if cc.Token == "" {
		return nil, api.ErrMissingToken
	}

	lps := site.Loadpoints()
	if cc.Loadpoint < 1 || cc.Loadpoint > len(lps) {
		return nil, fmt.Errorf("invalid loadpoint index %d (have %d)", cc.Loadpoint, len(lps))
	}

	return &GridReward{
		log:     util.NewLogger("tibber-gridreward").Redact(cc.Token, cc.HomeId),
		token:   cc.Token,
		homeId:  cc.HomeId,
		lp:      lps[cc.Loadpoint-1],
		refresh: cc.Refresh,
	}, nil
}

// ConsumptionLimit implements hems.API. Tibber grid reward does not impose a consumption limit.
func (g *GridReward) ConsumptionLimit() float64 {
	return 0
}

// Run implements hems.API. It connects to Tibber, subscribes to grid reward
// state changes, and drives the loadpoint's external control lease accordingly.
// It reconnects automatically on failure; when disconnected the lease expires
// naturally, returning control to evcc.
func (g *GridReward) Run() {
	ctx := context.Background()
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

		time.Sleep(bo.NextBackOff())
	}
}

// connect performs one full WebSocket session: dial → handshake → subscribe → event loop.
// Returns when the connection drops or an unrecoverable error occurs.
func (g *GridReward) connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{subprotocol},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + g.token}},
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
	renewTicker := time.NewTicker(g.refresh)
	defer renewTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-renewTicker.C:
			if delivering {
				g.log.DEBUG.Printf("renewing external control lease (%v)", lease)
				g.lp.SetExternalControl(lease)
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
					g.lp.SetExternalControl(lease)
				} else {
					delivering = false
					g.lp.SetExternalControl(0)
				}

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
