package server

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a keyed token bucket.
type Limiter struct {
	mu      sync.Mutex
	m       map[string]*bucket
	lastGC  time.Time
	nowFunc func() time.Time
}

func NewLimiter() *Limiter { return &Limiter{m: map[string]*bucket{}, nowFunc: time.Now} }

// Allow takes one token from key's bucket (refill `rate`/s, capacity `burst`).
func (l *Limiter) Allow(key string, rate float64, burst int) bool {
	now := l.nowFunc()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) > time.Minute {
		for k, b := range l.m {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.m, k)
			}
		}
		l.lastGC = now
	}
	b := l.m[key]
	if b == nil {
		b = &bucket{tokens: float64(burst), last: now}
		l.m[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
