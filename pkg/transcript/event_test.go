package transcript

import (
	"testing"
	"time"
)

func TestEventID_ParserOwnedNativeIDWins(t *testing.T) {
	e := Event{NativeID: "tool-result:abc", UUID: "u", ToolUseID: "abc", Type: EventToolResult}
	if got := e.ID(); got != "tool-result:abc" {
		t.Fatalf("ID = %q, want parser-owned NativeID", got)
	}
}

func TestEventID_KindQualifiedDistinctness(t *testing.T) {
	// A tool-use and its tool-result share ToolUseID but MUST get distinct ids.
	use := Event{Type: EventToolUse, ToolUseID: "t1"}
	res := Event{Type: EventToolResult, ToolUseID: "t1"}
	if use.ID() == res.ID() {
		t.Fatalf("tool-use and tool-result collapsed to the same ID %q", use.ID())
	}
}

func TestEventID_StableAcrossSeqAndArrival(t *testing.T) {
	// Same logical event parsed from two sources with different Seq / arrival
	// must yield the same ID (Seq is parser-local and must not affect identity).
	ts := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	live := Event{Seq: 3, Role: RoleAssistant, Type: EventText, Text: "hello", Timestamp: ts, Source: SourceLive}
	file := Event{Seq: 99, Role: RoleAssistant, Type: EventText, Text: "hello", Timestamp: ts, Source: SourceFile}
	if live.ID() != file.ID() {
		t.Fatalf("ID differs across Seq/Source: live=%q file=%q", live.ID(), file.ID())
	}
}

func TestEventID_MessageUUIDPrefixed(t *testing.T) {
	e := Event{UUID: "uuid-1", Role: RoleUser, Type: EventText}
	if got := e.ID(); got != "msg:uuid-1" {
		t.Fatalf("ID = %q, want msg:uuid-1", got)
	}
}

func TestParsedEvent_Envelope(t *testing.T) {
	pe := ParsedEvent{HarnessSessionID: "s1", ParentSessionID: "p1", Event: Event{Text: "x"}}
	env := pe.Envelope("run-7", "claude")
	if env.RunID != "run-7" || env.Harness != "claude" || env.HarnessSessionID != "s1" || env.ParentSessionID != "p1" {
		t.Fatalf("Envelope routing fields wrong: %+v", env)
	}
	if env.Event.Text != "x" {
		t.Fatalf("Envelope dropped the event")
	}
}

func TestTurnsFromEvents_DropsToolOnlyEvents(t *testing.T) {
	ts := time.Now()
	events := []Event{
		{Role: RoleUser, Type: EventText, Text: "hi", Timestamp: ts},
		{Role: RoleAssistant, Type: EventToolUse, ToolName: "Bash", ToolUseID: "t1"}, // no Text → dropped
		{Role: RoleAssistant, Type: EventText, Text: "done", Timestamp: ts},
	}
	turns := TurnsFromEvents(events)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (tool-only dropped)", len(turns))
	}
	if turns[0].Role != RoleUser || turns[0].Text != "hi" || turns[1].Text != "done" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
}
