package harness

import "sync/atomic"

// displaySinkCap bounds the best-effort display queue. Generous enough to absorb
// normal bursts; under sustained back-pressure the OLDEST lines are dropped (the
// user wants the latest output), counted, and never block the read loop.
const displaySinkCap = 1024

// displaySink delivers raw output lines to a best-effort consumer callback
// (harness.Config.OnDisplayLine) WITHOUT ever stalling the durable PTY read
// loop. The tap pushes lines non-blockingly; a dedicated goroutine drains them
// to the callback. This is deliberately separate from the durable OnEvent /
// line-tap path (reviews #3/#14): display is non-critical and may drop, the
// transcript path must not.
type displaySink struct {
	ch      chan string
	onLine  func(string)
	done    chan struct{}
	dropped atomic.Uint64
}

// newDisplaySink starts the drain goroutine, or returns nil when onLine is nil
// (the push/close methods are nil-safe, so callers need not branch).
func newDisplaySink(onLine func(string)) *displaySink {
	if onLine == nil {
		return nil
	}
	d := &displaySink{ch: make(chan string, displaySinkCap), onLine: onLine, done: make(chan struct{})}
	go d.run()
	return d
}

func (d *displaySink) run() {
	defer close(d.done)
	for line := range d.ch {
		d.deliver(line)
	}
}

// deliver calls the callback with panic recovery: a misbehaving display sink is
// dropped, never crashing the drain goroutine (display is non-critical).
func (d *displaySink) deliver(line string) {
	defer func() { _ = recover() }()
	d.onLine(line)
}

// push enqueues a line without blocking. It is called ONLY from the single tap
// goroutine. On a full queue it drops the OLDEST line to make room for the
// newest, counting the drop. A nil sink is a no-op.
func (d *displaySink) push(line string) {
	if d == nil {
		return
	}
	select {
	case d.ch <- line:
		return
	default:
	}
	// Full: evict the oldest (racing only the drain goroutine's receive), then
	// enqueue the newest. Both steps are non-blocking, so the read loop never
	// stalls.
	select {
	case <-d.ch:
		d.dropped.Add(1)
	default:
	}
	select {
	case d.ch <- line:
	default:
		d.dropped.Add(1)
	}
}

// close stops the sink and returns the dropped-line count. It must be called
// AFTER the tap goroutine has joined (no concurrent push), so closing the
// channel is safe. A nil sink returns 0.
func (d *displaySink) close() uint64 {
	if d == nil {
		return 0
	}
	close(d.ch)
	<-d.done
	return d.dropped.Load()
}
