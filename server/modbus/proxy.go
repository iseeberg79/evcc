package modbus

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/andig/mbserver"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	"github.com/evcc-io/evcc/util/sponsor"
)

// connectDelay gives the device a brief settling period after the proxy's
// downstream connection reconnects, before the next request goes out - a
// timeout closes the connection (see util/modbus/connection.go), and an
// immediate reconnect attempt can otherwise land on a device that hasn't
// recovered from whatever caused the timeout, timing out again right away.
const connectDelay = 100 * time.Millisecond

func StartProxy(port int, config modbus.Settings, readOnly ReadOnlyMode) error {
	conn, err := config.Connection(context.Background())
	if err != nil {
		return err
	}
	conn.ConnectDelay(connectDelay)

	if !sponsor.IsAuthorized() {
		return api.ErrSponsorRequired
	}

	h := newHandler(util.NewLogger(fmt.Sprintf("proxy-%d", port)), readOnly, conn)

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}

	h.log.DEBUG.Printf("modbus proxy for %s listening at :%d", config.String(), port)

	srv, err := mbserver.New(h, mbserver.Logger(&logger{log: h.log}))
	if err != nil {
		return err
	}

	if err := srv.Start(l); err != nil {
		return err
	}

	go h.reportStats(context.Background(), statsInterval)

	return nil
}
