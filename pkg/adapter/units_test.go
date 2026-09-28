package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A marker is written once, found by input and by native id, and a store that
// lost its sentinel proves no absence.
func TestMarkers(t *testing.T) {
	scratch := t.TempDir()
	m, err := OpenMarkers(scratch)
	if err != nil {
		t.Fatal(err)
	}
	mk := Marker{InputID: "..", Native: "n-1", SessionID: "s", At: time.Now().UTC()}
	if err := m.Write(mk); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(mk); !errors.Is(err, ErrMarked) {
		t.Errorf("a second marker for the input: %v, want ErrMarked", err)
	}
	if got, ok, err := m.Lookup(".."); err != nil || !ok || got.Native != "n-1" {
		t.Errorf("Lookup = %+v %v %v", got, ok, err)
	}
	if _, ok, err := m.Lookup("in-2"); err != nil || ok {
		t.Errorf("Lookup of an input never marked = %v %v, want absent", ok, err)
	}
	// Another process's store sees the marker by its native id.
	other, _ := OpenMarkers(scratch)
	if got, ok := other.ByNative("n-1"); !ok || got.InputID != ".." {
		t.Errorf("ByNative = %+v %v", got, ok)
	}
	// A withdrawn marker is gone for good: by input, by native id, and to
	// another process's store; withdrawing it again changes nothing.
	if err := m.Write(Marker{InputID: "in-4", Native: "n-4", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Withdraw("in-4"); err != nil {
			t.Fatalf("Withdraw: %v", err)
		}
	}
	if _, ok, err := m.Lookup("in-4"); err != nil || ok {
		t.Errorf("Lookup of a withdrawn marker = %v %v, want absent", ok, err)
	}
	if _, ok := m.ByNative("n-4"); ok {
		t.Error("ByNative finds a withdrawn marker")
	}
	if _, ok := other.ByNative("n-4"); ok {
		t.Error("another store finds a withdrawn marker")
	}
	if err := os.Remove(filepath.Join(scratch, markersDir, storeFile)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.Lookup("in-2"); err == nil || ok {
		t.Errorf("Lookup in a store that lost its sentinel = %v %v, want an error", ok, err)
	}
	if err := m.Write(Marker{InputID: "in-3", Native: "n-3", SessionID: "s"}); err == nil {
		t.Error("a marker written to a store that is not intact")
	}
}

// Recover from a store that is not intact says unknown, never not_found.
func TestRecoverWithoutAnIntactStore(t *testing.T) {
	scratch := t.TempDir()
	m, _ := OpenMarkers(scratch)
	r := newRecord(&stubReader{}, m, "s")
	if got, err := r.Recover(context.Background(), "in-1"); err != nil || got.Outcome != contract.RecoveredNotFound {
		t.Fatalf("Recover = %+v %v, want not_found", got, err)
	}
	_ = os.RemoveAll(filepath.Join(scratch, markersDir))
	if got, err := r.Recover(context.Background(), "in-1"); err != nil || got.Outcome != contract.RecoveredUnknown {
		t.Errorf("Recover with the store gone = %+v %v, want unknown", got, err)
	}
}

// stubReader returns its chunks one at a time, and records commits.
type stubReader struct {
	chunks    []Chunk
	committed []any
}

func (r *stubReader) Read(context.Context, int) (Chunk, error) {
	if len(r.chunks) == 0 {
		return Chunk{}, nil
	}
	return r.chunks[0], nil
}

func (r *stubReader) Commit(c Chunk) error {
	r.committed = append(r.committed, c.Token)
	r.chunks = r.chunks[1:]
	return nil
}

func (r *stubReader) Recover(context.Context, Marker) (contract.Recovered, error) {
	return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
}
func (r *stubReader) Close() error { return nil }

func textObs(key string, n int) contract.Observation {
	return contract.NewObservation(contract.KindUserInput, key, contract.OriginRecord, time.Now(), contract.TextData{Text: strings.Repeat("x", n)})
}

// A chunk too large for one batch spans several: its checkpoint goes only
// with the batch holding its last item, and the reader moves past it only
// once that batch is acknowledged.
func TestChunkSpansBatches(t *testing.T) {
	cp := &contract.Checkpoint{Format: 1, Data: []byte(`{"offset":9}`)}
	r := &stubReader{chunks: []Chunk{{
		Items:      []contract.Observation{textObs("e1:0", 40<<10), textObs("e2:0", 40<<10), textObs("e3:0", 40<<10)},
		Checkpoint: cp, Faults: []contract.Fault{{Kind: "unreadable"}}, Token: "c1",
	}}}
	c := newCursor(r, true)
	ctx := context.Background()
	var got []contract.Batch
	for i := 0; i < 3; i++ {
		b, err := c.observe(ctx, 0, contract.MinObserveBytes)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Items) != 1 {
			t.Fatalf("batch %d has %d items, want 1", i, len(b.Items))
		}
		if i < 2 && len(r.committed) != 0 {
			t.Fatalf("the reader moved before the chunk's last batch was acknowledged")
		}
		got = append(got, b)
		if err := c.ack(b.BatchID); err != nil {
			t.Fatal(err)
		}
	}
	if got[0].Checkpoint != nil || got[1].Checkpoint != nil || got[2].Checkpoint != cp {
		t.Errorf("checkpoints %v %v %v, want only on the last batch", got[0].Checkpoint, got[1].Checkpoint, got[2].Checkpoint)
	}
	if len(got[0].Faults) != 1 || len(got[1].Faults)+len(got[2].Faults) != 0 {
		t.Errorf("faults %v %v %v, want on the first batch", got[0].Faults, got[1].Faults, got[2].Faults)
	}
	if len(r.committed) != 1 || r.committed[0] != "c1" {
		t.Errorf("committed %v, want c1", r.committed)
	}
	if b, err := c.observe(ctx, 0, contract.MinObserveBytes); err != nil || !b.EndOfRecord || b.NeedsAck() {
		t.Errorf("after the record: %+v %v, want an end-of-record poll", b, err)
	}
	if err := c.ack(got[2].BatchID); err != nil {
		t.Errorf("acking the last acknowledged batch again: %v", err)
	}
	if err := c.ack(got[1].BatchID); contract.CodeOf(err) != contract.CodeUnexpected {
		t.Errorf("acking an older batch: %v, want unexpected", err)
	}
}

// refusingTransport refuses its first refuse inputs before anything reaches
// its harness, and takes the rest.
type refusingTransport struct {
	refuse    int
	submitted []string
}

func (r *refusingTransport) SessionID() string { return "s" }

func (r *refusingTransport) Submit(_ context.Context, s Submission) error {
	if r.refuse > 0 {
		r.refuse--
		return fmt.Errorf("%w: not now", ErrNotSubmitted)
	}
	r.submitted = append(r.submitted, s.InputID)
	return nil
}

func (r *refusingTransport) Interrupt(context.Context) error                       { return nil }
func (r *refusingTransport) Answer(context.Context, string, contract.Choice) error { return nil }
func (r *refusingTransport) Stop(context.Context, time.Duration) bool              { return true }

// A Submit that reached nothing withdraws the input's marker: the Host may
// send the input again under its id, and Recover finds it never submitted.
func TestNotSubmittedFreesTheInputID(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMarkers(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := &refusingTransport{refuse: 1}
	s := newSession(&harnessAdapter{p: newFakeProfile(), desc: newFakeProfile().Describe()}, contract.OpenRequest{})
	s.phase, s.t, s.markers, s.sessionID = contract.PhaseIdle, tr, m, "s"
	if _, err := s.Send(ctx, contract.Text("in-1", "hello")); contract.CertaintyOf(err) != contract.NotSubmitted {
		t.Fatalf("a refused send: %v, want not_submitted", err)
	}
	if _, ok, err := m.Lookup("in-1"); err != nil || ok {
		t.Errorf("the marker of an input never submitted: %v %v, want none", ok, err)
	}
	if got, err := newRecord(&stubReader{}, m, "s").Recover(ctx, "in-1"); err != nil || got.Outcome != contract.RecoveredNotFound {
		t.Errorf("Recover of an input never submitted = %+v %v, want not_found", got, err)
	}
	res, err := s.Send(ctx, contract.Text("in-1", "hello"))
	if err != nil || res.Receipt != contract.ReceiptSubmitted || res.TurnID != TurnID("in-1") {
		t.Fatalf("the input sent again under its id: %+v %v", res, err)
	}
	if _, ok, err := m.Lookup("in-1"); err != nil || !ok {
		t.Errorf("no marker for the submitted input: %v %v", ok, err)
	}
	if len(tr.submitted) != 1 || tr.submitted[0] != "in-1" {
		t.Errorf("the harness got %v", tr.submitted)
	}
}

// A usage wall with a known reset closes the gate until then, and opens it
// with an unblocked observation.
func TestGateOpensAtResume(t *testing.T) {
	s := newSession(&harnessAdapter{p: newFakeProfile(), desc: newFakeProfile().Describe()}, contract.OpenRequest{})
	s.phase = contract.PhaseIdle
	tr := &turn{inputID: "in-1", native: "n", turnID: TurnID("in-1"), endCh: make(chan struct{})}
	s.turns["in-1"], s.byNative["n"], s.current = tr, tr, tr
	s.report(Event{Kind: Started, Native: "n"})
	at := time.Now().Add(300 * time.Millisecond)
	s.report(Event{Kind: Ended, Native: "n", Outcome: contract.TurnErrored, Error: &contract.TurnError{Class: contract.ErrorUsageLimit, ResumeAt: &at}})
	if st := s.State(); st.Phase != contract.PhaseBlocked || st.Block == nil || st.Block.Reason != contract.BlockUsageLimited {
		t.Fatalf("after a usage wall: %+v, want blocked (usage_limited)", st)
	}
	if _, err := s.Send(context.Background(), contract.Text("in-2", "PING")); contract.CodeOf(err) != contract.CodeBlocked || contract.CertaintyOf(err) != contract.NotSubmitted {
		t.Errorf("Send past the wall: %v, want blocked, not_submitted", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.State().Phase != contract.PhaseIdle && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := s.State(); st.Phase != contract.PhaseIdle || st.Block != nil {
		t.Fatalf("after the reset: %+v, want idle", st)
	}
	kinds := map[contract.Kind]bool{}
	s.cur.mu.Lock()
	for _, e := range s.cur.queue {
		kinds[e.obs.Kind] = true
	}
	s.cur.mu.Unlock()
	if !kinds[contract.KindBlocked] || !kinds[contract.KindUnblocked] {
		t.Errorf("observations %v, want blocked and unblocked", kinds)
	}
}

// Truncate cuts at a rune boundary.
func TestTruncate(t *testing.T) {
	s := strings.Repeat("é", contract.MaxObservationText) // two bytes each
	got, cut := Truncate(s)
	if !cut || len(got) > contract.MaxObservationText || !utf8.ValidString(got) {
		t.Errorf("Truncate: %d bytes, cut %v, valid %v", len(got), cut, utf8.ValidString(got))
	}
	if got, cut := Truncate("short"); cut || got != "short" {
		t.Errorf("Truncate(short) = %q %v", got, cut)
	}
}

// TurnID stays an id however long the input id.
func TestTurnIDIsAnID(t *testing.T) {
	for _, in := range []string{"a", strings.Repeat("b", 126), strings.Repeat("c", 128)} {
		if id := TurnID(in); !contract.ValidID(id) {
			t.Errorf("TurnID(%d chars) = %q, not an id", len(in), id)
		}
	}
}

// A harness takes PATH, LANG, LC_* and TZ from its Host, and the variables
// HW_HARNESS_ENV names — nothing else.
func TestHostEnv(t *testing.T) {
	t.Setenv("PATH", "/bin")
	t.Setenv("LC_ALL", "C")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("SECRET_THING", "x")
	t.Setenv(HarnessEnvVar, "ANTHROPIC_BASE_URL, ")
	got := map[string]string{}
	for _, kv := range HostEnv() {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["PATH"] != "/bin" || got["LC_ALL"] != "C" || got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:1" {
		t.Errorf("HostEnv = %v", got)
	}
	if _, ok := got["SECRET_THING"]; ok {
		t.Error("a variable HW_HARNESS_ENV does not name passed through")
	}
}
