package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// writeFixtureTranscript creates a Claude on-disk transcript under
// <home>/.claude/projects/<enc>/<sessionID>.jsonl and returns its path.
func writeFixtureTranscript(t *testing.T, home, sessionID string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","uuid":"u1","timestamp":"2026-05-14T12:00:00Z","message":{"role":"user","content":"hi"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-05-14T12:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}}
`
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func payload(t *testing.T, sessionID, transcriptPath string) []byte {
	t.Helper()
	b, err := json.Marshal(claudeHookPayload{SessionID: sessionID, TranscriptPath: transcriptPath})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseHookPayloadStopReadsTranscript(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-abc"
	tpath := writeFixtureTranscript(t, home, sid)
	ctx := harness.HookContext{Home: home, Cwd: "/some/worktree"}

	evs, err := hookProvider{}.ParseHookPayload(ctx, "stop", payload(t, sid, tpath))
	if err != nil {
		t.Fatalf("ParseHookPayload(stop): %v", err)
	}
	// user "hi" + assistant "hello" + tool_use Bash = 3 events, all tagged with
	// the session and file-sourced.
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evs), evs)
	}
	for i, e := range evs {
		if e.HarnessSessionID != sid {
			t.Errorf("event %d HarnessSessionID = %q, want %q", i, e.HarnessSessionID, sid)
		}
		if e.Event.Source != transcript.SourceFile {
			t.Errorf("event %d Source = %q, want file", i, e.Event.Source)
		}
	}
	if evs[0].Event.Text != "hi" || evs[1].Event.Text != "hello" {
		t.Errorf("text/order wrong: %q, %q", evs[0].Event.Text, evs[1].Event.Text)
	}
	if evs[2].Event.Type != transcript.EventToolUse || evs[2].Event.ToolName != "Bash" {
		t.Errorf("event 2 = %+v, want tool_use Bash", evs[2].Event)
	}
}

func TestParseHookPayloadSessionStartEmitsMarker(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	for _, ev := range []string{"session-start", "user-prompt-submit"} {
		evs, err := hookProvider{}.ParseHookPayload(ctx, ev, payload(t, "sid-9", "/ignored/for/marker"))
		if err != nil {
			t.Fatalf("ParseHookPayload(%s): %v", ev, err)
		}
		if len(evs) != 1 || evs[0].Event.Type != transcript.EventSessionMeta || evs[0].HarnessSessionID != "sid-9" {
			t.Fatalf("%s: got %+v, want one session marker for sid-9", ev, evs)
		}
	}
}

func TestParseHookPayloadSubagentAndYieldNoParentEvents(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	for _, ev := range []string{"pre-task", "post-task", "yield-guard"} {
		evs, err := hookProvider{}.ParseHookPayload(ctx, ev, payload(t, "s", "/x"))
		if err != nil {
			t.Fatalf("ParseHookPayload(%s): %v", ev, err)
		}
		if evs != nil {
			t.Errorf("%s: got %+v, want nil (no parent transcript here)", ev, evs)
		}
	}
}

func TestParseHookPayloadRejectsPathOutsideRoot(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-x"
	// A path with the right basename but OUTSIDE the transcript root must be
	// rejected (defends against a malicious/garbled payload).
	evil := filepath.Join(t.TempDir(), sid+".jsonl")
	if err := os.WriteFile(evil, []byte(`{"type":"user","message":{"content":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "stop", payload(t, sid, evil)); err == nil {
		t.Fatal("expected error for transcript_path outside the harness root")
	}
}

func TestParseHookPayloadRejectsSessionMismatch(t *testing.T) {
	home := t.TempDir()
	// File exists under the root, but its basename != the payload's session id.
	tpath := writeFixtureTranscript(t, home, "real-session")
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "stop", payload(t, "different-session", tpath)); err == nil {
		t.Fatal("expected error when transcript basename != session id")
	}
}

func TestParseHookPayloadErrors(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "stop", []byte("not json")); err == nil {
		t.Error("expected parse error for malformed stdin")
	}
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "stop", payload(t, "", "/x")); err == nil {
		t.Error("expected error for empty session_id")
	}
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "bogus-event", payload(t, "s", "/x")); err == nil {
		t.Error("expected error for unknown event")
	}
}

func TestHookSpecShape(t *testing.T) {
	spec := hookProvider{}.HookSpec()
	if spec == nil {
		t.Fatal("HookSpec() = nil")
	}
	if spec.ConfigPath != filepath.Join(".claude", "settings.json") {
		t.Errorf("ConfigPath = %q", spec.ConfigPath)
	}
	if spec.Owner != "loom" {
		t.Errorf("Owner = %q, want loom", spec.Owner)
	}
	if len(spec.Events) != 6 {
		t.Fatalf("got %d events, want 6", len(spec.Events))
	}
	// The subagent hooks are Task-matched; the others match all.
	byArg := map[string]harness.HookEntry{}
	for _, e := range spec.Events {
		byArg[e.Arg] = e
	}
	if byArg["pre-task"].Matcher != "Task" || byArg["post-task"].Matcher != "Task" {
		t.Error("pre-task/post-task should be Task-matched")
	}
	if byArg["session-start"].NativeEvent != "SessionStart" {
		t.Errorf("session-start NativeEvent = %q", byArg["session-start"].NativeEvent)
	}
	if spec.Yield == nil || spec.Yield.NativeEvent != "PreToolUse" || spec.Yield.Arg != "yield-guard" {
		t.Errorf("Yield = %+v, want PreToolUse/yield-guard", spec.Yield)
	}
}

func TestResolvePopulatesHooks(t *testing.T) {
	rp := Profile{}.Resolve(harness.ResolveContext{})
	if rp.Hooks == nil {
		t.Fatal("Resolve: Hooks capability must be non-nil for claude")
	}
	if rp.Hooks.HookSpec() == nil {
		t.Fatal("resolved Hooks.HookSpec() = nil")
	}
}
