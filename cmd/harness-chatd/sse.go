package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/olesho/harness-wrapper/pkg/chat"
)

// fanout reads conv.Events() (single-reader) and broadcasts each event
// to all subscribed listeners. New subscribers only see events from
// their subscription point onward.
type fanout struct {
	mu          sync.Mutex
	subscribers map[string]chan chat.ConversationEvent
	closed      bool
}

func newFanout(src <-chan chat.ConversationEvent) *fanout {
	f := &fanout{subscribers: make(map[string]chan chat.ConversationEvent)}
	go f.pump(src)
	return f
}

func (f *fanout) pump(src <-chan chat.ConversationEvent) {
	for ev := range src {
		f.mu.Lock()
		for _, ch := range f.subscribers {
			select {
			case ch <- ev:
			default:
				// drop if subscriber is slow; matches pkg/chat policy
			}
		}
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.closed = true
	for id, ch := range f.subscribers {
		close(ch)
		delete(f.subscribers, id)
	}
	f.mu.Unlock()
}

// subscribe returns a buffered channel + an unsubscribe func. Channel
// is closed if the upstream is already closed.
func (f *fanout) subscribe() (<-chan chat.ConversationEvent, func()) {
	ch := make(chan chat.ConversationEvent, 64)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		close(ch)
		return ch, func() {
			// Already closed: nothing to unsubscribe.
		}
	}
	id := newToken()
	f.subscribers[id] = ch
	f.mu.Unlock()

	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if c, ok := f.subscribers[id]; ok {
			delete(f.subscribers, id)
			close(c)
		}
	}
}

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
