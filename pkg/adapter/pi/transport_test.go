package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// fakePi is the far end of a transport's pipes: it reads the commands the
// transport writes and writes pi's lines.
type fakePi struct {
	out *os.File // the transport's stdout, written here
	in  *bufio.Scanner
}

// pipeTransport is a transport on pipes, with no process: its reader runs,
// and the fakePi plays pi.
func pipeTransport(t *testing.T, scratch string, report func(adapter.Event)) (*transport, *fakePi) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tr := &transport{
		id: "s", scratch: scratch, report: report,
		waiting: map[string]chan rpcResult{},
		exited:  make(chan struct{}), readerEnd: make(chan struct{}),
		stderr: newTailBuffer(stderrTail),
		stdin:  inW, stdout: outR,
	}
	go tr.read()
	t.Cleanup(func() {
		_ = outW.Close()
		<-tr.readerEnd
		_, _, _ = inW.Close(), inR.Close(), outR.Close()
	})
	return tr, &fakePi{out: outW, in: bufio.NewScanner(inR)}
}

// next is the next command the transport wrote.
func (f *fakePi) next(t *testing.T) map[string]any {
	t.Helper()
	if !f.in.Scan() {
		t.Fatalf("no command: %v", f.in.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(f.in.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *fakePi) send(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if _, err := f.out.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// TestInterruptUnnoted: an interrupt whose note fails sends no abort and
// leaves the turn unmarked, so an error that ends it is a failure, not an
// interrupt.
func TestInterruptUnnoted(t *testing.T) {
	var ended []adapter.Event
	tr, pi := pipeTransport(t, t.TempDir(), func(ev adapter.Event) {
		if ev.Kind == adapter.Ended {
			ended = append(ended, ev)
		}
	})
	tr.mu.Lock()
	tr.turn = &turnState{native: "not a tag!", started: true}
	tr.mu.Unlock()
	if err := tr.Interrupt(context.Background()); err == nil {
		t.Fatal("Interrupt noted a bad tag")
	}
	pi.send(t, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": abortedMessage}})
	pi.send(t, map[string]any{"type": "agent_settled"})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		tr.rmu.Lock()
		n := len(ended)
		tr.rmu.Unlock()
		if n > 0 {
			break
		}
	}
	tr.rmu.Lock()
	defer tr.rmu.Unlock()
	if len(ended) != 1 || ended[0].Outcome != contract.TurnErrored {
		t.Fatalf("ended %+v, want the turn errored", ended)
	}
}

// TestReportsSerialized: Submit reports the turn begun from its caller's
// goroutine while the reader reports its end; the reports never overlap,
// and Started comes before Ended.
func TestReportsSerialized(t *testing.T) {
	for i := range 30 {
		var (
			in      atomic.Int32
			overlap atomic.Bool
			kinds   []adapter.EventKind // unguarded: -race flags concurrent reports
		)
		report := func(ev adapter.Event) {
			if in.Add(1) > 1 {
				overlap.Store(true)
			}
			time.Sleep(time.Millisecond)
			kinds = append(kinds, ev.Kind)
			in.Add(-1)
		}
		tr, pi := pipeTransport(t, t.TempDir(), report)
		done := make(chan error, 1)
		go func() {
			done <- tr.Submit(context.Background(), adapter.Submission{Native: "n", Text: "hi"})
		}()
		cmd := pi.next(t)
		// The answer and the run's end arrive together: the reader reports
		// Ended while Submit reports Started.
		pi.send(t, map[string]any{"type": "response", "id": cmd["id"], "command": "prompt", "success": true, "data": map[string]any{"disposition": "started"}})
		pi.send(t, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": "ok"}})
		pi.send(t, map[string]any{"type": "agent_settled"})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		var got []adapter.EventKind
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			tr.rmu.Lock()
			got = append([]adapter.EventKind(nil), kinds...)
			tr.rmu.Unlock()
			if len(got) >= 2 {
				break
			}
		}
		if overlap.Load() {
			t.Fatalf("run %d: reports overlapped", i)
		}
		if len(got) != 2 || got[0] != adapter.Started || got[1] != adapter.Ended {
			t.Fatalf("run %d: reports %v, want Started then Ended", i, got)
		}
	}
}
