package harnesscore

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// DrainSpool reads the spool as ReadSpool does: it never follows a symlink
// out of it, never blocks on a FIFO and never reads a file over
// MaxSpoolFileBytes. Each is skipped, reported and left in place, and the
// genuine file beside them is still drained.
func TestDrainSpoolRefusesUntrustedFiles(t *testing.T) {
	spool := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := writeSpool(filepath.Dir(outside), "stop", textEvent("outside")); err != nil {
		t.Fatal(err)
	}
	for name := range spoolJSON(t, filepath.Dir(outside)) {
		outside = filepath.Join(filepath.Dir(outside), name)
	}
	link := filepath.Join(spool, "00000000000000000001-stop-1-1.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(spool, "00000000000000000002-stop-1-2.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(spool, "00000000000000000003-stop-1-3.json")
	f, err := os.Create(big) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxSpoolFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := writeSpool(spool, "stop", textEvent("genuine")); err != nil {
		t.Fatal(err)
	}

	type result struct {
		texts []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		evs, err := DrainSpool(spool)
		var texts []string
		for _, pe := range evs {
			texts = append(texts, pe.Event.Text)
		}
		done <- result{texts, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSpool blocked (on the FIFO?)")
	}
	if !slices.Equal(got.texts, []string{"genuine"}) {
		t.Errorf("drained %v, want only the genuine file's event", got.texts)
	}
	if got.err == nil {
		t.Fatal("no error for the skipped files")
	}
	for _, p := range []string{link, fifo, big} {
		if !strings.Contains(got.err.Error(), filepath.Base(p)) {
			t.Errorf("error does not name %s: %v", filepath.Base(p), got.err)
		}
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was not left in place: %v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the symlink's target went: %v", err)
	}
}

// A spool dir that is itself a symlink is refused, as ReadSpool refuses it.
func TestDrainSpoolRefusesSymlinkedSpoolDir(t *testing.T) {
	real := t.TempDir()
	if err := spoolText(real, "x"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "spool")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if evs, err := DrainSpool(link); err == nil || len(evs) != 0 {
		t.Errorf("DrainSpool(symlink) = %v, %v; want refused", evs, err)
	}
	if len(spoolJSON(t, real)) != 1 {
		t.Error("the symlinked spool was drained")
	}
}
