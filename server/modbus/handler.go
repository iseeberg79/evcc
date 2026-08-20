package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"sync/atomic"
	"time"

	"github.com/andig/mbserver"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	gridx "github.com/grid-x/modbus"
)

// cacheTTL dedups reads several downstream clients issue for the same
// register range within a short window. Short enough that clients polling
// on their own, unrelated cadences essentially never share a hit - only
// genuinely concurrent requests do, so every read still reaches the device
// in near-real-time; a coalesced read just skips the wait behind a request
// already in flight instead of queuing behind it.
//
// A coalesced caller shares the leader's error, not just its payload -
// intentional, since a failing device rarely recovers within the same
// window a second attempt would land in.
const cacheTTL = time.Second

type handler struct {
	log       *util.Logger
	readOnly  ReadOnlyMode
	conn      *modbus.Connection
	cache     *modbus.Cache         // coils/discrete inputs - low traffic, exact-match is enough
	registers *modbus.RegisterCache // holding/input registers - the actual read volume
	reads     atomic.Int64          // every read request, hit or miss
	hits      atomic.Int64          // subset of reads served from cache
	maxDur    atomic.Int64          // longest request since the last reportStats tick, in ns
}

// newHandler returns a handler with its caches always initialized - a
// struct literal built without going through this can leave them nil,
// panicking on the first read.
func newHandler(log *util.Logger, readOnly ReadOnlyMode, conn *modbus.Connection) *handler {
	return &handler{
		log:       log,
		readOnly:  readOnly,
		conn:      conn,
		cache:     modbus.NewCache(cacheTTL),
		registers: modbus.NewRegisterCache(cacheTTL),
	}
}

// statsInterval is how often reportStats logs a summary.
const statsInterval = time.Minute

// reportStats logs a periodic summary of read volume and cache hit rate,
// until ctx is done. Started separately from newHandler so building a
// handler for tests doesn't leak a goroutine per instance.
func (h *handler) reportStats(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastReads, lastHits int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reads, hits := h.reads.Load(), h.hits.Load()
			dReads, dHits := reads-lastReads, hits-lastHits
			lastReads, lastHits = reads, hits
			maxDur := time.Duration(h.maxDur.Swap(0))

			rate := float64(dReads) / interval.Seconds()
			var hitRate float64
			if dReads > 0 {
				hitRate = float64(dHits) / float64(dReads) * 100
			}
			h.log.DEBUG.Printf("proxy stats: %.1f req/s, %d/%d cache hits (%.0f%%), longest request %v", rate, dHits, dReads, hitRate, maxDur)
		}
	}
}

func bytesAsUint16(b []byte) []uint16 {
	u := make([]uint16, 0, len(b)/2)
	for i := range len(b) / 2 {
		u = append(u, binary.BigEndian.Uint16(b[2*i:]))
	}
	return u
}

func asBytes(u []uint16) []byte {
	b := make([]byte, 2*len(u))
	for i, u := range u {
		binary.BigEndian.PutUint16(b[2*i:], u)
	}
	return b
}

func (h *handler) logResult(op string, b []byte, err error) {
	if err == nil {
		h.log.TRACE.Printf(op+": % 0x", b)
	} else {
		h.log.TRACE.Printf(op+": %v", err)
	}
}

// recordRead counts a read for reportStats and, if it was spared its own
// physical device access, logs it - grep-countable against the plain read
// count to see the cache's actual hit rate.
func (h *handler) recordRead(key string, hit bool) {
	h.reads.Add(1)
	if hit {
		h.hits.Add(1)
		h.log.TRACE.Printf("cache hit: %s", key)
	}
}

// trackDuration returns a func to be deferred at the top of each Handle*
// method; it records how long the request took (queueing behind other
// requests on the shared downstream connection included) against maxDur,
// the longest seen since the last reportStats tick, and TRACE-logs the same
// duration against desc - reportStats's periodic max only ever names a
// number, not which request it was; this is what actually answers that once
// a spike shows up live (see PR discussion, a burst of unrelated requests
// queuing behind each other on the shared connection, not one slow
// register).
func (h *handler) trackDuration(desc string) func() {
	start := time.Now()
	return func() {
		d := time.Since(start)
		for {
			cur := h.maxDur.Load()
			if int64(d) <= cur || h.maxDur.CompareAndSwap(cur, int64(d)) {
				break
			}
		}
		h.log.TRACE.Printf("duration %s: %v", desc, d)
	}
}

func (h *handler) exceptionToUint16AndError(op string, b []byte, err error) ([]uint16, error) {
	h.logResult(op, b, err)

	if me, ok := errors.AsType[*gridx.Error](err); ok {
		err = mbserver.MapExceptionCodeToError(me.ExceptionCode)
	}

	return bytesAsUint16(b), err
}

func coilsToBytes(b []bool) []byte {
	l := len(b) / 8
	if len(b)%8 != 0 {
		l++
	}

	res := make([]byte, l)

	for i, bb := range b {
		if bb {
			byteNum := i / 8
			bit := i % 8

			res[byteNum] |= bits.RotateLeft8(1, bit)
		}
	}

	return res
}

func (h *handler) bytesToBoolResult(op string, qty uint16, b []byte, err error) ([]bool, error) {
	h.logResult(op, b, err)

	if me, ok := errors.AsType[*gridx.Error](err); ok {
		err = mbserver.MapExceptionCodeToError(me.ExceptionCode)
	}

	var res []bool

LOOP:
	for _, bb := range b {
		for bit := range 8 {
			if len(res) >= int(qty) {
				break LOOP
			}

			res = append(res, bits.RotateLeft8(bb, -bit)&1 != 0)
		}
	}

	return res, err
}

func (h *handler) HandleDiscreteInputs(req *mbserver.DiscreteInputsRequest) ([]bool, error) {
	defer h.trackDuration(fmt.Sprintf("read discrete: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity))()
	h.log.TRACE.Printf("read discrete: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/di/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.cache.Fetch(key, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadDiscreteInputs(req.Addr, req.Quantity)
	})
	h.recordRead(key, hit)
	return h.bytesToBoolResult("read discrete", req.Quantity, b, err)
}

func (h *handler) HandleCoils(req *mbserver.CoilsRequest) ([]bool, error) {
	defer h.trackDuration(fmt.Sprintf("coils: id %d addr %d qty %d write %t", req.UnitId, req.Addr, req.Quantity, req.IsWrite))()
	if req.IsWrite {
		switch h.readOnly {
		case ReadOnlyDeny:
			h.log.TRACE.Printf("deny: write coils: id %d addr %d qty %d val %v", req.UnitId, req.Addr, req.Quantity, req.Args)
			return nil, mbserver.ErrIllegalFunction
		case ReadOnlyTrue:
			h.log.TRACE.Printf("ignore: write coils: id %d addr %d qty %d val %v", req.UnitId, req.Addr, req.Quantity, req.Args)
			return req.Args, nil
		}

		if req.WriteFuncCode == gridx.FuncCodeWriteSingleCoil {
			h.log.TRACE.Printf("write coil: id %d addr %d val %t", req.UnitId, req.Addr, req.Args[0])
			var u uint16
			if req.Args[0] {
				u = 0xFF00
			}

			b, err := h.conn.Clone(req.UnitId).WriteSingleCoil(req.Addr, u)
			h.cache.Clear()
			h.registers.Clear() // a coil can gate what a register reports too
			return h.bytesToBoolResult("write coil", req.Quantity, b, err)
		}

		h.log.TRACE.Printf("write coils: id %d addr %d qty %d val %v", req.UnitId, req.Addr, req.Quantity, req.Args)
		args := coilsToBytes(req.Args)
		b, err := h.conn.Clone(req.UnitId).WriteMultipleCoils(req.Addr, req.Quantity, args)
		h.cache.Clear()
		h.registers.Clear()
		return h.bytesToBoolResult("write coils", req.Quantity, b, err)
	}

	h.log.TRACE.Printf("read coils: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/coil/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.cache.Fetch(key, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadCoils(req.Addr, req.Quantity)
	})
	h.recordRead(key, hit)
	return h.bytesToBoolResult("read coils", req.Quantity, b, err)
}

func (h *handler) HandleInputRegisters(req *mbserver.InputRegistersRequest) ([]uint16, error) {
	defer h.trackDuration(fmt.Sprintf("read input: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity))()
	h.log.TRACE.Printf("read input: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/ir/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.registers.Fetch(req.UnitId, gridx.FuncCodeReadInputRegisters, req.Addr, req.Quantity, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadInputRegisters(req.Addr, req.Quantity)
	})
	h.recordRead(key, hit)
	return h.exceptionToUint16AndError("read input", b, err)
}

func (h *handler) HandleHoldingRegisters(req *mbserver.HoldingRegistersRequest) ([]uint16, error) {
	defer h.trackDuration(fmt.Sprintf("holding registers: id %d addr %d qty %d write %t", req.UnitId, req.Addr, req.Quantity, req.IsWrite))()
	if req.IsWrite {
		switch h.readOnly {
		case ReadOnlyDeny:
			h.log.TRACE.Printf("deny: write holdings: id %d addr %d qty %d val %0x", req.UnitId, req.Addr, req.Quantity, asBytes(req.Args))
			return nil, mbserver.ErrIllegalFunction
		case ReadOnlyTrue:
			h.log.TRACE.Printf("ignore: write holdings: id %d addr %d qty %d val %0x", req.UnitId, req.Addr, req.Quantity, asBytes(req.Args))
			return req.Args, nil
		}

		if req.WriteFuncCode == gridx.FuncCodeWriteSingleRegister {
			h.log.TRACE.Printf("write holding: id %d addr %d val %04x", req.UnitId, req.Addr, req.Args[0])
			b, err := h.conn.Clone(req.UnitId).WriteSingleRegister(req.Addr, req.Args[0])
			h.registers.Invalidate(req.UnitId, gridx.FuncCodeReadHoldingRegisters, req.Addr, 1)
			h.cache.Clear() // a register write can gate what a coil reports too
			return h.exceptionToUint16AndError("write holding", b, err)
		}

		h.log.TRACE.Printf("write holdings: id %d addr %d qty %d val %0x", req.UnitId, req.Addr, req.Quantity, asBytes(req.Args))
		b, err := h.conn.Clone(req.UnitId).WriteMultipleRegisters(req.Addr, req.Quantity, asBytes(req.Args))
		h.registers.Invalidate(req.UnitId, gridx.FuncCodeReadHoldingRegisters, req.Addr, req.Quantity)
		h.cache.Clear()
		return h.exceptionToUint16AndError("write multiple holding", b, err)
	}

	h.log.TRACE.Printf("read holdings: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/hr/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.registers.Fetch(req.UnitId, gridx.FuncCodeReadHoldingRegisters, req.Addr, req.Quantity, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadHoldingRegisters(req.Addr, req.Quantity)
	})
	h.recordRead(key, hit)
	return h.exceptionToUint16AndError("read holding", b, err)
}
