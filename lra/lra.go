package lra

import (
	"container/list"
	"sync"
	"time"

	"github.com/rosenlo/toolkits/promutil"
)

var (
	lraUsedBytes, _  = promutil.NewGaugeVec("lra_used_bytes", "memory usage of lra", []string{})
	lraTotalBytes, _ = promutil.NewGaugeVec("lra_total_bytes", "total memory of lra", []string{})
	EmptyValue       = emptyValue(0)
)

type Value interface {
	Len() int
}

type emptyValue int

func (e emptyValue) Len() int {
	return int(e)
}

// DefaultExpiration is the fallback stale-access threshold when a caller does
// not pin one explicitly. It preserves the package's historical one-minute
// behavior for the unconfigured New path.
const DefaultExpiration = time.Minute

type entry struct {
	key            string
	value          Value
	lastAccessTIme time.Time
}

type Cache struct {
	maxBytes   int64
	curBytes   int64
	ll         *list.List
	cache      map[string]*list.Element
	lock       sync.RWMutex
	OnEvicted  func(key string, value Value)
	expiration time.Duration
}

// New builds a cache with the historical one-minute stale-access threshold. It
// is kept for backward compatibility; callers that need a different threshold
// should use NewWithExpiration.
func New(maxBytes int64, onEvicted func(string, Value)) *Cache {
	return NewWithExpiration(maxBytes, DefaultExpiration, onEvicted)
}

// NewWithExpiration builds a cache whose entries are evicted once they have
// not been accessed for longer than expiration. The eviction scan still runs on
// a one-minute ticker, so shorter expirations are honored with at most that
// much extra retention; longer expirations are honored exactly.
func NewWithExpiration(maxBytes int64, expiration time.Duration, onEvicted func(string, Value)) *Cache {
	if expiration <= 0 {
		expiration = DefaultExpiration
	}
	c := &Cache{
		maxBytes:   maxBytes,
		ll:         list.New(),
		cache:      make(map[string]*list.Element),
		OnEvicted:  onEvicted,
		expiration: expiration,
	}
	go c.startEvictionTimer()
	return c
}

func (c *Cache) Get(key string) (value Value, ok bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if element, ok := c.cache[key]; ok {
		c.ll.MoveToFront(element)
		entry := element.Value.(*entry)
		entry.lastAccessTIme = time.Now()
		return entry.value, true
	}
	return
}

func (c *Cache) RemoveStaleEntries() {
	c.lock.Lock()
	defer c.lock.Unlock()
	for {
		element := c.ll.Back()
		if element == nil {
			return
		}
		entry := element.Value.(*entry)
		if time.Since(entry.lastAccessTIme) > c.expiration {
			c.removeElement(element)
			continue
		}
		break
	}
}

func (c *Cache) Add(key string, value Value) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if element, ok := c.cache[key]; ok {
		c.ll.MoveToFront(element)
		entry := element.Value.(*entry)
		c.curBytes += int64(value.Len()) - int64(entry.value.Len())
		entry.value = value
		entry.lastAccessTIme = time.Now()
		return
	}
	element := c.ll.PushFront(&entry{key, value, time.Now()})
	c.cache[key] = element
	c.curBytes += int64(len(key)) + int64(value.Len())
	for c.maxBytes != 0 && c.curBytes > c.maxBytes {
		c.removeOldest()
	}
}

func (c *Cache) removeOldest() {
	element := c.ll.Back()
	if element != nil {
		c.removeElement(element)
	}
}

func (c *Cache) removeElement(e *list.Element) {
	c.ll.Remove(e)
	entry := e.Value.(*entry)
	delete(c.cache, entry.key)
	c.curBytes -= int64(len(entry.key)) + int64(entry.value.Len())

	if c.OnEvicted != nil {
		c.OnEvicted(entry.key, entry.value)
	}
}

func (c *Cache) startEvictionTimer() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		c.RemoveStaleEntries()
		lraUsedBytes.WithLabelValues().Set(float64(c.curBytes))
		lraTotalBytes.WithLabelValues().Set(float64(c.maxBytes))
	}
}
