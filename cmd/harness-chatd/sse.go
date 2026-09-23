package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/olesho/harness-wrapper/pkg/chat"
)

// fanout broadcasts a conversation's events to every subscribed listener. It
// is the conversation's OnEvent, so it sees every event, in order (ADR-008);
// a subscriber too slow to take one loses it. New subscribers see events from
// their subscription on. EventExited is the last: publishing it closes every
// subscription.
type fanout struct {
	mu          sync.Mutex
	subscribers map[string]chan chat.ConversationEvent
	closed      bool
}

func newFanout() *fanout {
	return &fanout{subscribers: make(map[string]chan chat.ConversationEvent)}
}

// publish is the conversation's OnEvent.
func (f *fanout) publish(ev chat.ConversationEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	for _, ch := range f.subscribers {
		select {
		case ch <- ev:
		default:
			// drop if subscriber is slow; matches pkg/chat policy
		}
	}
	if ev.Type == chat.EventExited {
		f.closed = true
		for id, ch := range f.subscribers {
			close(ch)
			delete(f.subscribers, id)
		}
	}
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
