// Package delivery is the bounded, ordered event delivery behind the wrapper's
// and chat's OnEvent callbacks (ADR-008): one worker hands each event to the
// callback in the order it was pushed, and a producer that finds the queue full
// waits for room rather than dropping the event.
package delivery

import (
	"errors"
	"sync"
	"time"
)

// Default bounds of a queue.
const (
	DefaultEvents = 1024
	DefaultBytes  = 16 << 20
)

// Limits bounds what a queue holds at once: events, and their payload bytes.
// A zero field takes its default.
type Limits struct {
	Events int
	Bytes  int64
}

// Resolved returns l with its zero fields defaulted.
func (l Limits) Resolved() Limits {
	if l.Events <= 0 {
		l.Events = DefaultEvents
	}
	if l.Bytes <= 0 {
		l.Bytes = DefaultBytes
	}
	return l
}

var (
	// ErrTooLarge is returned by Push for an event larger than the queue's
	// byte bound. It was not queued: the caller decides what to deliver in
	// its place.
	ErrTooLarge = errors.New("delivery: event larger than the queue's byte bound")

	// ErrAbandoned is returned by Push once the queue is closed or abandoned,
	// or when the push gave up waiting for room. The event was not queued,
	// and is counted in Stats.Undelivered.
	ErrAbandoned = errors.New("delivery: queue closed")
)

// Stats is a queue's pressure, for diagnostics.
type Stats struct {
	Limits      Limits
	Queued      int
	QueuedBytes int64
	// WaitingSince is when the longest-waiting producer began waiting for
	// room; zero when none waits.
	WaitingSince time.Time
	// DeliveringSince is when the callback now running was called; zero when
	// the worker is idle.
	DeliveringSince time.Time
	Delivered       uint64
	// Undelivered counts events pushed and never delivered: refused after
	// the queue closed, given up on while waiting for room, or left queued
	// when it was abandoned.
	Undelivered uint64
}

type entry[T any] struct {
	v    T
	size int64
}

// Queue delivers pushed values to one callback, in push order, from one
// worker goroutine. The zero value is not usable; construct with New.
type Queue[T any] struct {
	limits  Limits
	size    func(T) int64
	deliver func(T)

	mu         sync.Mutex
	items      []entry[T]
	bytes      int64
	room       chan struct{} // closed and replaced when room frees up
	ready      chan struct{} // signalled when an item is queued or the queue closes
	closed     bool
	abandoned  bool
	waiters    map[*waiter]struct{}
	delivering time.Time
	delivered  uint64
	lost       uint64
	drained    chan struct{}
}

type waiter struct{ since time.Time }

// New starts a queue that hands each value to deliver, in order. size reports
// a value's payload bytes.
func New[T any](limits Limits, size func(T) int64, deliver func(T)) *Queue[T] {
	q := &Queue[T]{
		limits:  limits.Resolved(),
		size:    size,
		deliver: deliver,
		room:    make(chan struct{}),
		ready:   make(chan struct{}, 1),
		waiters: map[*waiter]struct{}{},
		drained: make(chan struct{}),
	}
	go q.run()
	return q
}

// Push queues v behind every value pushed before it, waiting while the queue
// is at either bound. It returns ErrTooLarge, without queueing, for a value
// larger than the byte bound, and ErrAbandoned when the queue closes, or abort
// closes, before v is queued. A value alone in the queue is always taken, so a
// value within the bound never waits forever for an empty queue.
func (q *Queue[T]) Push(v T, abort <-chan struct{}) error {
	n := q.size(v)
	if n > q.limits.Bytes {
		return ErrTooLarge
	}
	q.mu.Lock()
	var w *waiter
	defer func() {
		if w != nil {
			delete(q.waiters, w)
		}
		q.mu.Unlock()
	}()
	for {
		if q.closed || q.abandoned {
			q.lost++
			return ErrAbandoned
		}
		if len(q.items) == 0 || (len(q.items) < q.limits.Events && q.bytes+n <= q.limits.Bytes) {
			q.enqueue(v, n)
			return nil
		}
		if w == nil {
			w = &waiter{since: time.Now()}
			q.waiters[w] = struct{}{}
		}
		room := q.room
		q.mu.Unlock()
		select {
		case <-room:
			q.mu.Lock()
		case <-abort:
			q.mu.Lock()
			q.lost++
			return ErrAbandoned
		}
	}
}

// PushLast queues v past the bounds, as the last value the queue takes, and
// closes it. It never waits, so a producer ending the stream — a harness
// that exited — is never held by a stalled consumer. Producers still waiting
// for room get ErrAbandoned: their values would otherwise follow the last one.
func (q *Queue[T]) PushLast(v T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.abandoned {
		q.lost++
		return
	}
	q.enqueue(v, q.size(v))
	q.closeLocked()
}

// Close stops the queue taking values. The worker delivers what is queued,
// then Drained closes. Producers waiting for room get ErrAbandoned.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closeLocked()
}

// Abandon closes the queue and drops what it still holds, counting it
// undelivered. A callback already running is not interrupted.
func (q *Queue[T]) Abandon() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lost += uint64(len(q.items))
	q.items = nil
	q.bytes = 0
	q.abandoned = true
	q.closeLocked()
}

// Drained is closed once the queue is closed and everything it took has been
// delivered, or dropped by Abandon.
func (q *Queue[T]) Drained() <-chan struct{} { return q.drained }

// Stats reports the queue's pressure.
func (q *Queue[T]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := Stats{
		Limits:          q.limits,
		Queued:          len(q.items),
		QueuedBytes:     q.bytes,
		DeliveringSince: q.delivering,
		Delivered:       q.delivered,
		Undelivered:     q.lost,
	}
	for w := range q.waiters {
		if st.WaitingSince.IsZero() || w.since.Before(st.WaitingSince) {
			st.WaitingSince = w.since
		}
	}
	return st
}

func (q *Queue[T]) enqueue(v T, n int64) {
	q.items = append(q.items, entry[T]{v: v, size: n})
	q.bytes += n
	q.signal()
}

func (q *Queue[T]) closeLocked() {
	if q.closed {
		return
	}
	q.closed = true
	close(q.room) // wake every waiter; each sees closed
	q.room = make(chan struct{})
	q.signal()
}

func (q *Queue[T]) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// run is the worker: it delivers queued values one at a time, in order, and
// exits once the queue is closed and empty.
func (q *Queue[T]) run() {
	defer close(q.drained)
	for {
		q.mu.Lock()
		for len(q.items) == 0 {
			if q.closed {
				q.mu.Unlock()
				return
			}
			q.mu.Unlock()
			<-q.ready
			q.mu.Lock()
		}
		e := q.items[0]
		var zero entry[T]
		q.items[0] = zero
		q.items = q.items[1:]
		q.bytes -= e.size
		close(q.room)
		q.room = make(chan struct{})
		q.delivering = time.Now()
		q.mu.Unlock()

		q.deliver(e.v)

		q.mu.Lock()
		q.delivering = time.Time{}
		q.delivered++
		q.mu.Unlock()
	}
}
