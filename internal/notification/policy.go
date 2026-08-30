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
	if value.ID != "" {
		// A task can complete many times in one long-lived session. When the
		// producer supplies the normalized event id, dedupe the same event
		// delivered through live and history paths without suppressing the next
		// task merely because its state is also "waiting".
		key = value.SessionID + ":" + string(value.Attention) + ":event:" + value.ID
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if previous, ok := p.last[key]; ok && now.Sub(previous) < p.Window {
		return false
	}
	p.last[key] = now
	return true
}
