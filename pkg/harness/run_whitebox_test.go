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
		harness: "claude", runID: "run-1", mode: TranscriptStreamParse,
		stream: stream, sessions: fakeSessions{}, onEvent: onEvent, active: onEvent != nil,
	}
}

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

func TestResolveStrategy(t *testing.T) {
	withStream := ResolvedProfile{Stream: fakeStream{}}
	noStream := ResolvedProfile{}
	cases := []struct {
		mode          Mode
		rp            ResolvedProfile
		wantEffective Mode
		wantLabel     string
	}{
		{TranscriptStreamParse, withStream, TranscriptStreamParse, "stream"},
		{TranscriptHooks, withStream, TranscriptStreamParse, "stream"}, // hooks→stream floor (P3c pending)
		{TranscriptAuto, withStream, TranscriptStreamParse, "stream"},
		{TranscriptStreamParse, noStream, TranscriptOff, "none"}, // no parser → none
		{TranscriptOff, withStream, TranscriptOff, "none"},
	}
	for _, c := range cases {
		eff, label := resolveStrategy(c.mode, c.rp)
		if eff != c.wantEffective || label != c.wantLabel {
			t.Errorf("resolveStrategy(%s, stream=%v) = (%s,%q), want (%s,%q)",
				c.mode, c.rp.Stream != nil, eff, label, c.wantEffective, c.wantLabel)
		}
	}
}
