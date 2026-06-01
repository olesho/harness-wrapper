package harness

import (
	"errors"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// fakeStream is a StreamParser returning scripted events keyed by line.
type fakeStream struct {
	byLine map[string][]transcript.ParsedEvent
}

func (f fakeStream) ParseStreamLine(line string) []transcript.ParsedEvent { return f.byLine[line] }

// fakeSessions extracts a session id from a line of the form "SID=<id>".
type fakeSessions struct{}

func (fakeSessions) ExtractSessionID(line string) (string, bool) {
	const p = "SID="
	if len(line) > len(p) && line[:len(p)] == p {
		return line[len(p):], true
	}
	return "", false
}

func liveText(sid, text string) transcript.ParsedEvent {
	return transcript.ParsedEvent{
		HarnessSessionID: sid,
		Event:            transcript.Event{Role: transcript.RoleAssistant, Type: transcript.EventText, Text: text, Source: transcript.SourceLive},
	}
}

func newTap(stream StreamParser, onEvent func(transcript.EventEnvelope) error) *streamTap {
	return &streamTap{
		harness: "claude", runID: "run-1",
		stream: stream, sessions: fakeSessions{}, onEvent: onEvent, emitLive: onEvent != nil,
	}
}

// fakeHooks is a no-op HookProvider for plan tests (planAcquisition only checks
// rp.Hooks != nil, never calling its methods).
type fakeHooks struct{}

func (fakeHooks) HookSpec() *HookSpec { return &HookSpec{} }
func (fakeHooks) ParseHookPayload(HookContext, string, []byte) ([]transcript.ParsedEvent, error) {
	return nil, nil
}
func (fakeHooks) EnsureConfig(string, []string) error { return nil }

func TestStreamTapStampsAndDelivers(t *testing.T) {
	stream := fakeStream{byLine: map[string][]transcript.ParsedEvent{
		"L1": {liveText("s1", "hello")},
		"L2": {liveText("s1", "world"), liveText("s1", "again")},
	}}
	var got []transcript.EventEnvelope
	tap := newTap(stream, func(e transcript.EventEnvelope) error { got = append(got, e); return nil })

	tap.onLine("L1")
	tap.onLine("L2")

	if len(got) != 3 {
		t.Fatalf("got %d envelopes, want 3", len(got))
	}
	for i, e := range got {
		if e.RunID != "run-1" || e.Harness != "claude" {
			t.Errorf("env %d: RunID/Harness = %q/%q", i, e.RunID, e.Harness)
		}
		if e.HarnessSessionID != "s1" {
			t.Errorf("env %d: HarnessSessionID = %q, want s1", i, e.HarnessSessionID)
		}
		if e.Event.Seq != i {
			t.Errorf("env %d: Seq = %d, want %d (monotonic across the run)", i, e.Event.Seq, i)
		}
		if e.Event.SchemaVersion != transcript.SchemaVersion {
			t.Errorf("env %d: SchemaVersion = %d, want %d", i, e.Event.SchemaVersion, transcript.SchemaVersion)
		}
	}
	if got[0].Event.Text != "hello" || got[2].Event.Text != "again" {
		t.Errorf("text/order wrong: %q ... %q", got[0].Event.Text, got[2].Event.Text)
	}
}

func TestStreamTapCapturesSessionIDAndBackfills(t *testing.T) {
	// An event whose ParsedEvent carries no session id is backfilled from the
	// id captured off an earlier system line.
	stream := fakeStream{byLine: map[string][]transcript.ParsedEvent{
		"E": {liveText("", "no-sid-on-event")},
	}}
	var got []transcript.EventEnvelope
	tap := newTap(stream, func(e transcript.EventEnvelope) error { got = append(got, e); return nil })

	tap.onLine("SID=captured-123") // session-id line, no events
	tap.onLine("E")

	if tap.sessionID != "captured-123" {
		t.Fatalf("captured session id = %q, want captured-123", tap.sessionID)
	}
	if len(got) != 1 || got[0].HarnessSessionID != "captured-123" {
		t.Fatalf("envelope session id not backfilled: %+v", got)
	}
}

func TestStreamTapAuthorityFilterDropsFileParentInStreamMode(t *testing.T) {
	// In stream mode a file-sourced PARENT event is dropped by the authority
	// filter (live is the sole parent source); a subagent file event passes.
	fileParent := transcript.ParsedEvent{Event: transcript.Event{Type: transcript.EventText, Text: "from file", Source: transcript.SourceFile}}
	subagent := transcript.ParsedEvent{ParentSessionID: "parent-x", Event: transcript.Event{Type: transcript.EventText, Text: "sub", Source: transcript.SourceFile}}
	stream := fakeStream{byLine: map[string][]transcript.ParsedEvent{"X": {fileParent, subagent}}}
	var got []transcript.EventEnvelope
	tap := newTap(stream, func(e transcript.EventEnvelope) error { got = append(got, e); return nil })

	tap.onLine("X")
	if len(got) != 1 || got[0].Event.Text != "sub" {
		t.Fatalf("authority filter wrong: got %+v, want only the subagent event", got)
	}
}

func TestStreamTapDeliveryFailureAborts(t *testing.T) {
	// On an OnEvent error the tap latches the error, cancels (aborts the run),
	// and stops emitting — subsequent events are not delivered.
	stream := fakeStream{byLine: map[string][]transcript.ParsedEvent{
		"L1": {liveText("s", "first"), liveText("s", "second")},
		"L2": {liveText("s", "third")},
	}}
	var delivered int
	boom := errors.New("store down")
	canceled := false
	tap := newTap(stream, func(transcript.EventEnvelope) error { delivered++; return boom })
	tap.cancel = func() { canceled = true }

	tap.onLine("L1")
	tap.onLine("L2") // must be inert after the failure

	if delivered != 1 {
		t.Errorf("delivered %d, want 1 (stop after first failure)", delivered)
	}
	if !errors.Is(tap.deliverErr, boom) {
		t.Errorf("deliverErr = %v, want %v", tap.deliverErr, boom)
	}
	if !canceled {
		t.Error("cancel not called — the harness would not be terminated on delivery failure")
	}
}

func TestStreamTapPanicInSinkBecomesError(t *testing.T) {
	stream := fakeStream{byLine: map[string][]transcript.ParsedEvent{"L": {liveText("s", "x")}}}
	tap := newTap(stream, func(transcript.EventEnvelope) error { panic("kaboom") })
	tap.cancel = func() {}
	tap.onLine("L")
	if tap.deliverErr == nil {
		t.Fatal("a panicking sink must become a delivery error, not crash the goroutine")
	}
}

func TestPlanAcquisition(t *testing.T) {
	stream := ResolvedProfile{Stream: fakeStream{}}
	hooksAndStream := ResolvedProfile{Stream: fakeStream{}, Hooks: fakeHooks{}}
	none := ResolvedProfile{}
	cases := []struct {
		name string
		mode Mode
		rp   ResolvedProfile
		sink bool
		want acqPlan
	}{
		{"hooks latches with shadow", TranscriptHooks, hooksAndStream, true, acqPlan{"hooks", true, false, true}},
		{"auto latches hooks with shadow", TranscriptAuto, hooksAndStream, true, acqPlan{"hooks", true, false, true}},
		{"hooks degrades to stream", TranscriptHooks, stream, true, acqPlan{"stream", false, true, false}},
		{"streamparse", TranscriptStreamParse, stream, true, acqPlan{"stream", false, true, false}},
		{"streamparse no parser", TranscriptStreamParse, none, true, acqPlan{label: "none"}},
		{"off", TranscriptOff, hooksAndStream, true, acqPlan{label: "none"}},
		{"no sink", TranscriptStreamParse, stream, false, acqPlan{label: "none"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planAcquisition(c.mode, c.rp, c.sink)
			if got != c.want {
				t.Errorf("planAcquisition = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDrainHooksEmitsFileEventsAsHooks(t *testing.T) {
	spool := t.TempDir()
	// Write a spool batch as a hook subprocess would: a session marker + parent
	// conversation events, all file-sourced.
	events := []transcript.ParsedEvent{
		{HarnessSessionID: "s", Event: transcript.Event{Type: transcript.EventSessionMeta, Source: transcript.SourceFile}},
		{HarnessSessionID: "s", Event: transcript.Event{Type: transcript.EventText, Text: "from file", Source: transcript.SourceFile}},
	}
	if err := writeSpool(spool, "stop", events); err != nil {
		t.Fatal(err)
	}
	var got []transcript.EventEnvelope
	tap := &streamTap{harness: "claude", runID: "r", onEvent: func(e transcript.EventEnvelope) error { got = append(got, e); return nil }}

	if s := tap.drainHooks(spool); s != "hooks" {
		t.Errorf("strategy = %q, want hooks (a parent event was captured)", s)
	}
	if len(got) != 2 {
		t.Fatalf("emitted %d events, want 2", len(got))
	}
	// In Hooks authority, the file PARENT event is admitted (file is authoritative).
	if got[1].Event.Text != "from file" {
		t.Errorf("parent file event not emitted: %+v", got)
	}
}

func TestDrainHooksFallsBackToShadowWhenNoHookFired(t *testing.T) {
	spool := t.TempDir() // empty: no hook fired
	tap := &streamTap{harness: "claude", runID: "r"}
	tap.onEvent = func(transcript.EventEnvelope) error { return nil }
	// Pretend the live stream was buffered during the run.
	tap.shadow = []transcript.ParsedEvent{
		{HarnessSessionID: "s", Event: transcript.Event{Type: transcript.EventText, Text: "live", Source: transcript.SourceLive}},
	}
	var got []transcript.EventEnvelope
	tap.onEvent = func(e transcript.EventEnvelope) error { got = append(got, e); return nil }

	if s := tap.drainHooks(spool); s != "stream" {
		t.Errorf("strategy = %q, want stream (fallback to buffered live)", s)
	}
	if len(got) != 1 || got[0].Event.Text != "live" {
		t.Fatalf("fallback did not flush the shadow buffer: %+v", got)
	}
}
