package notification

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var ErrSuppressed = errors.New("notification suppressed by policy")

type Dispatcher struct {
	Policy   *Policy
	Notifier *WebhookNotifier

	mu sync.RWMutex
}

func NewDispatcher(targets []Target, window time.Duration, client *http.Client) *Dispatcher {
	return &Dispatcher{Policy: NewPolicy(window), Notifier: &WebhookNotifier{Targets: append([]Target(nil), targets...), Client: client}}
}

func (d *Dispatcher) SetTargets(targets []Target) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Notifier == nil {
		d.Notifier = &WebhookNotifier{}
	}
	d.Notifier.Targets = append([]Target(nil), targets...)
}

func (d *Dispatcher) NotifySessionEvent(ctx context.Context, value SessionNotification) error {
	if d == nil || d.Policy == nil {
		return nil
	}
	if !d.Policy.Allow(value) {
		return ErrSuppressed
	}
	d.mu.RLock()
	notifier := d.Notifier
	d.mu.RUnlock()
	if notifier == nil {
		return nil
	}
	return notifier.NotifySessionEvent(ctx, value)
}
