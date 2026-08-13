package notification

import (
	"sync"
	"time"
)

type Policy struct {
	Window time.Duration
	mu     sync.Mutex
	last   map[string]time.Time
}

func NewPolicy(window time.Duration) *Policy {
	if window <= 0 {
		window = 30 * time.Second
	}
	return &Policy{Window: window, last: make(map[string]time.Time)}
}

func (p *Policy) Allow(value SessionNotification) bool {
	if value.Attention == AttentionNone {
		return false
	}
	key := value.SessionID + ":" + string(value.Attention) + ":" + value.State
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if previous, ok := p.last[key]; ok && now.Sub(previous) < p.Window {
		return false
	}
	p.last[key] = now
	return true
}
