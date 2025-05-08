package ttlsafemap

import (
	"sync"
	"time"
)

type Item struct {
	Value      any
	Expiration int64
}

type Cache struct {
	syncMap sync.Map
}

func NewCache() Cache {
	return Cache{}
}

func (c *Cache) Set(key any, value any, ttl time.Duration) {
	expiration := time.Now().Add(ttl).UnixNano()
	item := Item{Value: value, Expiration: expiration}
	c.syncMap.Store(key, item)
}

func (c *Cache) Get(key any) (any, bool) {
	value, ok := c.syncMap.Load(key)
	if !ok {
		return nil, false
	}

	item := value.(Item)
	if time.Now().UnixNano() > item.Expiration {
		c.Delete(key)
		return nil, false
	}

	return item.Value, ok
}

func (c *Cache) Delete(key any) {
	c.syncMap.Delete(key)
}

func (c *Cache) Keys() []any {
	keys := make([]any, 0)
	c.syncMap.Range(func(key, value any) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

func (c *Cache) StartCleanupTimer(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			c.syncMap.Range(func(key, value any) bool {
				item := value.(Item)
				if time.Now().UnixNano() > item.Expiration {
					c.Delete(key)
					return true
				}
				return false
			})
		}
	}()
}
