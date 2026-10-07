//go:build linux

package contain

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// staleState creates ephemeral state with no launch recorded and ages it past
// the sweep's minimum age.
func staleState(t *testing.T, now time.Time) string {
	t.Helper()
	s, err := NewState(false)
	if err != nil {
		t.Fatal(err)
	}
	root := s.Root()
	s.Close()
	old := now.Add(-2 * time.Minute)
	if err := os.Chtimes(root, old, old); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestSweepRunsInBackgroundOncePerInterval: Prepare's sweep never runs the
// (possibly 30 s per entry) recovery itself, only one sweep runs at a time,
// and a new one starts at most once per sweepInterval.
func TestSweepRunsInBackgroundOncePerInterval(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	var queued []func()
	sweepNow = func() time.Time { return now }
	sweepAsync = func(fn func()) { queued = append(queued, fn) }
	t.Cleanup(func() {
		sweepNow, sweepAsync = time.Now, func(fn func()) { go fn() }
		sweepMu.Lock()
		sweepRunning, sweepLast = false, time.Time{}
		sweepMu.Unlock()
	})
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	first := staleState(t, now)
	sweepStale()
	if len(queued) != 1 {
		t.Fatalf("sweeps queued: %d, want 1", len(queued))
	}
	if !exists(first) {
		t.Fatal("the sweep ran in the caller, not in the background")
	}
	sweepStale() // one already running
	if len(queued) != 1 {
		t.Fatalf("a second sweep started while one ran: %d queued", len(queued))
	}
	queued[0]()
	if exists(first) {
		t.Fatal("the background sweep left stale state")
	}

	second := staleState(t, now)
	sweepStale() // within the interval
	if len(queued) != 1 {
		t.Fatalf("a sweep started within the interval: %d queued", len(queued))
	}
	now = now.Add(sweepInterval)
	sweepStale()
	if len(queued) != 2 {
		t.Fatalf("no sweep after the interval: %d queued", len(queued))
	}
	queued[1]()
	if exists(second) {
		t.Fatal("the second sweep left stale state")
	}

	// A held entry is left for a later sweep.
	now = now.Add(sweepInterval)
	held := staleState(t, now)
	s, err := OpenState(filepath.Base(held))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.lock(); err != nil {
		t.Fatal(err)
	}
	sweepStale()
	if len(queued) != 3 {
		t.Fatalf("sweeps queued: %d, want 3", len(queued))
	}
	queued[2]()
	if !exists(held) {
		t.Fatal("the sweep removed state a launch holds")
	}
	s.Close()
}
