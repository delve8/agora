package daemon

import (
	"sync"

	"github.com/delve8/agora/internal/protocol"
)

type outbox struct {
	mu    sync.Mutex
	limit int
	items []protocol.Envelope
	gap   bool
}

func newOutbox(limit int) *outbox {
	return &outbox{limit: limit}
}

func (o *outbox) Add(frame protocol.Envelope) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.items) >= o.limit {
		o.items = o.items[1:]
		o.gap = true
	}
	o.items = append(o.items, frame)
}

func (o *outbox) Items() []protocol.Envelope {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]protocol.Envelope(nil), o.items...)
}

func (o *outbox) Remove(messageID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, item := range o.items {
		if item.MessageID == messageID {
			o.items = append(o.items[:i], o.items[i+1:]...)
			return
		}
	}
}

func (o *outbox) Gap() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gap
}
