package modbus

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andig/mbserver"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConcurrentRead(t *testing.T) {
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer l.Close()

	srv, _ := mbserver.New(&echoHandler{
		id:             0,
		RequestHandler: new(mbserver.DummyHandler),
	})
	require.NoError(t, srv.Start(l))
	defer func() { _ = srv.Stop() }()

	var wg sync.WaitGroup

	for id := 1; id <= 10; id++ {
		wg.Go(func() {
			// client
			conn, err := modbus.NewConnection(t.Context(), l.Addr().String(), "", "", 0, modbus.Tcp, uint8(id))
			require.NoError(t, err)

			for range 50 {
				addr := uint16(rand.N(200) + 1)
				qty := uint16(rand.N(32) + 1)

				b, err := conn.ReadInputRegisters(addr, qty)
				require.NoError(t, err)

				if err == nil {
					for u := range qty {
						assert.Equal(t, addr^uint16(id)^u, binary.BigEndian.Uint16(b[2*u:]))
					}
				}

				time.Sleep(rand.N(time.Millisecond))
			}
		})
	}

	wg.Wait()
}

func TestReadCoils(t *testing.T) {
	// downstream server
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer l.Close()

	srv, _ := mbserver.New(&echoHandler{
		id:             0,
		RequestHandler: new(mbserver.DummyHandler),
	})
	require.NoError(t, srv.Start(l))
	defer func() { _ = srv.Stop() }()

	// proxy server
	pl, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer pl.Close()

	downstreamConn, err := modbus.NewConnection(t.Context(), l.Addr().String(), "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)

	proxy, _ := mbserver.New(newHandler(util.NewLogger("foo"), ReadOnlyFalse, downstreamConn))
	require.NoError(t, proxy.Start(pl))
	defer func() { _ = proxy.Stop() }()

	// test client
	{
		conn, err := modbus.NewConnection(t.Context(), pl.Addr().String(), "", "", 0, modbus.Tcp, 1)
		require.NoError(t, err)

		{ // read
			b, err := conn.ReadCoils(1, 1)
			require.NoError(t, err)
			assert.Equal(t, []byte{0x01}, b)

			b, err = conn.ReadCoils(1, 2)
			require.NoError(t, err)
			assert.Equal(t, []byte{0x03}, b)

			b, err = conn.ReadCoils(1, 9)
			require.NoError(t, err)
			assert.Equal(t, []byte{0xFF, 0x01}, b)
		}
		{ // write
			b, err := conn.WriteSingleCoil(1, 0xFF00)
			require.NoError(t, err)
			assert.Equal(t, []byte{0xFF, 0x00}, b)

			b, err = conn.WriteMultipleCoils(1, 9, []byte{0xFF, 0x01})
			require.NoError(t, err)
			assert.Equal(t, []byte{0x00, 0x09}, b)
		}
	}
}

// TestProxyCacheCoalescesConcurrentReads verifies that two clients reading
// the same register range genuinely concurrently are coalesced into a
// single downstream request, not just served from an already-warm cache.
func TestProxyCacheCoalescesConcurrentReads(t *testing.T) {
	downstream := &countingHandler{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	pl, _ := startTestProxy(t, downstream)

	client1, err := modbus.NewConnection(t.Context(), pl, "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)
	client2, err := modbus.NewConnection(t.Context(), pl, "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := client1.ReadHoldingRegisters(10, 2)
		assert.NoError(t, err) // require.FailNow is only valid on the test goroutine
	})

	<-downstream.entered // first read is in flight, holding the downstream response

	wg.Go(func() {
		_, err := client2.ReadHoldingRegisters(10, 2)
		assert.NoError(t, err)
	})

	// give the second read time to join the first read's flight before
	// releasing it - arriving late instead would still pass the assertion
	// below via a plain TTL hit, silently proving the wrong thing. No
	// synchronization signal exists to rule that out deterministically, so
	// this is a heuristic margin, not a guarantee.
	time.Sleep(100 * time.Millisecond)
	close(downstream.release)
	wg.Wait()

	assert.Equal(t, int32(1), downstream.holdingReads.Load(), "two concurrent reads for the same range must share one downstream request")
}

// TestProxyCacheInvalidatesOnWrite verifies a read right after a write is
// not served the pre-write cached value.
func TestProxyCacheInvalidatesOnWrite(t *testing.T) {
	downstream := &countingHandler{}
	pl, _ := startTestProxy(t, downstream)

	client, err := modbus.NewConnection(t.Context(), pl, "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)

	_, err = client.ReadHoldingRegisters(10, 2)
	require.NoError(t, err)
	_, err = client.ReadHoldingRegisters(10, 2)
	require.NoError(t, err)
	require.Equal(t, int32(1), downstream.holdingReads.Load(), "second read within TTL must be served from cache")

	_, err = client.WriteSingleRegister(10, 42)
	require.NoError(t, err)

	_, err = client.ReadHoldingRegisters(10, 2)
	require.NoError(t, err)
	assert.Equal(t, int32(2), downstream.holdingReads.Load(), "a read right after a write must not be served the pre-write cached value")
}

// TestProxyCacheServesSubsetOfLargerRead verifies a request that only
// overlaps a previously read range - not repeating it exactly - is still
// served from cache, end to end through the proxy.
func TestProxyCacheServesSubsetOfLargerRead(t *testing.T) {
	downstream := &countingHandler{}
	pl, _ := startTestProxy(t, downstream)

	client, err := modbus.NewConnection(t.Context(), pl, "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)

	_, err = client.ReadHoldingRegisters(10, 4)
	require.NoError(t, err)

	// different start address and quantity, but fully covered by the read above
	_, err = client.ReadHoldingRegisters(11, 2)
	require.NoError(t, err)

	assert.Equal(t, int32(1), downstream.holdingReads.Load(), "a range covered by an earlier, differently-shaped read must not trigger its own downstream request")
}

// TestHandlerRecordRead verifies recordRead counts every read and, of those,
// exactly the ones served from cache.
func TestHandlerRecordRead(t *testing.T) {
	h := newHandler(util.NewLogger("foo"), ReadOnlyFalse, nil)

	h.recordRead("k", false)
	h.recordRead("k", true)
	h.recordRead("k", true)

	assert.Equal(t, int64(3), h.reads.Load())
	assert.Equal(t, int64(2), h.hits.Load())
}

// TestHandlerReportStats verifies reportStats keeps ticking until its
// context is cancelled, and returns promptly afterwards.
func TestHandlerReportStats(t *testing.T) {
	h := newHandler(util.NewLogger("foo"), ReadOnlyFalse, nil)
	h.recordRead("k", false)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		h.reportStats(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond) // let at least one tick fire
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reportStats did not return after context cancellation")
	}
}

// startTestProxy wires downstream up behind a proxy handler and returns the
// proxy's listen address and the connection the proxy itself uses downstream.
func startTestProxy(t *testing.T, downstream mbserver.RequestHandler) (addr string, downstreamConn *modbus.Connection) {
	t.Helper()

	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })

	srv, _ := mbserver.New(downstream)
	require.NoError(t, srv.Start(l))
	t.Cleanup(func() { _ = srv.Stop() })

	pl, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	t.Cleanup(func() { pl.Close() })

	downstreamConn, err = modbus.NewConnection(t.Context(), l.Addr().String(), "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)

	proxy, _ := mbserver.New(newHandler(util.NewLogger("foo"), ReadOnlyFalse, downstreamConn))
	require.NoError(t, proxy.Start(pl))
	t.Cleanup(func() { _ = proxy.Stop() })

	return pl.Addr().String(), downstreamConn
}

// countingHandler counts downstream HandleHoldingRegisters calls, to verify
// the proxy's cache actually avoids the physical read it claims to. If
// release is set, the first read blocks until it's closed, signaling entry
// via entered - lets a test hold a read in flight while a second one joins.
type countingHandler struct {
	mbserver.DummyHandler
	holdingReads atomic.Int32
	entered      chan struct{}
	release      chan struct{}
	entryOnce    sync.Once
}

func (h *countingHandler) HandleHoldingRegisters(req *mbserver.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		return req.Args, nil
	}
	h.holdingReads.Add(1)
	if h.release != nil {
		h.entryOnce.Do(func() { close(h.entered) })
		<-h.release
	}
	return make([]uint16, req.Quantity), nil
}

type echoHandler struct {
	id int
	mbserver.RequestHandler
}

func (h *echoHandler) HandleInputRegisters(req *mbserver.InputRegistersRequest) (res []uint16, err error) {
	for u := uint16(0); u < req.Quantity; u++ {
		res = append(res, req.Addr^uint16(req.UnitId)^u)
	}

	return res, err
}

func (h *echoHandler) HandleCoils(req *mbserver.CoilsRequest) (res []bool, err error) {
	if req.IsWrite {
		return nil, nil
	}

	for u := uint16(0); u < req.Quantity; u++ {
		res = append(res, true)
	}

	return res, err
}
