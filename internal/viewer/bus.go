package viewer

import (
	"sync"

	"github.com/mustacheride/nest-to-ONVIF/internal/events"
)

// EventBus fans out motion edges to SSE subscribers.
type EventBus struct {
	mu      sync.RWMutex
	clients map[chan events.Edge]struct{}
}

func NewEventBus() *EventBus {
	return &EventBus{clients: map[chan events.Edge]struct{}{}}
}

func (b *EventBus) Publish(e events.Edge) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- e:
		default:
		}
	}
}

func (b *EventBus) Subscribe() (chan events.Edge, func()) {
	ch := make(chan events.Edge, 8)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.clients, ch)
		b.mu.Unlock()
		close(ch)
	}
}
