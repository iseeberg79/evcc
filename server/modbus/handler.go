package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
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
	log      *util.Logger
	readOnly ReadOnlyMode
	conn     *modbus.Connection
	cache    *modbus.Cache
}

// newHandler returns a handler with its cache always initialized - a struct
// literal built without going through this can leave cache nil, panicking
// on the first read.
func newHandler(log *util.Logger, readOnly ReadOnlyMode, conn *modbus.Connection) *handler {
	return &handler{
		log:      log,
		readOnly: readOnly,
		conn:     conn,
		cache:    modbus.NewCache(cacheTTL),
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

// logCacheHit notes a read spared its own physical device access - grep-
// countable against the plain read count to see the cache's actual hit rate.
func (h *handler) logCacheHit(key string, hit bool) {
	if hit {
		h.log.TRACE.Printf("cache hit: %s", key)
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
	h.log.TRACE.Printf("read discrete: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/di/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.cache.Fetch(key, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadDiscreteInputs(req.Addr, req.Quantity)
	})
	h.logCacheHit(key, hit)
	return h.bytesToBoolResult("read discrete", req.Quantity, b, err)
}

func (h *handler) HandleCoils(req *mbserver.CoilsRequest) ([]bool, error) {
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
			return h.bytesToBoolResult("write coil", req.Quantity, b, err)
		}

		h.log.TRACE.Printf("write coils: id %d addr %d qty %d val %v", req.UnitId, req.Addr, req.Quantity, req.Args)
		args := coilsToBytes(req.Args)
		b, err := h.conn.Clone(req.UnitId).WriteMultipleCoils(req.Addr, req.Quantity, args)
		h.cache.Clear()
		return h.bytesToBoolResult("write coils", req.Quantity, b, err)
	}

	h.log.TRACE.Printf("read coils: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/coil/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.cache.Fetch(key, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadCoils(req.Addr, req.Quantity)
	})
	h.logCacheHit(key, hit)
	return h.bytesToBoolResult("read coils", req.Quantity, b, err)
}

func (h *handler) HandleInputRegisters(req *mbserver.InputRegistersRequest) ([]uint16, error) {
	h.log.TRACE.Printf("read input: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/ir/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.cache.Fetch(key, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadInputRegisters(req.Addr, req.Quantity)
	})
	h.logCacheHit(key, hit)
	return h.exceptionToUint16AndError("read input", b, err)
}

func (h *handler) HandleHoldingRegisters(req *mbserver.HoldingRegistersRequest) ([]uint16, error) {
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
			h.cache.Clear()
			return h.exceptionToUint16AndError("write holding", b, err)
		}

		h.log.TRACE.Printf("write holdings: id %d addr %d qty %d val %0x", req.UnitId, req.Addr, req.Quantity, asBytes(req.Args))
		b, err := h.conn.Clone(req.UnitId).WriteMultipleRegisters(req.Addr, req.Quantity, asBytes(req.Args))
		h.cache.Clear()
		return h.exceptionToUint16AndError("write multiple holding", b, err)
	}

	h.log.TRACE.Printf("read holdings: id %d addr %d qty %d", req.UnitId, req.Addr, req.Quantity)
	key := fmt.Sprintf("%d/hr/%d/%d", req.UnitId, req.Addr, req.Quantity)
	b, hit, err := h.cache.Fetch(key, func() ([]byte, error) {
		return h.conn.Clone(req.UnitId).ReadHoldingRegisters(req.Addr, req.Quantity)
	})
	h.logCacheHit(key, hit)
	return h.exceptionToUint16AndError("read holding", b, err)
}
