package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

// postTaskPayload builds a PostToolUse[Task] hook stdin payload.
func postTaskPayload(t *testing.T, sessionID, transcriptPath, agentID string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"tool_use_id":     "task_tu",
		"tool_response":   map[string]string{"agentId": agentID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseHookPayloadPostTaskReadsSubagent(t *testing.T) {
	home := t.TempDir()
	const parentSID = "parent-sess"
	const agentID = "sub-42"
	parentPath := writeFixtureTranscript(t, home, parentSID)

	// Write the subagent transcript where Claude puts it: a per-session sidecar
	// dir named after the session FILE —
	// <projects>/<encoded-cwd>/<session-uuid>/subagents/agent-<agentId>.jsonl
	subDir := filepath.Join(strings.TrimSuffix(parentPath, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subBody := `{"type":"user","uuid":"su1","timestamp":"2026-05-14T12:00:00Z","message":{"role":"user","content":"do subtask"}}
{"type":"assistant","uuid":"sa1","timestamp":"2026-05-14T12:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"subtask done"}]}}
`
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+agentID+".jsonl"), []byte(subBody), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "post-task", postTaskPayload(t, parentSID, parentPath, agentID))
	if err != nil {
		t.Fatalf("ParseHookPayload(post-task): %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d subagent events, want 2: %+v", len(evs), evs)
	}
	for i, e := range evs {
		// Nested: the subagent's own session, parented to the spawner.
		if e.HarnessSessionID != agentID {
			t.Errorf("event %d HarnessSessionID = %q, want %q (subagent session)", i, e.HarnessSessionID, agentID)
		}
		if e.ParentSessionID != parentSID {
			t.Errorf("event %d ParentSessionID = %q, want %q", i, e.ParentSessionID, parentSID)
		}
	}
	if evs[0].Event.Text != "do subtask" || evs[1].Event.Text != "subtask done" {
		t.Errorf("subagent text/order wrong: %q, %q", evs[0].Event.Text, evs[1].Event.Text)
	}
}

func TestParseHookPayloadPostTaskNoAgentID(t *testing.T) {
	// A Task result without a spawned-subagent id yields no events (not an error).
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "post-task", postTaskPayload(t, "s", "/x", ""))
	if err != nil {
		t.Fatalf("post-task without agentId should be a no-op, got %v", err)
	}
	if evs != nil {
		t.Errorf("got %+v, want nil", evs)
	}
}

func TestParseHookPayloadPostTaskMissingSubagentFile(t *testing.T) {
	// Valid agentId but no transcript file on disk (timing) → best-effort nil.
	home := t.TempDir()
	parentPath := writeFixtureTranscript(t, home, "p")
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "post-task", postTaskPayload(t, "p", parentPath, "ghost"))
	if err != nil {
		t.Fatalf("missing subagent file should be a no-op, got %v", err)
	}
	if evs != nil {
		t.Errorf("got %+v, want nil", evs)
	}
}

func TestParseHookPayloadPostTaskRejectsTraversalID(t *testing.T) {
	home := t.TempDir()
	parentPath := writeFixtureTranscript(t, home, "p")
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	for _, bad := range []string{"../../etc/passwd", "a/b", "x.y"} {
		if _, err := (hookProvider{}).ParseHookPayload(ctx, "post-task", postTaskPayload(t, "p", parentPath, bad)); err == nil {
			t.Errorf("agentId %q should be rejected (path traversal)", bad)
		}
	}
}

// TestSubagentTranscriptPathShape pins the derivation itself: the first
// candidate is the per-session sidecar dir named after the session FILE, not
// its parent directory. This is the regression pin for the one-level-too-high
// bug that made every spawned-subagent capture silently record zero events.
func TestSubagentTranscriptPathShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		parent  string
		agentID string
		want    string
	}{
		{
			name:    "session sidecar dir",
			parent:  "/root/proj/sid.jsonl",
			agentID: "abc",
			want:    "/root/proj/sid/subagents/agent-abc.jsonl",
		},
		{
			name:    "uuid session file",
			parent:  "/h/.claude/projects/-wt/4f1c-9d.jsonl",
			agentID: "sub_42-x",
			want:    "/h/.claude/projects/-wt/4f1c-9d/subagents/agent-sub_42-x.jsonl",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := subagentTranscriptPaths(tc.parent, tc.agentID)
			if len(got) == 0 {
				t.Fatal("no candidates returned")
			}
			if got[0] != tc.want {
				t.Errorf("first candidate = %q, want %q", got[0], tc.want)
			}
		})
	}
}

func TestParseHookPayloadPostTaskLegacyLayout(t *testing.T) {
	// The pre-2.1.261 sibling-of-the-session-file location still parses.
	home := t.TempDir()
	const parentSID = "legacy-sess"
	const agentID = "sub-7"
	parentPath := writeFixtureTranscript(t, home, parentSID)

	subDir := filepath.Join(filepath.Dir(parentPath), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subBody := `{"type":"user","uuid":"lu1","timestamp":"2026-05-14T12:00:00Z","message":{"role":"user","content":"legacy subtask"}}
`
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+agentID+".jsonl"), []byte(subBody), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "post-task", postTaskPayload(t, parentSID, parentPath, agentID))
	if err != nil {
		t.Fatalf("ParseHookPayload(post-task): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d subagent events, want 1: %+v", len(evs), evs)
	}
	if evs[0].HarnessSessionID != agentID || evs[0].ParentSessionID != parentSID {
		t.Errorf("session tagging wrong: %q under %q", evs[0].HarnessSessionID, evs[0].ParentSessionID)
	}
	if evs[0].Event.Text != "legacy subtask" {
		t.Errorf("text = %q, want %q", evs[0].Event.Text, "legacy subtask")
	}
}

// writeSubagentTranscript writes a one-event subagent transcript for agentID
// into dir, whose single user turn reads text. The text is what tells the
// layouts apart in the negative controls below.
func writeSubagentTranscript(t *testing.T, dir, agentID, text string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{
		"type":      "user",
		"uuid":      "u-" + agentID,
		"timestamp": "2026-05-14T12:00:00Z",
		"message":   map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-"+agentID+".jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestParseHookPayloadPostTaskPrefersSidecarLayout is a negative control for
// the lookup ORDER: with a transcript for the same agent in both layouts, the
// current sidecar one must win, so a leftover file in the legacy location can
// never shadow the live one.
func TestParseHookPayloadPostTaskPrefersSidecarLayout(t *testing.T) {
	home := t.TempDir()
	const parentSID = "both-sess"
	const agentID = "sub-both"
	parentPath := writeFixtureTranscript(t, home, parentSID)
	writeSubagentTranscript(t, filepath.Join(strings.TrimSuffix(parentPath, ".jsonl"), "subagents"), agentID, "current layout")
	writeSubagentTranscript(t, filepath.Join(filepath.Dir(parentPath), "subagents"), agentID, "legacy layout")

	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "post-task", postTaskPayload(t, parentSID, parentPath, agentID))
	if err != nil {
		t.Fatalf("ParseHookPayload(post-task): %v", err)
	}
	if len(evs) != 1 || evs[0].Event.Text != "current layout" {
		t.Fatalf("got %+v, want the single event from the current sidecar layout", evs)
	}
}

// TestParseHookPayloadPostTaskSidecarDirWithoutFile covers a session whose
// sidecar dir exists (claude creates it for other subagents) without THIS
// agent's file: the legacy location must still be consulted.
func TestParseHookPayloadPostTaskSidecarDirWithoutFile(t *testing.T) {
	home := t.TempDir()
	const parentSID = "dir-only-sess"
	const agentID = "sub-legacy"
	parentPath := writeFixtureTranscript(t, home, parentSID)
	sidecar := filepath.Join(strings.TrimSuffix(parentPath, ".jsonl"), "subagents")
	writeSubagentTranscript(t, sidecar, "some-other-agent", "not this one")
	writeSubagentTranscript(t, filepath.Join(filepath.Dir(parentPath), "subagents"), agentID, "legacy layout")

	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "post-task", postTaskPayload(t, parentSID, parentPath, agentID))
	if err != nil {
		t.Fatalf("ParseHookPayload(post-task): %v", err)
	}
	if len(evs) != 1 || evs[0].Event.Text != "legacy layout" {
		t.Fatalf("got %+v, want the single event from the legacy layout", evs)
	}
}

// TestParseHookPayloadPostTaskSurfacesUnreadableSidecar pins the error arm: a
// read failure at the current layout that is NOT "file absent" is reported,
// never papered over by falling back to a possibly stale legacy file.
func TestParseHookPayloadPostTaskSurfacesUnreadableSidecar(t *testing.T) {
	home := t.TempDir()
	const parentSID = "unreadable-sess"
	const agentID = "sub-dir"
	parentPath := writeFixtureTranscript(t, home, parentSID)
	// A directory where the transcript file should be: os.ReadFile fails with
	// EISDIR, which is not os.IsNotExist.
	if err := os.MkdirAll(filepath.Join(strings.TrimSuffix(parentPath, ".jsonl"), "subagents", "agent-"+agentID+".jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSubagentTranscript(t, filepath.Join(filepath.Dir(parentPath), "subagents"), agentID, "stale legacy")

	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "post-task", postTaskPayload(t, parentSID, parentPath, agentID))
	if err == nil {
		t.Fatalf("got events %+v and no error; an unreadable current-layout transcript must be reported", evs)
	}
	if evs != nil {
		t.Errorf("got %+v alongside the error, want no events", evs)
	}
}
