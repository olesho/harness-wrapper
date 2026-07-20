package screen

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteAndSnapshot(t *testing.T) {
	s := New(40, 10)
	_, _ = s.Write([]byte("\x1b[2J\x1b[Hhello \x1b[1mworld\x1b[0m"))
	snap := s.Snapshot()
	if !strings.Contains(snap.Text, "hello world") {
		t.Fatalf("expected snapshot to contain 'hello world', got: %q", snap.Text)
	}
	if snap.Generation != 1 {
		t.Fatalf("expected Generation=1, got %d", snap.Generation)
	}
	if snap.Cols != 40 || snap.Rows != 10 {
		t.Fatalf("expected 40x10, got %dx%d", snap.Cols, snap.Rows)
	}
}

func TestSubscribeSignalsOnWrite(t *testing.T) {
	s := New(40, 10)
	ch, unsub := s.Subscribe()
	defer unsub()

	_, _ = s.Write([]byte("hi"))

	select {
	case <-ch:
		// ok
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected subscribe channel to fire after Write")
	}
}

func TestSubscribeCoalesces(t *testing.T) {
	s := New(40, 10)
	ch, unsub := s.Subscribe()
	defer unsub()

	for i := 0; i < 100; i++ {
		_, _ = s.Write([]byte("x"))
	}
	// Drain: there should be exactly one pending signal regardless of write count.
	<-ch
	select {
	case <-ch:
		t.Fatal("expected coalesced signals, but a second one was pending")
	case <-time.After(20 * time.Millisecond):
		// ok
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	s := New(40, 10)
	ch, unsub := s.Subscribe()
	unsub()

	_, _ = s.Write([]byte("x"))
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed after unsubscribe")
		}
	case <-time.After(20 * time.Millisecond):
		// channel closed-and-drained is acceptable; the key invariant
		// is that no further values are delivered.
	}
}

func TestConcurrentWritesAreSafe(t *testing.T) {
	s := New(80, 24)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.Write([]byte("abcdefghij"))
				_ = s.Snapshot()
			}
		}()
	}
	wg.Wait()
	if g := s.Generation(); g != 400 {
		t.Fatalf("expected Generation=400, got %d", g)
	}
}

func TestResizeWithPeerFailurePreservesFullSnapshotAndDoesNotNotify(t *testing.T) {
	s := New(24, 8)
	if _, err := s.Write([]byte("\x1b[2J\x1b[Htop-left\x1b[8;19Hbottom")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before := s.Snapshot()
	if !strings.Contains(before.Text, "bottom") {
		t.Fatalf("test setup did not place content in the rows to be cropped: %q", before.Text)
	}

	notifications, unsubscribe := s.Subscribe()
	defer unsubscribe()
	wantErr := errors.New("peer resize failed")
	err := s.ResizeWithPeer(12, 4, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("ResizeWithPeer error = %v, want %v", err, wantErr)
	}
	if after := s.Snapshot(); after != before {
		t.Fatalf("failed shrink changed snapshot:\nbefore: %#v\n after: %#v", before, after)
	}
	select {
	case <-notifications:
		t.Fatal("failed resize notified subscribers")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestResizeWithPeerSuccessChangesGenerationAndNotifies(t *testing.T) {
	s := New(40, 10)
	before := s.Snapshot()
	notifications, unsubscribe := s.Subscribe()
	defer unsubscribe()

	called := false
	if err := s.ResizeWithPeer(20, 5, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("ResizeWithPeer: %v", err)
	}
	if !called {
		t.Fatal("peer resize was not called")
	}
	after := s.Snapshot()
	if after.Cols != 20 || after.Rows != 5 {
		t.Fatalf("size after resize = %dx%d, want 20x5", after.Cols, after.Rows)
	}
	if after.Generation != before.Generation+1 {
		t.Fatalf("generation after resize = %d, want %d", after.Generation, before.Generation+1)
	}
	select {
	case <-notifications:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("successful resize did not notify subscribers")
	}
}

func TestResizeWithPeerSameSizeInvokesPeerWithoutChangingScreen(t *testing.T) {
	s := New(40, 10)
	if _, err := s.Write([]byte("unchanged")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before := s.Snapshot()
	notifications, unsubscribe := s.Subscribe()
	defer unsubscribe()

	called := false
	if err := s.ResizeWithPeer(40, 10, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("ResizeWithPeer: %v", err)
	}
	if !called {
		t.Fatal("same-size resize did not invoke peer")
	}
	if after := s.Snapshot(); after != before {
		t.Fatalf("same-size resize changed snapshot:\nbefore: %#v\n after: %#v", before, after)
	}
	select {
	case <-notifications:
		t.Fatal("same-size resize notified subscribers")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestResizeWithPeerDoesNotBlockWritesOrSnapshots pins the Option 1 contract:
// the screen lock is NOT held across a (possibly slow / kernel-blocking) peer
// resize, so Write and Snapshot stay live while it runs. The emulator is
// committed to the new dimensions only after the peer succeeds, and that commit
// is atomic.
func TestResizeWithPeerDoesNotBlockWritesOrSnapshots(t *testing.T) {
	s := New(40, 10)
	if _, err := s.Write([]byte("before")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before := s.Snapshot()

	peerEntered := make(chan struct{})
	allowPeer := make(chan struct{})
	resizeDone := make(chan error, 1)
	go func() {
		resizeDone <- s.ResizeWithPeer(20, 5, func() error {
			close(peerEntered)
			<-allowPeer
			return nil
		})
	}()
	select {
	case <-peerEntered:
	case <-time.After(time.Second):
		t.Fatal("peer resize did not start")
	}

	// While the peer resize is still running, Snapshot must NOT block. The
	// emulator is not yet committed, so it still reports the pre-resize size.
	snapshotDone := make(chan Snapshot, 1)
	go func() { snapshotDone <- s.Snapshot() }()
	select {
	case snap := <-snapshotDone:
		if snap.Cols != before.Cols || snap.Rows != before.Rows {
			t.Fatalf("Snapshot during peer resize = %dx%d, want pre-commit %dx%d",
				snap.Cols, snap.Rows, before.Cols, before.Rows)
		}
	case <-time.After(time.Second):
		t.Fatal("Snapshot blocked during peer resize — screen lock held across peer op")
	}

	// Write must not block either.
	writeDone := make(chan error, 1)
	go func() { _, err := s.Write([]byte("\x1b[Hafter")); writeDone <- err }()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("Write during peer resize: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Write blocked during peer resize — screen lock held across peer op")
	}

	// Let the peer finish; the emulator commits to the new dimensions.
	close(allowPeer)
	select {
	case err := <-resizeDone:
		if err != nil {
			t.Fatalf("ResizeWithPeer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("resize did not complete")
	}

	after := s.Snapshot()
	if after.Cols != 20 || after.Rows != 5 {
		t.Fatalf("final size = %dx%d, want 20x5", after.Cols, after.Rows)
	}
	if !strings.Contains(after.Text, "after") {
		t.Fatalf("final screen does not contain the write issued during resize: %q", after.Text)
	}
	// "before" write (gen+1), the "after" write during the resize (gen+1), and
	// the resize commit (gen+1).
	if after.Generation != before.Generation+2 {
		t.Fatalf("final generation = %d, want %d", after.Generation, before.Generation+2)
	}
}
