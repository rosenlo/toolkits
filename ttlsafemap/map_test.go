package ttlsafemap

import (
	"testing"
	"time"
)

func cacheLen(c *Cache) int {
	n := 0
	c.syncMap.Range(func(_, _ any) bool { n++; return true })
	return n
}

// Expired keys that are never read again must go, whatever live keys sit
// around them in Range order.
func TestDeleteExpiredPassesLiveItems(t *testing.T) {
	c := NewCache()
	for i := 0; i < 1000; i++ {
		c.Set(i, nil, time.Hour)
	}
	for i := 1000; i < 2000; i++ {
		c.Set(i, nil, -time.Second)
	}

	c.deleteExpired()

	if n := cacheLen(&c); n != 1000 {
		t.Fatalf("items after cleanup = %d, want the 1000 live ones", n)
	}
	for i := 0; i < 1000; i++ {
		if _, ok := c.Get(i); !ok {
			t.Fatalf("live key %d was deleted", i)
		}
	}
}
