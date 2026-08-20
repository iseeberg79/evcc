package plugin

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	gridx "github.com/grid-x/modbus"
)

// sunspecCachedClient dedups the holding register reads gosunspec issues on
// behalf of all sunspec values sharing a model block within one poll cycle.
type sunspecCachedClient struct {
	gridx.Client
	cache *modbus.Cache
}

// newSunspecCachedClient wraps client for use as gosunspec's modbus client.
func newSunspecCachedClient(client gridx.Client) *sunspecCachedClient {
	return &sunspecCachedClient{Client: client, cache: modbus.NewCache(modbusBlockTTL)}
}

// TEMPORARY: hit-rate logging, to compare against the superset-serving
// variant evaluated separately - remove once compared.
var (
	sunspecCacheLog   = util.NewLogger("sunspec-cache")
	sunspecCacheReads atomic.Int64
	sunspecCacheHits  atomic.Int64
	sunspecCacheOnce  sync.Once
)

// reportSunspecCacheStats logs the aggregate read rate and cache hit rate
// across all sunspecCachedClient instances every interval. TEMPORARY, see
// sunspecCacheLog above.
func reportSunspecCacheStats(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastReads, lastHits int64
	for range ticker.C {
		reads, hits := sunspecCacheReads.Load(), sunspecCacheHits.Load()
		dReads, dHits := reads-lastReads, hits-lastHits
		lastReads, lastHits = reads, hits

		rate := float64(dReads) / interval.Seconds()
		var hitRate float64
		if dReads > 0 {
			hitRate = float64(dHits) / float64(dReads) * 100
		}
		sunspecCacheLog.DEBUG.Printf("sunspec cache stats: %.1f req/s, %d/%d cache hits (%.0f%%)", rate, dHits, dReads, hitRate)
	}
}

func (c *sunspecCachedClient) ReadHoldingRegisters(address, quantity uint16) ([]byte, error) {
	sunspecCacheOnce.Do(func() { go reportSunspecCacheStats(time.Minute) }) // TEMPORARY

	key := fmt.Sprintf("%d/%d", address, quantity)

	b, hit, err := c.cache.Fetch(key, func() ([]byte, error) {
		return c.Client.ReadHoldingRegisters(address, quantity)
	})
	if err != nil {
		return nil, err
	}

	// TEMPORARY
	sunspecCacheReads.Add(1)
	if hit {
		sunspecCacheHits.Add(1)
	}

	// callers unmarshal in place, hand out a copy of the shared payload
	return bytes.Clone(b), nil
}

func (c *sunspecCachedClient) WriteSingleRegister(address, value uint16) ([]byte, error) {
	defer c.cache.Clear()
	return c.Client.WriteSingleRegister(address, value)
}

func (c *sunspecCachedClient) WriteMultipleRegisters(address, quantity uint16, value []byte) ([]byte, error) {
	defer c.cache.Clear()
	return c.Client.WriteMultipleRegisters(address, quantity, value)
}
