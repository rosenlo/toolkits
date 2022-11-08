package safemap

import (
	"log"
	"runtime/debug"
	"sync"
	"time"
)

const (
	DefaultCleanDuration = time.Hour
)

type KeyData struct {
	Name         string
	Value        interface{}
	CreateAt     time.Time
	LastAccessAt time.Time
	Expiration   time.Duration
}

type Options struct {
	CleanDuration time.Duration
}

type Map struct {
	sync.RWMutex
	m    map[string]*KeyData
	opts Options
}

func New(opts Options) *Map {
	m := &Map{
		m:    make(map[string]*KeyData),
		opts: opts,
	}
	go m.cleanStaleKey()

	return m
}

func (s *Map) Set(key string, value interface{}, expiration time.Duration) {
	now := time.Now()
	keyData := &KeyData{
		Name:         key,
		Value:        value,
		CreateAt:     now,
		LastAccessAt: now,
		Expiration:   expiration,
	}

	s.Lock()

	s.m[key] = keyData

	s.Unlock()
}

func (s *Map) Get(key string) (interface{}, bool) {
	s.RLock()
	defer s.RUnlock()

	keyData, exists := s.m[key]
	if !exists {
		return nil, exists
	}

	return keyData.Value, exists
}

func (s *Map) Remove(key string) {
	s.Lock()

	delete(s.m, key)

	s.Unlock()
}

func (s *Map) cleanStaleKey() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("recover: %v", r)
			debug.PrintStack()
		}
	}()

	if s.opts.CleanDuration == 0 {
		return
	}

	ticker := time.NewTicker(s.opts.CleanDuration)

	for {
		deleteKeys := make([]string, 0)

		now := time.Now()

		s.RLock()
		for key, keyData := range s.m {
			if now.UnixNano()-keyData.LastAccessAt.UnixNano() > keyData.Expiration.Nanoseconds() {
				deleteKeys = append(deleteKeys, key)
			}
		}
		s.RUnlock()

		for i := range deleteKeys {
			s.Remove(deleteKeys[i])
		}

		select {
		case <-ticker.C:
			continue
		}

	}
}
