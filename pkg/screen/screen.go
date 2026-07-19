// Package screen wraps a vt100 terminal emulator (vt10x, per ADR-001)
// behind a small concurrent-safe surface. Callers feed raw PTY bytes
// via Write and read coherent screen snapshots via Snapshot. Subscribe
// returns a coalesced notification channel that fires after every successful
// Write or size change so observers (turn detectors, gateways) can react
// without polling.
//
// The Screen is the substrate the turn-detection layer reads from. It
// intentionally exposes only what that layer needs: the rendered text,
// terminal dimensions, cursor position, and a monotonically increasing
// generation counter for change detection.
package screen

import (
	"sync"

	"github.com/hinshun/vt10x"
)

// Snapshot is a coherent point-in-time view of the emulated screen.
// Snapshots are taken under a read lock and are safe to retain — the
// underlying terminal state will continue to mutate independently.
type Snapshot struct {
	// Text is the rendered screen contents, top-to-bottom, with one
	// '\n' per row. Trailing whitespace per row is preserved; callers
	// that compare snapshots should normalize first.
	Text string

	// Cols, Rows are the terminal dimensions in cells.
	Cols, Rows int

	// CursorCol, CursorRow are the 0-indexed cursor position.
	CursorCol, CursorRow int

	// Generation increments on each successful Write or resize. Compare
	// against the previous snapshot's Generation to skip no-op redraws.
	Generation uint64
}

// Screen wraps a vt100 emulator with change-notification fan-out.
//
// All methods are safe for concurrent use.
type Screen struct {
	cols, rows int

	// resizeMu serializes ResizeWithPeer/Resize calls so a fallible peer
	// resize and the matching emulator commit happen as one operation, without
	// interleaving. It is distinct from mu on purpose: Write, Snapshot, and
	// Generation take only mu, so they stay live while a (possibly
	// kernel-blocking) peer resize runs under resizeMu.
	resizeMu sync.Mutex

	mu   sync.RWMutex
	term vt10x.Terminal
	gen  uint64

	subMu sync.Mutex
	subs  []chan struct{}
}

// New constructs a Screen of the given dimensions.
//
// Cols and rows must be > 0. Defaults of 120x40 are applied to
// non-positive inputs to make tests and quick experiments forgiving.
func New(cols, rows int) *Screen {
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 40
	}
	return &Screen{
		cols: cols,
		rows: rows,
		term: vt10x.New(vt10x.WithSize(cols, rows)),
	}
}

// Write feeds raw PTY bytes (ANSI escapes intact) into the emulator.
// On success it bumps Generation and signals every subscriber. Errors
// are returned verbatim from the underlying emulator.
//
// Write is the io.Writer entry point intended for wrapper.Session.AttachOutput.
func (s *Screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.term.Write(p)
	if err == nil {
		s.gen++
	}
	s.mu.Unlock()
	if err == nil {
		s.notify()
	}
	return n, err
}

// Snapshot returns a coherent point-in-time view of the emulated screen.
func (s *Screen) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.term.Cursor()
	cols, rows := s.term.Size()
	return Snapshot{
		Text:       s.term.String(),
		Cols:       cols,
		Rows:       rows,
		CursorCol:  c.X,
		CursorRow:  c.Y,
		Generation: s.gen,
	}
}

// Generation returns the current change counter without rendering a snapshot.
// Useful for cheap "has anything changed?" checks.
func (s *Screen) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gen
}

// Resize changes the terminal dimensions. Existing screen content is
// preserved as best the emulator allows.
func (s *Screen) Resize(cols, rows int) {
	_ = s.ResizeWithPeer(cols, rows, nil)
}

// ResizeWithPeer changes the terminal dimensions while synchronizing with a
// fallible resize of a peer terminal, such as the PTY that feeds this screen.
//
// The peer is resized FIRST, without holding the screen read/write lock (mu);
// only when it succeeds is the emulator committed to the new dimensions under a
// brief mu critical section. The screen lock is deliberately NOT held across
// resizePeer: a PTY window-size ioctl can block in the kernel behind an
// in-flight stdin write, and holding mu across it would freeze Write, Snapshot,
// and Generation — and, for callers that resize while holding their own locks
// (e.g. chat.Conversation), deadlock. The cost is a brief, self-correcting
// transient: output produced for the peer's new dimensions may render in the
// not-yet-committed emulator until the commit below, after which the next
// Write/Snapshot reflects the correct size.
//
// A peer failure leaves the screen's contents, cursor, dimensions, and
// generation untouched and sends no subscriber notification. Concurrent
// resizes are serialized by resizeMu so the peer resize and its commit never
// interleave.
//
// resizePeer must not call back into this Screen. A nil callback performs an
// ordinary screen-only resize. Non-positive dimensions are no-ops. For
// unchanged dimensions resizePeer is still invoked so a divergent peer can be
// healed, but the screen generation and subscribers remain untouched.
func (s *Screen) ResizeWithPeer(cols, rows int, resizePeer func() error) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}

	s.resizeMu.Lock()
	defer s.resizeMu.Unlock()

	if resizePeer != nil {
		if err := resizePeer(); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if s.cols == cols && s.rows == rows {
		s.mu.Unlock()
		return nil
	}
	s.cols, s.rows = cols, rows
	s.term.Resize(cols, rows)
	s.gen++
	s.mu.Unlock()
	s.notify()
	return nil
}

// Subscribe returns a notification channel that signals (non-blocking,
// coalesced into a buffer of 1) after every successful Write or size change.
// Callers should drain it and then call Snapshot to read current state.
//
// Returns an unsubscribe function that removes the channel and closes it.
func (s *Screen) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subs = append(s.subs, ch)
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		defer s.subMu.Unlock()
		for i, c := range s.subs {
			if c == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
}

func (s *Screen) notify() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
			// Subscriber already has a pending signal; coalesce.
		}
	}
}
