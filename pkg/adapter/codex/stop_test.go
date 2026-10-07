package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
)

// TestStopBoundedAfterKill: a group that never reads as empty after SIGKILL
// (a member nobody reaps) holds Stop for killWait, not forever.
func TestStopBoundedAfterKill(t *testing.T) {
	oldEmpty, oldWait := groupEmpty, killWait
	groupEmpty = func(*exec.Cmd) bool { return false }
	killWait = 200 * time.Millisecond
	t.Cleanup(func() { groupEmpty, killWait = oldEmpty, oldWait })

	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil { //nolint:gosec // a test's script
		t.Fatal(err)
	}
	tr := &transport{
		report: func(adapter.Event) {},
		calls:  map[int64]chan rpcResult{}, wake: make(chan struct{}),
		exited: make(chan struct{}), readerEnd: make(chan struct{}),
		stderr: newTailBuffer(stderrTail),
	}
	if err := tr.start(bin, dir, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- tr.Stop(context.Background(), 0) }()
	select {
	case gone := <-done:
		if gone {
			t.Error("Stop reported the group gone")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after killWait")
	}
	<-tr.exited
}
