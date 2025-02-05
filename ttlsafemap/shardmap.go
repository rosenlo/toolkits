package ttlsafemap

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rosenlo/toolkits/promutil"
)

var (
	cacheTotal   *prometheus.GaugeVec
	cacheExpired *prometheus.GaugeVec
	cacheUsage   *prometheus.HistogramVec
)

func init() {
	cacheTotal, _ = promutil.NewGaugeVec("ttlsafemap_cache_total", "", []string{"shard_id"})
	cacheExpired, _ = promutil.NewGaugeVec("ttlsafemap_cache_expired", "", []string{"shard_id"})
	cacheUsage, _ = promutil.NewHistogramVec("ttlsafemap_cache_usage", "", nil, []string{"shard_id", "method"})
}

type Shard struct {
	sync.RWMutex
	id string
	m  map[uint64]Item
}

type ShardMap struct {
	shardNum uint64
	shards   []Shard
}

func NewShardMap(shardNum int) *ShardMap {
	sm := &ShardMap{shardNum: uint64(shardNum), shards: make([]Shard, shardNum)}
	for i := 0; i < shardNum; i++ {
		sm.shards[i] = Shard{
			id: strconv.Itoa(i),
			m:  make(map[uint64]Item),
		}
	}
	return sm
}

func (sm *ShardMap) getShard(key uint64) *Shard {
	return &sm.shards[key%sm.shardNum]
}

func (sm *ShardMap) Set(key uint64, value any, ttl time.Duration) {
	shard := sm.getShard(key)
	shard.Set(key, value, ttl)
}

func (sm *ShardMap) Get(key uint64) (any, bool) {
	shard := sm.getShard(key)
	return shard.Get(key)
}

func (sm *ShardMap) Delete(key uint64) {
	shard := sm.getShard(key)
	shard.Delete(key)
}

func (sm *ShardMap) StartCleanupTimer(interval time.Duration) {
	for i := range sm.shards {
		sm.shards[i].StartCleanupTimer(interval)
	}
}

func (c *Shard) Set(key uint64, value any, ttl time.Duration) {
	start := time.Now()
	item := Item{Value: value, Expiration: time.Now().Add(ttl).UnixNano()}

	c.Lock()
	c.m[key] = item
	c.Unlock()

	cacheUsage.WithLabelValues(c.id, "set").Observe(time.Since(start).Seconds())
}

func (c *Shard) Get(key uint64) (any, bool) {
	start := time.Now()

	c.RLock()
	item, ok := c.m[key]
	c.RUnlock()

	cacheUsage.WithLabelValues(c.id, "get").Observe(time.Since(start).Seconds())

	if !ok {
		return nil, false
	}

	return item.Value, ok
}

func (c *Shard) Delete(key uint64) {
	start := time.Now()

	c.Lock()
	delete(c.m, key)
	c.Unlock()

	cacheUsage.WithLabelValues(c.id, "delete").Observe(time.Since(start).Seconds())
}

func (c *Shard) StartCleanupTimer(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			total := 0
			expired := 0
			deleteKeys := make([]uint64, 0)

			c.RLock()
			for key, item := range c.m {
				total++
				if time.Now().UnixNano() > item.Expiration {
					expired++
					deleteKeys = append(deleteKeys, key)
				}
			}
			c.RUnlock()

			for i := range deleteKeys {
				c.Delete(deleteKeys[i])
			}

			cacheTotal.WithLabelValues(c.id).Set(float64(total))
			cacheExpired.WithLabelValues(c.id).Set(float64(expired))
		}
	}()
}
