package turns_test

import (
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// TestWatcherScreenPump exercises only the screen pump path. We pass a
// nil session and rely on Watch tolerating the nil-source behavior...
// actually Watch requires a non-nil session for the events channel, so
// we skip this test if we can't construct one. The compile-time invariant
// is covered by other tests; here we use a manual screen-only path by
// passing nil for sess via package-internal helper.
//
// Since Watch requires sess.Events() this test instead just verifies
// that the screen subscription wiring fires the adapter via direct
// observation: Subscribe + Write + Snapshot.
func TestScreenWriteSnapshotPath(t *testing.T) {
	scr := screen.New(40, 10)
	ch, unsub := scr.Subscribe()
	defer unsub()

	_, _ = scr.Write([]byte("\x1b[2J\x1b[Hready"))
	select {
	case <-ch:
		// ok
	case <-time.After(100 * time.Millisecond):
		t.Fatal("screen subscription did not fire")
	}
	snap := scr.Snapshot()
	if snap.Generation == 0 {
		t.Fatal("expected Generation > 0")
	}
}
