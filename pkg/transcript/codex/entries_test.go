package codex

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// rollout0144 is a thread as codex 0.144 writes it: a completed turn with a
// tool call, a turn that failed (its task_complete has no reply and no
// error), an interrupted turn, and one cut by a crash mid-turn.
const rollout0144 = `{"timestamp":"2026-09-28T18:53:16.847Z","type":"session_meta","payload":{"id":"T1","cwd":"/work"}}
{"timestamp":"2026-09-28T18:53:17.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"timestamp":"2026-09-28T18:53:17.001Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>"}]}}
{"timestamp":"2026-09-28T18:53:17.002Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"TOOL echo hi"}]}}
{"timestamp":"2026-09-28T18:53:17.003Z","type":"event_msg","payload":{"type":"user_message","client_id":"cid-1","message":"TOOL echo hi"}}
{"timestamp":"2026-09-28T18:53:17.004Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo hi\"}","call_id":"call_1"}}
{"timestamp":"2026-09-28T18:53:17.005Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"hi\n"}}
{"timestamp":"2026-09-28T18:53:17.006Z","type":"event_msg","payload":{"type":"agent_message","message":"TOOL DONE: hi"}}
{"timestamp":"2026-09-28T18:53:17.007Z","type":"response_item","payload":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"TOOL DONE: hi"}]}}
{"timestamp":"2026-09-28T18:53:17.008Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"TOOL DONE: hi"}}
{"timestamp":"2026-09-28T18:53:18.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}
{"timestamp":"2026-09-28T18:53:18.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"cid-2","message":"ERR 529 99"}}
{"timestamp":"2026-09-28T18:53:18.002Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2","last_agent_message":null}}
{"timestamp":"2026-09-28T18:53:19.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-3"}}
{"timestamp":"2026-09-28T18:53:19.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"cid-3","message":"SLOW 60"}}
{"timestamp":"2026-09-28T18:53:19.002Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-3","reason":"interrupted"}}
{"timestamp":"2026-09-28T18:53:20.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-4"}}
{"timestamp":"2026-09-28T18:53:20.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"cid-4","message":"STALL 30"}}
`

// rollout0157 is codex 0.157's form: the input on an item_completed, and a
// failed turn's error on its task_complete.
const rollout0157 = `{"timestamp":"2026-09-28T19:03:30.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-9"}}
{"timestamp":"2026-09-28T19:03:30.001Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-9","item":{"type":"UserMessage","id":"u9","client_id":"cid-9","content":[{"type":"text","text":"LIMIT"}]}}}
{"timestamp":"2026-09-28T19:03:30.002Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-9","item":{"type":"AgentMessage","id":"msg_9","content":[{"type":"Text","text":"never"}]}}}
{"timestamp":"2026-09-28T19:03:30.003Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-9","last_agent_message":null,"error":{"message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}}
{"timestamp":"2026-09-28T19:03:31.003Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-10","error":{"message":"cut","codex_error_info":{"response_stream_disconnected":{"http_status_code":null}}}}}
`

// home writes rollout as thread's under a new CODEX_HOME.
func home(t *testing.T, thread, rollout string) string {
	t.Helper()
	h := t.TempDir()
	dir := filepath.Join(h, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-28T18-53-16-"+thread+".jsonl"), []byte(rollout), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

func follow(t *testing.T, h, thread string) []transcript.FollowedEvent {
	t.Helper()
	path, err := Rollout(h, thread)
	if err != nil {
		t.Fatal(err)
	}
	f, err := FollowRollout(path, thread, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	return b.Events
}

func TestRolloutEntries0144(t *testing.T) {
	events := follow(t, home(t, "T1", rollout0144), "T1")
	var got []string
	for _, fe := range events {
		e := fe.Meta.(*Entry)
		s := e.Kind + ":" + fe.Event.Type
		switch {
		case e.ClientID != "":
			s += ":" + e.ClientID + "=" + fe.Event.Text
		case fe.Event.Type == transcript.EventToolUse:
			s += ":" + fe.Event.ToolName + ":" + fe.Event.ToolUseID + ":" + string(fe.Event.ToolInput)
		case fe.Event.Type == transcript.EventToolResult:
			s += ":" + fe.Event.ToolUseID + ":" + fe.Event.Output
		case e.MessageID != "":
			s += ":" + e.MessageID + "=" + fe.Event.Text
		case e.Kind == "task_complete" && e.LastAgentMessage != nil:
			s += ":" + e.TurnID + "=" + *e.LastAgentMessage
		case e.Kind == "turn_aborted":
			s += ":" + e.TurnID + "=" + e.AbortReason
		default:
			s += ":" + e.TurnID
		}
		got = append(got, s)
	}
	want := []string{
		"task_started:entry:turn-1",
		"user_message:text:cid-1=TOOL echo hi",
		`function_call:tool_use:exec_command:call_1:{"cmd":"echo hi"}`,
		"function_call_output:tool_result:call_1:hi\n",
		"message:text:msg_1=TOOL DONE: hi",
		"task_complete:entry:turn-1=TOOL DONE: hi",
		"task_started:entry:turn-2",
		"user_message:text:cid-2=ERR 529 99",
		"task_complete:entry:turn-2",
		"task_started:entry:turn-3",
		"user_message:text:cid-3=SLOW 60",
		"turn_aborted:entry:turn-3=interrupted",
		"task_started:entry:turn-4",
		"user_message:text:cid-4=STALL 30",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRolloutEntries0157(t *testing.T) {
	events := follow(t, home(t, "T9", rollout0157), "T9")
	if len(events) != 4 {
		t.Fatalf("%d events: %+v", len(events), events)
	}
	if e := events[1].Meta.(*Entry); e.ClientID != "cid-9" || events[1].Event.Text != "LIMIT" || events[1].Event.Role != transcript.RoleUser {
		t.Errorf("the input: %+v %+v", e, events[1].Event)
	}
	if e := events[2].Meta.(*Entry); e.Kind != "task_complete" || e.LastAgentMessage != nil || e.ErrorInfo != "usage_limit_exceeded" || e.ErrorMessage == "" {
		t.Errorf("a failed turn's end: %+v", e)
	}
	if e := events[3].Meta.(*Entry); e.ErrorInfo != "response_stream_disconnected" {
		t.Errorf("an error in object form: %+v", e)
	}
}

func TestRolloutNotYetWritten(t *testing.T) {
	h := t.TempDir()
	if _, err := Rollout(h, "T1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no sessions yet: %v, want fs.ErrNotExist", err)
	}
	h = home(t, "T1", rollout0144)
	if _, err := Rollout(h, "T2"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("another thread's rollout only: %v, want fs.ErrNotExist", err)
	}
	if _, err := Rollout(h, "../T1"); err == nil {
		t.Error("a thread id with a path separator was accepted")
	}
}
