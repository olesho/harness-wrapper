package harness

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// parentEvent builds a PARENT-conversation ParsedEvent tagged with sid (empty
// ParentSessionID), as readParentTranscript / sessionMarker produce.
func parentEvent(sid, text string) transcript.ParsedEvent {
	return transcript.ParsedEvent{
		HarnessSessionID: sid,
		Event:            transcript.Event{Type: transcript.EventText, Text: text},
	}
}

// subagentEvent builds a SUBAGENT ParsedEvent as readSubagentTranscript
// produces: HarnessSessionID is the subagent's OWN native agentID, and
// ParentSessionID is the fired/parent session id.
func subagentEvent(agentID, parentSID, text string) transcript.ParsedEvent {
	return transcript.ParsedEvent{
		HarnessSessionID: agentID,
		ParentSessionID:  parentSID,
		Event:            transcript.Event{Type: transcript.EventText, Text: text},
	}
}

func texts(evs []transcript.ParsedEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Event.Text
	}
	return out
}

// (a) Empty expected id ⇒ the guard is DISARMED: every parsed event survives.
// This is the fresh-session case (Claude forces no --session-id, so the expected
// id is empty) and every non-resume / codex launch.
func TestFilterResumeSessionDisarmedWhenExpectedEmpty(t *testing.T) {
	in := []transcript.ParsedEvent{
		parentEvent("sess-A", "p1"),
		parentEvent("sess-B", "p2"), // a "different" id, but with no expected id it must NOT be dropped
		subagentEvent("agent-1", "sess-A", "s1"),
	}
	got := filterResumeSession("", in)
	if len(got) != 3 {
		t.Fatalf("disarmed guard dropped events: got %v, want all 3", texts(got))
	}
}

// (b) A matching parent id ⇒ written.
func TestFilterResumeSessionKeepsMatchingParent(t *testing.T) {
	in := []transcript.ParsedEvent{parentEvent("sess-A", "p1")}
	got := filterResumeSession("sess-A", in)
	if len(got) != 1 || got[0].Event.Text != "p1" {
		t.Fatalf("matching parent should survive, got %v", texts(got))
	}
}

// (c) A non-empty MISMATCHED parent id (ParentSessionID == "", HarnessSessionID
// differs) ⇒ dropped before the spool write.
func TestFilterResumeSessionDropsMismatchedParent(t *testing.T) {
	in := []transcript.ParsedEvent{parentEvent("stale-session", "leftover")}
	got := filterResumeSession("sess-A", in)
	if len(got) != 0 {
		t.Fatalf("stale parent event for a different session should be dropped, got %v", texts(got))
	}
}

// (d) A mixed batch of PARENT events ⇒ only the matching-id ones survive.
func TestFilterResumeSessionMixedParentBatch(t *testing.T) {
	in := []transcript.ParsedEvent{
		parentEvent("sess-A", "keep1"),
		parentEvent("stale", "drop1"),
		parentEvent("sess-A", "keep2"),
		parentEvent("other", "drop2"),
	}
	got := filterResumeSession("sess-A", in)
	want := []string{"keep1", "keep2"}
	if len(got) != len(want) {
		t.Fatalf("mixed batch: got %v, want %v", texts(got), want)
	}
	for i, w := range want {
		if got[i].Event.Text != w || got[i].HarnessSessionID != "sess-A" {
			t.Fatalf("mixed batch element %d = %+v, want text %q id sess-A", i, got[i], w)
		}
	}
}

// (e) REQUIRED — a subagent event (ParentSessionID = the expected parent id,
// HarnessSessionID = a DIFFERENT agentID) on a resume launch SURVIVES. This
// proves the predicate does not regress nested capture: a naive
// HarnessSessionID == expected filter would drop it (agentID != parent id).
func TestFilterResumeSessionKeepsSubagentOnResume(t *testing.T) {
	in := []transcript.ParsedEvent{
		parentEvent("sess-A", "parent"),
		subagentEvent("agent-xyz", "sess-A", "nested"), // agentID differs from expected parent id
	}
	got := filterResumeSession("sess-A", in)
	if len(got) != 2 {
		t.Fatalf("subagent event must survive on resume, got %v want [parent nested]", texts(got))
	}
	if got[1].Event.Text != "nested" || got[1].HarnessSessionID != "agent-xyz" || got[1].ParentSessionID != "sess-A" {
		t.Fatalf("subagent event mangled: %+v", got[1])
	}
}

// A subagent whose PARENT id also mismatches the expected id must still survive:
// the ONLY drop condition is a parent-conversation event (ParentSessionID == "").
func TestFilterResumeSessionKeepsSubagentEvenWithMismatchedParentTag(t *testing.T) {
	in := []transcript.ParsedEvent{subagentEvent("agent-1", "some-other-parent", "nested")}
	got := filterResumeSession("sess-A", in)
	if len(got) != 1 {
		t.Fatalf("subagent event must never be dropped, got %v", texts(got))
	}
}
