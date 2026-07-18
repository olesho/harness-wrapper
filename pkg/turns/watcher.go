package turns

import (
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Watcher composes a wrapper.Session, a screen.Screen, and an Adapter
// into a single <-chan Event stream.
//
// Typical use:
//
//	scr := screen.New(120, 40)
//	cfg := wrapper.Config{ ..., Stdout: scr } // or use sess.AttachOutput(scr)
//	sess, _ := wrapper.Start(ctx, cfg)
//	w := turns.Watch(sess, scr, generic.New())
//	defer w.Close()
//	for ev := range w.Events() {
//	    ...
//	}
//
// The events channel is closed after both source streams (the wrapper's
// SessionEvent channel and the screen's subscription) are exhausted —
// i.e. after Session terminates AND Close() is called.
type Watcher struct {
	events chan Event

	closeOnce sync.Once
	done      chan struct{}

	wg sync.WaitGroup
}

// Watch starts a Watcher. It does not consume the session's Wait/Stop
// methods; the caller still owns those. Watch is non-blocking; the
// event-pumping goroutines run in the background until both sources
// stop AND Close() is called.
//
// Pass nil for scr to skip screen-derived signals (e.g. when using an
// adapter that only consumes wrapper.Status).
func Watch(sess *wrapper.Session, scr *screen.Screen, adapter Adapter) *Watcher {
	w := &Watcher{
		events: make(chan Event, 32),
		done:   make(chan struct{}),
	}

	// Pump 1: wrapper session events → adapter.OnWrapperStatus.
	w.wg.Add(1)
	go w.pumpStatus(sess, adapter)

	// Pump 2: screen subscription → adapter.OnScreen. Subscribe
	// synchronously so no snapshot is missed before the pump starts.
	if scr != nil {
		notifyCh, unsubscribe := scr.Subscribe()
		w.wg.Add(1)
		go w.pumpScreen(scr, adapter, notifyCh, unsubscribe)
	}

	// Closer: drain both pumps then close events.
	go func() {
		w.wg.Wait()
		close(w.events)
	}()

	return w
}

// pumpStatus forwards wrapper session events through adapter.OnWrapperStatus
// until the session terminates.
func (w *Watcher) pumpStatus(sess *wrapper.Session, adapter Adapter) {
	defer w.wg.Done()
	for ev := range sess.Events() {
		for _, te := range adapter.OnWrapperStatus(ev.Status, ev.Reason) {
			w.send(enrichFromStatus(te, ev))
		}
		if ev.Terminated {
			return
		}
	}
}

// pumpScreen forwards screen snapshots through adapter.OnScreen until the
// screen subscription closes or Close() is called.
func (w *Watcher) pumpScreen(scr *screen.Screen, adapter Adapter, notifyCh <-chan struct{}, unsubscribe func()) {
	defer w.wg.Done()
	defer unsubscribe()
	for {
		select {
		case <-w.done:
			return
		case _, ok := <-notifyCh:
			if !ok {
				return
			}
			snap := scr.Snapshot()
			for _, te := range adapter.OnScreen(snap) {
				w.send(enrichFromScreen(te, snap))
			}
		}
	}
}

// enrichFromStatus backfills the structured fields the adapter contract
// doesn't see (timestamp, HTTP code, retry-after) from the wrapper event.
// Adapters may set these explicitly (rare); only zero values are filled.
func enrichFromStatus(te Event, ev wrapper.SessionEvent) Event {
	if te.At.IsZero() {
		te.At = ev.At
	}
	if te.HTTPCode == 0 {
		te.HTTPCode = ev.HTTPCode
	}
	if te.RetryAfter == 0 {
		te.RetryAfter = ev.RetryAfter
	}
	return te
}

// enrichFromScreen backfills the timestamp and originating snapshot for a
// screen-derived event when the adapter left them unset.
func enrichFromScreen(te Event, snap screen.Snapshot) Event {
	if te.At.IsZero() {
		te.At = time.Now()
	}
	if te.Snap == nil {
		s := snap
		te.Snap = &s
	}
	return te
}

// Events returns the channel of turn events. It is closed after both
// the wrapper session and the watcher itself have terminated.
func (w *Watcher) Events() <-chan Event { return w.events }

// Close signals the screen-pumping goroutine to stop. It does not stop
// the wrapper session; the caller owns sess.Stop. Safe to call
// multiple times.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	return nil
}

func (w *Watcher) send(ev Event) {
	select {
	case w.events <- ev:
	case <-w.done:
	}
}
