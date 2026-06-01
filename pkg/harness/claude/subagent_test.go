package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	// Write the subagent transcript where Claude puts it:
	// <parent_dir>/subagents/agent-<agentId>.jsonl
	subDir := filepath.Join(filepath.Dir(parentPath), "subagents")
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
