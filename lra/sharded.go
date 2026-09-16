package lra

import (
	"time"
)

// ShardedCache spreads keys over several Cache instances, keyed by
// fnv1a(key) % shards, so concurrent callers do not serialise on one mutex.
//
// Cache.Add always takes the *write* lock -- a hit still moves the entry to the
// front of the LRU list -- so a single Cache caps a hot writer at roughly one
// core's worth of throughput no matter how many goroutines call it.
//
// The lra_used_bytes / lra_total_bytes gauges keep their whole-cache meaning:
// ShardedCache runs one eviction timer that sums every shard, rather than
// letting each shard's timer overwrite the pair with its own slice.
type ShardedCache struct {
	shards   []*Cache
	shardNum uint64
}

// NewSharded builds a ShardedCache with the historical one-minute stale-access
// threshold. maxBytes is the budget for the cache as a whole; it is split
// evenly across the shards. maxBytes == 0 means unbounded, as it does for Cache.
func NewSharded(shardNum int, maxBytes int64, onEvicted func(string, Value)) *ShardedCache {
	return NewShardedWithExpiration(shardNum, maxBytes, DefaultExpiration, onEvicted)
}

// NewShardedWithExpiration is NewSharded with an explicit stale-access
// threshold, mirroring NewWithExpiration.
func NewShardedWithExpiration(shardNum int, maxBytes int64, expiration time.Duration, onEvicted func(string, Value)) *ShardedCache {
	if shardNum < 1 {
		shardNum = 1
	}
	s := &ShardedCache{
		shards:   make([]*Cache, shardNum),
		shardNum: uint64(shardNum),
	}
	perShard := maxBytes / int64(shardNum)
	if maxBytes != 0 && perShard == 0 {
		// Never turn a bounded cache into an unbounded one by rounding down.
		perShard = 1
	}
	for i := range s.shards {
		s.shards[i] = newCache(perShard, expiration, onEvicted)
	}
	go s.startEvictionTimer()
	return s
}

func (s *ShardedCache) shard(key string) *Cache {
	return s.shards[fnv1a(key)%s.shardNum]
}

func (s *ShardedCache) Add(key string, value Value) {
	s.shard(key).Add(key, value)
}

func (s *ShardedCache) Get(key string) (Value, bool) {
	return s.shard(key).Get(key)
}

func (s *ShardedCache) RemoveStaleEntries() {
	for _, shard := range s.shards {
		shard.RemoveStaleEntries()
	}
}

// UsedBytes reports the whole cache's usage, summed across shards. It is a
// snapshot: shards are read one at a time, not under a single lock.
func (s *ShardedCache) UsedBytes() int64 {
	var total int64
	for _, shard := range s.shards {
		total += shard.UsedBytes()
	}
	return total
}

// MaxBytes reports the whole cache's budget. It can differ from the maxBytes
// passed to the constructor by up to shardNum-1 bytes, because each shard gets
// an integer share.
func (s *ShardedCache) MaxBytes() int64 {
	var total int64
	for _, shard := range s.shards {
		total += shard.MaxBytes()
	}
	return total
}

func (s *ShardedCache) startEvictionTimer() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.RemoveStaleEntries()
		s.updateGauges()
	}
}

func (s *ShardedCache) updateGauges() {
	lraUsedBytes.WithLabelValues().Set(float64(s.UsedBytes()))
	lraTotalBytes.WithLabelValues().Set(float64(s.MaxBytes()))
}

// fnv1a is FNV-1a over the key, inlined so sharding stays allocation-free.
func fnv1a(key string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	return h
}
