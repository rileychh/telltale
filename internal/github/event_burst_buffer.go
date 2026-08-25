package github

import (
	"sync"
	"time"
)

const eventBurstSettleTimeout = 500 * time.Millisecond

type eventBurstKey struct {
	repo   string
	number int
	actor  string
}

type pendingEventBurst[T any] struct {
	events []T
	timer  *time.Timer
}

type eventBurstBuffer[T any] struct {
	mu      sync.Mutex
	pending map[eventBurstKey]*pendingEventBurst[T]
	flush   func(eventBurstKey)
}

func newEventBurstBuffer[T any](flush func(eventBurstKey)) *eventBurstBuffer[T] {
	return &eventBurstBuffer[T]{
		pending: make(map[eventBurstKey]*pendingEventBurst[T]),
		flush:   flush,
	}
}

func (b *eventBurstBuffer[T]) add(key eventBurstKey, event T) {
	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.pending[key]
	if !ok {
		p = &pendingEventBurst[T]{}
		b.pending[key] = p
		p.timer = time.AfterFunc(eventBurstSettleTimeout, func() {
			b.flush(key)
		})
	} else {
		p.timer.Reset(eventBurstSettleTimeout)
	}
	p.events = append(p.events, event)
}

func (b *eventBurstBuffer[T]) take(key eventBurstKey) []T {
	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.pending[key]
	if !ok {
		return nil
	}
	delete(b.pending, key)
	return p.events
}
