package lra

import (
	"sync"
	"testing"
	"time"
)

func TestNewPreservesDefaultExpiration(t *testing.T) {
	c := New(0, nil)
	if c.expiration != DefaultExpiration {
		t.Fatalf("New expiration = %v, want %v", c.expiration, DefaultExpiration)
	}
}

func TestNewWithExpirationDefaultsOnNonPositive(t *testing.T) {
	for _, expiration := range []time.Duration{0, -time.Second} {
		c := NewWithExpiration(0, expiration, nil)
		if c.expiration != DefaultExpiration {
			t.Fatalf("NewWithExpiration(%v) expiration = %v, want %v", expiration, c.expiration, DefaultExpiration)
		}
	}
}

func TestRemoveStaleEntriesHonorsCustomExpiration(t *testing.T) {
	var (
		mu      sync.Mutex
		evicted []string
	)
	onEvicted := func(key string, _ Value) {
		mu.Lock()
		defer mu.Unlock()
		evicted = append(evicted, key)
	}

	c := NewWithExpiration(0, 50*time.Millisecond, onEvicted)
	c.Add("a", EmptyValue)

	// A freshly added entry must survive an immediate sweep.
	c.RemoveStaleEntries()
	mu.Lock()
	n := len(evicted)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("fresh entry evicted: %v", evicted)
	}

	// After the expiration window the sweep must evict it exactly once.
	time.Sleep(80 * time.Millisecond)
	c.RemoveStaleEntries()
	mu.Lock()
	evictedCopy := append([]string(nil), evicted...)
	mu.Unlock()
	if len(evictedCopy) != 1 || evictedCopy[0] != "a" {
		t.Fatalf("evicted = %v, want [a]", evictedCopy)
	}
}
