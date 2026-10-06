package codex

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Starts in one environment take turns: a second waits while the first
// holds the lock, gives up when its context ends, and goes once the first
// lets go, which it may do twice.
func TestStartLock(t *testing.T) {
	scratch := t.TempDir()
	release, err := startLock(context.Background(), scratch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = startLock(ctx, scratch)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a start while another holds the lock, until its context ends: %v, want the deadline", err)
	}
	next := make(chan func(), 1)
	go func() {
		if r, err := startLock(context.Background(), scratch); err == nil {
			next <- r
		}
	}()
	select {
	case <-next:
		t.Fatal("a start went while another held the lock")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	release()
	select {
	case r := <-next:
		r()
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting start never went")
	}
}
