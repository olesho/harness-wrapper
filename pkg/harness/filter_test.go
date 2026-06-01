package harness

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func TestAdmitParentStreamParseMode(t *testing.T) {
	// StreamParse: live is the sole parent source — every live parent kind is
	// admitted; every file parent kind is dropped (file = subagents only).
	cases := []struct {
		kind string
		want bool // for source=live; file is always the negation here
	}{
		{transcript.EventText, true},
		{transcript.EventToolUse, true},
		{transcript.EventToolResult, true},
		{transcript.EventSessionMeta, true}, // session/usage also come from live here
	}
	for _, c := range cases {
		if got := admitParent(TranscriptStreamParse, transcript.SourceLive, c.kind, false); got != c.want {
			t.Errorf("StreamParse live parent %s: got %v, want %v", c.kind, got, c.want)
		}
		// File parent events are dropped in StreamParse mode (all kinds).
		if got := admitParent(TranscriptStreamParse, transcript.SourceFile, c.kind, false); got {
			t.Errorf("StreamParse file parent %s: admitted, want dropped", c.kind)
		}
	}
}

func TestAdmitParentHooksMode(t *testing.T) {
	// Hooks: the file is authoritative for the parent; live admits ONLY
	// non-conversation kinds (session/usage), dropping conversation kinds so
	// they don't double-leak alongside the file copy.
	conv := []string{transcript.EventText, transcript.EventToolUse, transcript.EventToolResult}
	for _, k := range conv {
		if !admitParent(TranscriptHooks, transcript.SourceFile, k, false) {
			t.Errorf("Hooks file parent %s: dropped, want admitted (file is authoritative)", k)
		}
		if admitParent(TranscriptHooks, transcript.SourceLive, k, false) {
			t.Errorf("Hooks live parent %s: admitted, want dropped (avoid double-leak)", k)
		}
	}
	// Non-conversation kinds from live ARE admitted in Hooks mode (session/usage).
	if !admitParent(TranscriptHooks, transcript.SourceLive, transcript.EventSessionMeta, false) {
		t.Error("Hooks live session_meta: dropped, want admitted (session/usage from live)")
	}
	// And unknown/advisory kinds from live are admitted (not conversation).
	if !admitParent(TranscriptHooks, transcript.SourceLive, "usage", false) {
		t.Error("Hooks live usage: dropped, want admitted")
	}
}

func TestAdmitParentSubagentAlwaysAdmitted(t *testing.T) {
	// Subagent events (ParentSessionID set) bypass the parent-source rule in
	// every mode and for every source/kind.
	for _, mode := range []Mode{TranscriptStreamParse, TranscriptHooks, TranscriptOff, TranscriptAuto} {
		for _, src := range []string{transcript.SourceLive, transcript.SourceFile} {
			for _, kind := range []string{transcript.EventText, transcript.EventToolUse, transcript.EventSessionMeta} {
				if !admitParent(mode, src, kind, true) {
					t.Errorf("subagent dropped: mode=%s source=%s kind=%s", mode, src, kind)
				}
			}
		}
	}
}

func TestAdmitParentOffAdmitsNothing(t *testing.T) {
	// Off (and any unrecognized mode) admits no PARENT event. (Subagents are
	// covered separately above; the orchestrator does not invoke the filter in
	// Off mode anyway, but the contract is total.)
	for _, src := range []string{transcript.SourceLive, transcript.SourceFile} {
		for _, kind := range []string{transcript.EventText, transcript.EventSessionMeta} {
			if admitParent(TranscriptOff, src, kind, false) {
				t.Errorf("Off admitted parent source=%s kind=%s, want dropped", src, kind)
			}
		}
	}
}

func TestIsParentConversationKind(t *testing.T) {
	conv := []string{transcript.EventText, transcript.EventToolUse, transcript.EventToolResult}
	for _, k := range conv {
		if !isParentConversationKind(k) {
			t.Errorf("%s should be a parent-conversation kind", k)
		}
	}
	nonConv := []string{transcript.EventSessionMeta, "usage", "system", "", "anything-unknown"}
	for _, k := range nonConv {
		if isParentConversationKind(k) {
			t.Errorf("%s should NOT be a parent-conversation kind (advisory/metadata)", k)
		}
	}
}
