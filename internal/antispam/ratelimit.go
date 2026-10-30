package antispam

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter keeps per-key token buckets in memory. Keys are "bucket:subject"
// strings such as "report:user:42" or "login:ip:1.2.3.4".
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*entry
	stop    chan struct{}
}

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewLimiter creates a limiter and starts a background sweeper.
func NewLimiter() *Limiter {
	l := &Limiter{entries: map[string]*entry{}, stop: make(chan struct{})}
	go l.sweep()
	return l
}

// Close stops the sweeper.
func (l *Limiter) Close() { close(l.stop) }

func (l *Limiter) sweep() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			l.mu.Lock()
			for k, e := range l.entries {
				if now.Sub(e.seen) > time.Hour {
					delete(l.entries, k)
				}
			}
			l.mu.Unlock()
		}
	}
}

// Allow consumes one token from the bucket identified by key, which refills
// at `perPeriod` events per `period` and holds at most `perPeriod` tokens.
func (l *Limiter) Allow(key string, perPeriod int, period time.Duration) bool {
	if perPeriod <= 0 {
		return true
	}
	l.mu.Lock()
	e, ok := l.entries[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(rate.Every(period/time.Duration(perPeriod)), perPeriod)}
		l.entries[key] = e
	}
	e.seen = time.Now()
	l.mu.Unlock()
	return e.lim.Allow()
}
