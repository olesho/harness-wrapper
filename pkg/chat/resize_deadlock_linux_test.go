//go:build linux

package chat

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"golang.org/x/term"
)

// TestLinuxDeadlockHelper is re-executed under a PTY. It writes a 1 MB stdout
// burst (far larger than Linux's ~64 KB PTY buffer) BEFORE it next reads stdin.
// So if the parent's PTY read loop stalls, the child wedges partway through the
// write and never reaches the read — it can no longer drain the parent's stdin.
func TestLinuxDeadlockHelper(t *testing.T) {
	if os.Getenv("HW_LINUX_DEADLOCK_HELPER") != "1" {
		return
	}
	if r, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer func() { _ = term.Restore(int(os.Stdin.Fd()), r) }()
	}
	chunk := bytes.Repeat([]byte("z"), 1024*1024)
	sink := make([]byte, 64*1024)
	for {
		if _, err := os.Stdout.Write(chunk); err != nil {
			return
		}
		if _, err := os.Stdin.Read(sink); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond) // slow stdin drain: keeps the write in flight
	}
}

func snapshotBlocksLinux(conv *Conversation, d time.Duration) bool {
	done := make(chan struct{})
	go func() { conv.screen.Snapshot(); close(done) }()
	select {
	case <-done:
		return false
	case <-time.After(d):
		return true
	}
}

// TestLinuxResizeDeadlock drives the real scenario end-to-end on Linux: a large
// WriteStdin is in flight when conv.Resize runs. On the pre-fix code, conv.Resize
// holds the screen lock across the peer PTY resize, which blocks on the stdin
// lock; the frozen read loop then wedges the child, which can never drain the
// write — a permanent deadlock, and ScreenSnapshot is frozen forever. On the
// fixed code the read loop stays live, the child drains the write, the resize
// unblocks, and ScreenSnapshot never freezes.
func TestLinuxResizeDeadlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conv, err := Open(ctx, Options{
		Harness:    "generic",
		BinaryPath: os.Args[0],
		Args:       []string{"-test.run=^TestLinuxDeadlockHelper$"},
		Env:        append(os.Environ(), "HW_LINUX_DEADLOCK_HELPER=1"),
		Cols:       80,
		Rows:       24,
		Store:      newFakeStore(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = conv.Wrapper().Stop(context.Background()) }()

	// Stuck stdin write: the child only reads stdin after finishing a 1 MB
	// stdout burst, so once the read loop stalls it can never drain this.
	go func() { _, _ = conv.Wrapper().WriteStdin(bytes.Repeat([]byte("i"), 8*1024*1024)) }()
	time.Sleep(300 * time.Millisecond)

	rzDone := make(chan struct{})
	go func() { _ = conv.Resize(132, 43); close(rzDone) }()
	time.Sleep(300 * time.Millisecond) // let the resize reach (and, pre-fix, block inside) the peer op

	frozen := snapshotBlocksLinux(conv, 2*time.Second)
	resizeReturned := false
	select {
	case <-rzDone:
		resizeReturned = true
	default:
	}
	t.Logf("RESULT: screenFrozen=%v resizeReturnedWithin~2.6s=%v", frozen, resizeReturned)

	if frozen {
		t.Fatalf("DEADLOCK/FREEZE: ScreenSnapshot frozen while conv.Resize is in flight behind a stuck WriteStdin")
	}

	// On the fixed code the resize also recovers once the write drains (the
	// read loop stays live, so the child keeps consuming stdin).
	select {
	case <-rzDone:
		t.Logf("conv.Resize returned — system recovered")
	case <-time.After(2 * time.Second):
		t.Logf("conv.Resize still draining the slow child, but screen stayed responsive throughout")
	}
}
