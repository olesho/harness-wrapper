package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSubagentHooksSpoolRoundTrip: claude's SubagentStart and SubagentStop,
// handled the way a consumer's hook command handles them, spool the start
// marker, and the subagent's transcript followed by the stop marker, in files
// named after their arguments; ReadSpool returns them intact and AckSpool
// clears them. An armed resume guard keeps them: they are subagent events.
func TestSubagentHooksSpoolRoundTrip(t *testing.T) {
	const parentSID, agentID = "parent-sess", "a85b6d07d12462a17"
	home, spool := t.TempDir(), t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "proj")
	parent := filepath.Join(proj, parentSID+".jsonl")
	sub := filepath.Join(proj, parentSID, "subagents", "agent-"+agentID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","uuid":"sa1","timestamp":"2026-05-14T12:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"42"}]}}` + "\n"
	if err := os.WriteFile(sub, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		harness.EnvSpool + "=" + spool, harness.EnvHome + "=" + home, harness.EnvHookCwd + "=/wt",
		harness.EnvHarnessSessionID + "=" + parentSID, // the resume guard, armed
	}
	common := map[string]any{"session_id": parentSID, "transcript_path": parent, "agent_id": agentID, "agent_type": "general-purpose"}
	with := func(extra map[string]any) []byte {
		m := map[string]any{}
		for k, v := range common {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return []byte(mustJSON(t, m))
	}
	if _, err := harness.HandleHookEvent("claude", harness.HookArgSubagentStart, env, with(map[string]any{"hook_event_name": "SubagentStart"})); err != nil {
		t.Fatalf("subagent-start: %v", err)
	}
	stop := with(map[string]any{"hook_event_name": "SubagentStop", "agent_transcript_path": sub, "last_assistant_message": "42"})
	if _, err := harness.HandleHookEvent("claude", harness.HookArgSubagentStop, env, stop); err != nil {
		t.Fatalf("subagent-stop: %v", err)
	}

	got, err := harness.ReadSpool(spool)
	if err != nil {
		t.Fatal(err)
	}
	var start, marker *transcript.ParsedEvent
	var transcriptEvents int
	for _, b := range got.Batches {
		for i := range b.Events {
			pe := b.Events[i]
			if pe.HarnessSessionID != agentID || pe.ParentSessionID != parentSID {
				t.Errorf("%s: sessions %q under %q", b.Receipt.Name, pe.HarnessSessionID, pe.ParentSessionID)
			}
			switch {
			case strings.HasPrefix(b.Receipt.Name, harness.HookArgSubagentStart+"-"):
				start = &pe
			case strings.HasPrefix(b.Receipt.Name, harness.HookArgSubagentStop+"-") && pe.Event.Type == transcript.EventSubagentStop:
				marker = &pe
			case strings.HasPrefix(b.Receipt.Name, harness.HookArgSubagentStop+"-"):
				transcriptEvents++
				if pe.Event.Text != "42" || pe.Event.Source == transcript.SourceHook || i != 0 {
					t.Errorf("subagent transcript event %d = %+v, want the record on disk ahead of the marker", i, pe.Event)
				}
			}
		}
	}
	if start == nil || start.Event.Type != transcript.EventSubagentStart || start.Event.Source != transcript.SourceHook ||
		start.Event.AgentType != "general-purpose" || start.Event.NativeID != "hook:subagent-start:"+agentID {
		t.Errorf("start = %+v", start)
	}
	if marker == nil || marker.Event.Source != transcript.SourceHook || marker.Event.AgentType != "general-purpose" || marker.Event.Text != "42" {
		t.Errorf("stop marker = %+v", marker)
	}
	if transcriptEvents != 1 {
		t.Errorf("%d subagent transcript events spooled at SubagentStop, want the one record on disk", transcriptEvents)
	}
	var receipts []harness.SpoolReceipt
	for _, b := range got.Batches {
		receipts = append(receipts, b.Receipt)
	}
	if err := harness.AckSpool(spool, receipts...); err != nil {
		t.Fatal(err)
	}
	if left, _ := harness.ReadSpool(spool); len(left.Batches) != 0 {
		t.Errorf("%d batches left after the ack", len(left.Batches))
	}
}
