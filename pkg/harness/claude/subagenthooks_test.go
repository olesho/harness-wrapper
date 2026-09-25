package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

const (
	subParent = "parent-sess"
	subAgent  = "a85b6d07d12462a17"
	subBody   = `{"type":"user","uuid":"su1","timestamp":"2026-05-14T12:00:00Z","message":{"role":"user","content":"do subtask"}}
{"type":"assistant","uuid":"sa1","timestamp":"2026-05-14T12:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"subtask done"}]}}
`
)

// subagentHookBody builds a SubagentStart / SubagentStop stdin payload as
// claude 2.1.281 sends it.
func subagentHookBody(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeNativeSubagentFixture writes the parent transcript and the subagent's
// transcript in claude's current layout, returning the subagent's path.
func writeNativeSubagentFixture(t *testing.T, home string) (parent, sub string) {
	t.Helper()
	parent = writeFixtureTranscript(t, home, subParent)
	dir := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sub = filepath.Join(dir, "agent-"+subAgent+".jsonl")
	if err := os.WriteFile(sub, []byte(subBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return parent, sub
}

func stopBody(t *testing.T, parent, sub, last string) []byte {
	return subagentHookBody(t, map[string]any{
		"session_id": subParent, "transcript_path": parent, "hook_event_name": "SubagentStop",
		"agent_id": subAgent, "agent_type": "general-purpose", "agent_transcript_path": sub,
		"last_assistant_message": last, "stop_hook_active": false,
	})
}

func TestSubagentStartMarker(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	body := subagentHookBody(t, map[string]any{
		"session_id": subParent, "transcript_path": "/x/" + subParent + ".jsonl", "hook_event_name": "SubagentStart",
		"agent_id": subAgent, "agent_type": "general-purpose", "prompt_id": "p1",
	})
	evs, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStart, body)
	if err != nil {
		t.Fatalf("subagent-start: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want the start marker: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.HarnessSessionID != subAgent || e.ParentSessionID != subParent {
		t.Errorf("sessions = %q under %q, want %q under %q", e.HarnessSessionID, e.ParentSessionID, subAgent, subParent)
	}
	want := transcript.Event{
		Timestamp: e.Event.Timestamp, Role: transcript.RoleSystem, Type: transcript.EventSubagentStart,
		AgentType: "general-purpose", Source: transcript.SourceHook, NativeID: "hook:subagent-start:" + subAgent,
	}
	if !reflect.DeepEqual(e.Event, want) || e.Event.Timestamp.IsZero() {
		t.Errorf("start marker = %+v, want %+v (stamped)", e.Event, want)
	}
}

func TestSubagentHookRefusesBadPayloads(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	for _, arg := range []string{harness.HookArgSubagentStart, harness.HookArgSubagentStop} {
		for _, id := range []string{"", "../../etc", "a/b", "x.y"} {
			body := subagentHookBody(t, map[string]any{"session_id": subParent, "agent_id": id})
			if _, err := (hookProvider{}).ParseHookPayload(ctx, arg, body); err == nil {
				t.Errorf("%s with agent_id %q was accepted", arg, id)
			}
		}
		if _, err := (hookProvider{}).ParseHookPayload(ctx, arg, []byte(`{"session_id":`)); err == nil {
			t.Errorf("%s with a malformed payload was accepted", arg)
		}
		if _, err := (hookProvider{}).ParseHookPayload(ctx, arg, subagentHookBody(t, map[string]any{"agent_id": subAgent})); err == nil {
			t.Errorf("%s without a session_id was accepted", arg)
		}
	}
}

func TestSubagentAgentTypeBounded(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	long := strings.Repeat("é", 200) // 400 bytes
	body := subagentHookBody(t, map[string]any{"session_id": subParent, "agent_id": subAgent, "agent_type": long})
	evs, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStart, body)
	if err != nil {
		t.Fatal(err)
	}
	if at := evs[0].Event.AgentType; len(at) > maxAgentTypeBytes || !utf8.ValidString(at) || !strings.HasPrefix(long, at) {
		t.Errorf("agent type %d bytes (valid UTF-8 %v), want a rune-safe cut to %d", len(at), utf8.ValidString(at), maxAgentTypeBytes)
	}
}

func TestSubagentStopLastReplyBounded(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}
	long := strings.Repeat("x", harness.MaxToolHookBytes+100)
	evs, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStop, stopBody(t, "/x/"+subParent+".jsonl", "", long))
	if err != nil {
		t.Fatal(err)
	}
	text := evs[len(evs)-1].Event.Text
	if !strings.HasSuffix(text, harness.ToolHookTruncated) || len(text) != harness.MaxToolHookBytes+len(harness.ToolHookTruncated) {
		t.Errorf("last reply %d bytes, want cut to %d plus the marker", len(text), harness.MaxToolHookBytes)
	}
}

// TestRetiredPreTaskStillAccepted: a settings.json an earlier hw wrote still
// runs pre-task until its next ensure; it stays accepted and spools nothing.
func TestRetiredPreTaskStillAccepted(t *testing.T) {
	home := t.TempDir()
	parent, _ := writeNativeSubagentFixture(t, home)
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	if evs, err := (hookProvider{}).ParseHookPayload(ctx, argPreTask, postTaskPayload(t, subParent, parent, subAgent)); err != nil || evs != nil {
		t.Errorf("pre-task = %+v, %v; want nothing", evs, err)
	}
}

func TestSubagentStopReadsTranscriptThenMarker(t *testing.T) {
	home := t.TempDir()
	parent, sub := writeNativeSubagentFixture(t, home)
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	evs, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStop, stopBody(t, parent, sub, "subtask done"))
	if err != nil {
		t.Fatalf("subagent-stop: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want the 2 transcript events and the stop marker: %+v", len(evs), evs)
	}
	if evs[0].Event.Text != "do subtask" || evs[1].Event.Text != "subtask done" {
		t.Errorf("transcript = %q, %q", evs[0].Event.Text, evs[1].Event.Text)
	}
	for i, e := range evs {
		if e.HarnessSessionID != subAgent || e.ParentSessionID != subParent {
			t.Errorf("event %d sessions = %q under %q", i, e.HarnessSessionID, e.ParentSessionID)
		}
	}
	stop := evs[2].Event
	want := transcript.Event{
		Timestamp: stop.Timestamp, Role: transcript.RoleSystem, Type: transcript.EventSubagentStop, Text: "subtask done",
		AgentType: "general-purpose", Source: transcript.SourceHook, NativeID: "hook:subagent-stop:" + subAgent,
	}
	if !reflect.DeepEqual(stop, want) {
		t.Errorf("stop marker = %+v, want %+v", stop, want)
	}
}

// TestSubagentStopMatchesPostTask: for the same subagent, subagent-stop
// yields exactly the transcript events post-task yields — same sessions, same
// fields, same ids — so a consumer that nests subagents under their parent
// (loom's Runs tab) sees the same records, and the two copies dedup.
func TestSubagentStopMatchesPostTask(t *testing.T) {
	home := t.TempDir()
	parent, sub := writeNativeSubagentFixture(t, home)
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	post, err := hookProvider{}.ParseHookPayload(ctx, argPostTask, postTaskPayload(t, subParent, parent, subAgent))
	if err != nil {
		t.Fatalf("post-task: %v", err)
	}
	stop, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStop, stopBody(t, parent, sub, "subtask done"))
	if err != nil {
		t.Fatalf("subagent-stop: %v", err)
	}
	if len(post) == 0 || !reflect.DeepEqual(post, stop[:len(stop)-1]) {
		t.Errorf("subagent-stop's transcript differs from post-task's:\npost-task: %+v\nstop:      %+v", post, stop)
	}
}

func TestSubagentStopLegacyLayout(t *testing.T) {
	home := t.TempDir()
	parent := writeFixtureTranscript(t, home, subParent)
	dir := filepath.Join(filepath.Dir(parent), "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "agent-"+subAgent+".jsonl")
	if err := os.WriteFile(sub, []byte(subBody), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, err := hookProvider{}.ParseHookPayload(harness.HookContext{Home: home, Cwd: "/wt"}, harness.HookArgSubagentStop, stopBody(t, parent, sub, ""))
	if err != nil || len(evs) != 3 {
		t.Fatalf("legacy layout: %d events, %v", len(evs), err)
	}
}

func TestSubagentStopRefusesPaths(t *testing.T) {
	home := t.TempDir()
	parent, _ := writeNativeSubagentFixture(t, home)
	proj := filepath.Dir(parent)
	root := filepath.Dir(proj)
	name := "agent-" + subAgent + ".jsonl"
	for _, tc := range []struct{ name, path string }{
		{"outside the root", "/etc/passwd"},
		{"traversal out of the root", filepath.Join(proj, subParent, "subagents") + "/../../../../x/" + name},
		{"another subagent's file", filepath.Join(proj, subParent, "subagents", "agent-other.jsonl")},
		{"not in a subagents dir", filepath.Join(proj, subParent, name)},
		{"another session's subagents", filepath.Join(proj, "other-sess", "subagents", name)},
		{"too deep", filepath.Join(proj, "x", subParent, "subagents", name)},
		{"the root itself", root},
		{"relative without a cwd", "projects/proj/" + subParent + "/subagents/" + name},
	} {
		ctx := harness.HookContext{Home: home}
		if tc.name != "relative without a cwd" {
			ctx.Cwd = "/wt"
		}
		if _, err := (hookProvider{}).ParseHookPayload(ctx, harness.HookArgSubagentStop, stopBody(t, parent, tc.path, "")); err == nil {
			t.Errorf("%s (%s) was accepted", tc.name, tc.path)
		}
	}
}

// TestSubagentStopRefusesSymlinkEscape: a subagent transcript, or its
// subagents directory, that is a symlink out of the transcript root is
// refused, not read.
func TestSubagentStopRefusesSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.jsonl")
	if err := os.WriteFile(secret, []byte(subBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("file", func(t *testing.T) {
		home := t.TempDir()
		parent, sub := writeNativeSubagentFixture(t, home)
		if err := os.Remove(sub); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, sub); err != nil {
			t.Fatal(err)
		}
		if evs, err := (hookProvider{}).ParseHookPayload(harness.HookContext{Home: home, Cwd: "/wt"}, harness.HookArgSubagentStop, stopBody(t, parent, sub, "")); err == nil {
			t.Errorf("a symlink out of the root was read: %+v", evs)
		}
	})
	t.Run("dir", func(t *testing.T) {
		home := t.TempDir()
		parent, sub := writeNativeSubagentFixture(t, home)
		dir := filepath.Dir(sub)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(secret, filepath.Join(outside, filepath.Base(sub))); err != nil {
			t.Fatal(err)
		}
		if evs, err := (hookProvider{}).ParseHookPayload(harness.HookContext{Home: home, Cwd: "/wt"}, harness.HookArgSubagentStop, stopBody(t, parent, sub, "")); err == nil {
			t.Errorf("a symlinked subagents dir out of the root was read: %+v", evs)
		}
	})
}

// TestSubagentStopWithoutTranscript: with no transcript on disk yet, or no
// path handed over, the stop is still recorded.
func TestSubagentStopWithoutTranscript(t *testing.T) {
	shortReplyWait(t)
	home := t.TempDir()
	parent := writeFixtureTranscript(t, home, subParent)
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	missing := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents", "agent-"+subAgent+".jsonl")
	for name, path := range map[string]string{"not yet on disk": missing, "no path handed over": ""} {
		evs, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStop, stopBody(t, parent, path, "late"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(evs) != 1 || evs[0].Event.Type != transcript.EventSubagentStop || evs[0].Event.Text != "late" {
			t.Errorf("%s: got %+v, want only the stop marker", name, evs)
		}
	}
}

// TestSubagentStopRelativeConfigRoot: under a relative CLAUDE_CONFIG_DIR claude
// hands over paths relative to its own cwd, the run's working dir.
func TestSubagentStopRelativeConfigRoot(t *testing.T) {
	wd := t.TempDir()
	cfg := filepath.Join(wd, "cfg")
	parentDir := filepath.Join(cfg, "projects", "proj")
	sub := filepath.Join(parentDir, subParent, "subagents", "agent-"+subAgent+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte(subBody), 0o600); err != nil {
		t.Fatal(err)
	}
	rel := func(p string) string {
		r, err := filepath.Rel(wd, p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ctx := harness.HookContext{Cwd: wd, Home: t.TempDir(), ConfigDir: "cfg"}
	body := stopBody(t, rel(filepath.Join(parentDir, subParent+".jsonl")), rel(sub), "")
	evs, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStop, body)
	if err != nil || len(evs) != 3 {
		t.Fatalf("relative config root: %d events, %v", len(evs), err)
	}
}

// shortReplyWait cuts lastReplyWait for one test.
func shortReplyWait(t *testing.T) {
	t.Helper()
	prev := lastReplyWait
	lastReplyWait = 100 * time.Millisecond
	t.Cleanup(func() { lastReplyWait = prev })
}

// TestSubagentStopWaitsForLastReply: when SubagentStop fires before claude
// has written the subagent's last reply, subagent-stop reads the file again
// until the reply is there — here an entry of two text blocks, which claude
// hands over joined by a newline and trimmed.
func TestSubagentStopWaitsForLastReply(t *testing.T) {
	home := t.TempDir()
	parent, sub := writeNativeSubagentFixture(t, home)
	const entry = `{"type":"assistant","uuid":"sa2","timestamp":"2026-05-14T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"the answer"},{"type":"text","text":"is 42\n"}]}}` + "\n"
	done := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		f, err := os.OpenFile(sub, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, err = f.WriteString(entry)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		done <- err
	}()
	start := time.Now()
	evs, err := hookProvider{}.ParseHookPayload(harness.HookContext{Home: home, Cwd: "/wt"}, harness.HookArgSubagentStop, stopBody(t, parent, sub, "the answer\nis 42"))
	elapsed := time.Since(start)
	if werr := <-done; werr != nil {
		t.Fatal(werr)
	}
	if err != nil {
		t.Fatalf("subagent-stop: %v", err)
	}
	if len(evs) != 5 || evs[2].Event.Text != "the answer" || evs[3].Event.Text != "is 42\n" {
		t.Fatalf("got %+v, want the two first records, the late reply's two blocks and the marker", evs)
	}
	if elapsed >= lastReplyWait {
		t.Errorf("took %v: it waited out lastReplyWait instead of stopping at the reply", elapsed)
	}
}

// TestSubagentStopGivesUpOnLastReply: a reply that never reaches the file
// costs at most lastReplyWait, and what the file holds is spooled; a stop
// with no reply to wait for does not wait.
func TestSubagentStopGivesUpOnLastReply(t *testing.T) {
	shortReplyWait(t)
	home := t.TempDir()
	parent, sub := writeNativeSubagentFixture(t, home)
	ctx := harness.HookContext{Home: home, Cwd: "/wt"}
	start := time.Now()
	evs, err := hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStop, stopBody(t, parent, sub, "never written"))
	if elapsed := time.Since(start); err != nil || len(evs) != 3 || elapsed < lastReplyWait || elapsed > 10*lastReplyWait {
		t.Errorf("unwritten reply: %d events, %v, after %v; want the 2 records and the marker after %v", len(evs), err, elapsed, lastReplyWait)
	}
	start = time.Now()
	evs, err = hookProvider{}.ParseHookPayload(ctx, harness.HookArgSubagentStop, stopBody(t, parent, sub, ""))
	if elapsed := time.Since(start); err != nil || len(evs) != 3 || elapsed >= lastReplyWait {
		t.Errorf("no reply: %d events, %v, after %v; want them at once", len(evs), err, elapsed)
	}
}

func TestEndsWithReply(t *testing.T) {
	ev := func(role, typ, uuid, text string) transcript.ParsedEvent {
		return transcript.ParsedEvent{Event: transcript.Event{Role: role, Type: typ, UUID: uuid, Text: text}}
	}
	a, u := transcript.RoleAssistant, transcript.RoleUser
	txt, tool := transcript.EventText, transcript.EventToolUse
	for _, tc := range []struct {
		name  string
		evs   []transcript.ParsedEvent
		reply string
		want  bool
	}{
		{"one block", []transcript.ParsedEvent{ev(u, txt, "1", "q"), ev(a, txt, "2", " 42\n")}, "42", true},
		{"blocks of one entry", []transcript.ParsedEvent{ev(a, txt, "2", "a"), ev(a, tool, "2", ""), ev(a, txt, "2", "b")}, "a\nb", true},
		{"an earlier entry", []transcript.ParsedEvent{ev(a, txt, "1", "42"), ev(a, txt, "2", "working")}, "42", false},
		{"blocks of two entries", []transcript.ParsedEvent{ev(a, txt, "1", "a"), ev(a, txt, "2", "b")}, "a\nb", false},
		{"after a user entry", []transcript.ParsedEvent{ev(a, txt, "1", "42"), ev(u, txt, "2", "ok")}, "42", true},
		{"no assistant text", []transcript.ParsedEvent{ev(u, txt, "1", "42")}, "42", false},
		{"nothing", nil, "42", false},
	} {
		if got := endsWithReply(tc.evs, tc.reply); got != tc.want {
			t.Errorf("%s: endsWithReply = %v, want %v", tc.name, got, tc.want)
		}
	}
}
