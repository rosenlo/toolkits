package lra

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// blockingEvictor parks the *first* eviction inside removeElement, which runs
// with the shard's write lock held. That is how a test holds one shard's lock
// without reaching into Cache's internals.
type blockingEvictor struct {
	entered chan struct{}
	release chan struct{}
	parked  atomic.Bool
}

func newBlockingEvictor() *blockingEvictor {
	return &blockingEvictor{entered: make(chan struct{}), release: make(chan struct{})}
}

// Only the first eviction parks; later ones return at once. sync.Once would not
// do: Do() makes concurrent callers wait for the in-flight function, so the
// second Add would block on the Once rather than on the lock under test.
func (b *blockingEvictor) onEvicted(string, Value) {
	if b.parked.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
}

func keysInDifferentShards(shardNum uint64) (string, string) {
	first := "shard-probe-0"
	for i := 1; ; i++ {
		other := fmt.Sprintf("shard-probe-%d", i)
		if fnv1a(first)%shardNum != fnv1a(other)%shardNum {
			return first, other
		}
	}
}

// TestShardedCacheLocksAreIndependent is the point of ShardedCache: an Add must
// not wait on a lock held for an unrelated key. The control sub-test runs the
// same fixture with a single shard and asserts it *does* block -- without that,
// the test would pass against the unsharded Cache it is meant to improve on.
func TestShardedCacheLocksAreIndependent(t *testing.T) {
	const shardNum = 8
	busyKey, otherKey := keysInDifferentShards(shardNum)

	for _, tc := range []struct {
		name        string
		shardNum    int
		expectBlock bool
	}{
		{name: "sharded", shardNum: shardNum, expectBlock: false},
		{name: "control/single lock", shardNum: 1, expectBlock: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evictor := newBlockingEvictor()
			// 1 byte per shard: the first Add overflows and evicts itself, so
			// the callback fires synchronously with the lock held.
			cache := NewSharded(tc.shardNum, int64(tc.shardNum), evictor.onEvicted)
			defer close(evictor.release)

			go cache.Add(busyKey, EmptyValue)
			<-evictor.entered

			done := make(chan struct{})
			go func() {
				cache.Add(otherKey, EmptyValue)
				close(done)
			}()

			if tc.expectBlock {
				select {
				case <-done:
					t.Fatal("control Add completed while the only lock was held: the fixture no longer holds a lock, so the sharded case proves nothing")
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Add blocked on another shard's lock")
			}
		})
	}
}

func TestShardedCacheRoutesEachKeyToOneShard(t *testing.T) {
	const shardNum = 16
	cache := NewSharded(shardNum, 0, nil)

	occupied := make(map[uint64]struct{})
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("route-%d", i)
		cache.Add(key, EmptyValue)

		want := fnv1a(key) % shardNum
		occupied[want] = struct{}{}
		for s := range cache.shards {
			if _, ok := cache.shards[s].Get(key); ok != (uint64(s) == want) {
				t.Fatalf("key %q: shard %d has it = %v, want %v", key, s, ok, uint64(s) == want)
			}
		}
	}
	if len(occupied) != shardNum {
		t.Errorf("1000 keys reached only %d of %d shards", len(occupied), shardNum)
	}
}

// TestShardedCacheGaugesReportTheWholeCache guards the reason ShardedCache owns
// its own timer: one timer per shard would leave lra_used_bytes holding
// whichever shard wrote last instead of the total.
func TestShardedCacheGaugesReportTheWholeCache(t *testing.T) {
	const (
		shardNum = 4
		maxBytes = 1 << 20
	)
	cache := NewSharded(shardNum, maxBytes, nil)

	var want int64
	for i := 0; i < 64; i++ {
		key := fmt.Sprintf("gauge-%d", i)
		cache.Add(key, emptyValue(8))
		want += int64(len(key)) + 8
	}
	if got := cache.UsedBytes(); got != want {
		t.Fatalf("UsedBytes() = %d, want %d", got, want)
	}

	cache.updateGauges()
	if got := gaugeValue(t, lraUsedBytes.WithLabelValues()); got != float64(want) {
		t.Errorf("lra_used_bytes = %v, want %v", got, float64(want))
	}
	if got := gaugeValue(t, lraTotalBytes.WithLabelValues()); got != float64(maxBytes) {
		t.Errorf("lra_total_bytes = %v, want %v -- the shards' budgets must add up to the caller's maxBytes", got, float64(maxBytes))
	}
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func TestNewShardedClampsShardNumAndKeepsBounded(t *testing.T) {
	if got := NewSharded(0, 0, nil).shardNum; got != 1 {
		t.Errorf("shardNum for NewSharded(0, ...) = %d, want 1", got)
	}
	// A budget smaller than the shard count must stay bounded on every shard;
	// rounding down to 0 would silently mean "unbounded" in Cache.Add.
	cache := NewSharded(8, 4, nil)
	for _, shard := range cache.shards {
		if shard.MaxBytes() == 0 {
			t.Fatal("shard budget rounded down to 0, which Cache treats as unbounded")
		}
	}
}

func BenchmarkShardedCacheAddParallel(b *testing.B) {
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-%032d", i)
	}

	for _, shardNum := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("shards=%d", shardNum), func(b *testing.B) {
			cache := NewSharded(shardNum, 1<<30, nil)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					cache.Add(keys[i&(len(keys)-1)], EmptyValue)
					i++
				}
			})
		})
	}
}
