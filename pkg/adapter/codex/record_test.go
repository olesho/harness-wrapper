package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

const thread = "01a0e95d-3c7a-7112-b96c-9714e6f9529f"

// rollout is a thread's rollout as codex 0.144 writes it: a turn of a
// marked input with a tool call, one sent by another host, one that failed
// (its task_complete has no reply and no error), one interrupted, and one
// cut by a crash.
const rollout = `{"timestamp":"2026-09-28T18:53:16.000Z","type":"session_meta","payload":{"id":"` + thread + `"}}
{"timestamp":"2026-09-28T18:53:17.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"2026-09-28T18:53:17.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"n1","message":"TOOL echo hi"}}
{"timestamp":"2026-09-28T18:53:17.002Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo hi\"}","call_id":"call_1"}}
{"timestamp":"2026-09-28T18:53:17.003Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"hi\n"}}
{"timestamp":"2026-09-28T18:53:17.004Z","type":"response_item","payload":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"TOOL DONE: hi"}]}}
{"timestamp":"2026-09-28T18:53:17.005Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"TOOL DONE: hi"}}
{"timestamp":"2026-09-28T18:53:18.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}
{"timestamp":"2026-09-28T18:53:18.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"elsewhere","message":"PING 2"}}
{"timestamp":"2026-09-28T18:53:18.002Z","type":"response_item","payload":{"type":"message","id":"msg_2","role":"assistant","content":[{"type":"output_text","text":"PONG 2"}]}}
{"timestamp":"2026-09-28T18:53:18.003Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t2","last_agent_message":"PONG 2"}}
{"timestamp":"2026-09-28T18:53:19.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t3"}}
{"timestamp":"2026-09-28T18:53:19.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"n3","message":"ERR 529 99"}}
{"timestamp":"2026-09-28T18:53:19.002Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t3","last_agent_message":null}}
{"timestamp":"2026-09-28T18:53:20.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t4"}}
{"timestamp":"2026-09-28T18:53:20.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"n4","message":"SLOW 60"}}
{"timestamp":"2026-09-28T18:53:20.002Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t4","reason":"interrupted"}}
{"timestamp":"2026-09-28T18:53:21.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t5"}}
{"timestamp":"2026-09-28T18:53:21.001Z","type":"event_msg","payload":{"type":"user_message","client_id":"n5","message":"STALL 30"}}
`

// recordEnv is a CODEX_HOME, a scratch root with the inputs' markers, and
// the record source over them.
type recordEnv struct {
	home, scratch string
	markers       *adapter.Markers
	src           adapter.RecordSource
}

func newRecordEnv(t *testing.T) *recordEnv {
	t.Helper()
	e := &recordEnv{home: t.TempDir(), scratch: t.TempDir()}
	m, err := adapter.OpenMarkers(e.scratch)
	if err != nil {
		t.Fatal(err)
	}
	for in, native := range map[string]string{"in_1": "n1", "in_3": "n3", "in_4": "n4", "in_5": "n5", "in_6": "n6"} {
		if err := m.Write(adapter.Marker{InputID: in, Native: native, SessionID: thread, At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	oc, _ := json.Marshal(openConfig{Binary: "/nowhere/codex", WorkingDir: "/w", CodexHome: e.home})
	e.markers = m
	e.src = adapter.RecordSource{SessionID: thread, OpenConfig: oc, Markers: m}
	return e
}

func (e *recordEnv) write(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(e.home, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-28T18-53-16-"+thread+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readAll reads and commits every chunk, and returns the items and the last
// checkpoint.
func readAll(t *testing.T, r adapter.Reader) ([]contract.Observation, *contract.Checkpoint) {
	t.Helper()
	var items []contract.Observation
	var cp *contract.Checkpoint
	for {
		ch, err := r.Read(context.Background(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(ch.Items) == 0 && ch.Checkpoint == nil && ch.Rescan == nil {
			return items, cp
		}
		items = append(items, ch.Items...)
		if ch.Checkpoint != nil {
			cp = ch.Checkpoint
		}
		if err := r.Commit(ch); err != nil {
			t.Fatal(err)
		}
	}
}

func summary(items []contract.Observation) []string {
	var out []string
	for _, o := range items {
		s := o.ID + " " + o.InputID
		if o.Kind == contract.KindTurnEnded {
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			s += " " + string(d.Outcome) + " " + d.Text
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func TestReadRecord(t *testing.T) {
	e := newRecordEnv(t)
	r, err := Profile{}.Record(e.src)
	if err != nil {
		t.Fatal(err)
	}
	// Before codex writes its first turn there is nothing to read.
	if items, _ := readAll(t, r); len(items) != 0 {
		t.Fatalf("items before the rollout exists: %v", summary(items))
	}
	e.write(t, rollout)
	items, cp := readAll(t, r)
	want := []string{
		"user_input:u:n1 in_1",
		"tool_use:call_1 in_1",
		"tool_result:call_1 in_1",
		"assistant_text:msg_1:0 in_1",
		"turn_ended:in_1 in_1 completed TOOL DONE: hi",
		"user_input:u:elsewhere",
		"assistant_text:msg_2:0",
		"user_input:u:n3 in_3",
		"user_input:u:n4 in_4",
		"turn_ended:in_4 in_4 interrupted",
		"user_input:u:n5 in_5",
	}
	if got := summary(items); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if cp == nil || cp.Format != CheckpointFormat {
		t.Fatalf("checkpoint %+v", cp)
	}

	// Reopened at the checkpoint, the reader delivers only what codex wrote
	// since, stamped with the turn it is in.
	e.write(t, rollout+`{"timestamp":"2026-09-28T18:53:22.000Z","type":"response_item","payload":{"type":"message","id":"msg_5","role":"assistant","content":[{"type":"output_text","text":"after stall"}]}}
{"timestamp":"2026-09-28T18:53:22.001Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t5","last_agent_message":"after stall"}}
`)
	src := e.src
	src.Checkpoint = cp
	r2, err := Profile{}.Record(src)
	if err != nil {
		t.Fatal(err)
	}
	items, _ = readAll(t, r2)
	want = []string{"assistant_text:msg_5:0 in_5", "turn_ended:in_5 in_5 completed after stall"}
	if got := summary(items); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("after the checkpoint:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A checkpoint the reader cannot read makes it read from the start, with a
// rescan and the same ids.
func TestRecordRescan(t *testing.T) {
	e := newRecordEnv(t)
	e.write(t, rollout)
	r, _ := Profile{}.Record(e.src)
	first, _ := readAll(t, r)
	src := e.src
	src.Checkpoint = &contract.Checkpoint{Format: 9999, Data: []byte("x")}
	r2, err := Profile{}.Record(src)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := r2.Read(context.Background(), 1<<20)
	if err != nil || ch.Rescan == nil {
		t.Fatalf("first read: %+v %v, want a rescan", ch.Rescan, err)
	}
	if strings.Join(summary(ch.Items), ",") != strings.Join(summary(first), ",") {
		t.Errorf("the rescan's ids differ:\n%v\n%v", summary(ch.Items), summary(first))
	}
}

// Recover answers from the rollout: an outcome where the turn's end proves
// one, unknown where it cannot.
func TestRecoverFromRecord(t *testing.T) {
	e := newRecordEnv(t)
	e.write(t, rollout+`{"timestamp":"2026-09-28T19:03:30.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t6"}}
{"timestamp":"2026-09-28T19:03:30.001Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t6","item":{"type":"UserMessage","client_id":"n6","content":[{"type":"text","text":"LIMIT"}]}}}
{"timestamp":"2026-09-28T19:03:30.003Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t6","last_agent_message":null,"error":{"message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}}
`)
	r, err := Profile{}.Record(e.src)
	if err != nil {
		t.Fatal(err)
	}
	for native, want := range map[string]contract.RecoveredOutcome{
		"n1":    contract.RecoveredCompleted,
		"n3":    contract.RecoveredUnknown, // failed in codex 0.144: no error recorded
		"n4":    contract.RecoveredInterrupted,
		"n5":    contract.RecoveredUnknown, // cut by a crash: no end
		"n6":    contract.RecoveredErrored,
		"never": contract.RecoveredUnknown,
	} {
		got, err := r.Recover(context.Background(), adapter.Marker{InputID: "x", Native: native, SessionID: thread})
		if err != nil || got.Outcome != want {
			t.Errorf("Recover(%s) = %+v %v, want %s", native, got, err, want)
		}
	}
}

// goalRollout is a thread whose goal made codex start turns itself, as codex
// 0.144 writes them: one that completed; one stopped before codex wrote
// anything of it; one that took an input in, which codex folds into a turn
// that runs; and one cut by a crash.
const goalRollout = `{"timestamp":"2026-09-30T15:00:00.000Z","type":"session_meta","payload":{"id":"` + thread + `"}}
{"timestamp":"2026-09-30T15:00:01.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"g1"}}
{"timestamp":"2026-09-30T15:00:01.001Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<codex_internal_context source=\"goal\">\nContinue working toward the active thread goal.\n</codex_internal_context>"}],"internal_chat_message_metadata_passthrough":{"turn_id":"g1"}}}
{"timestamp":"2026-09-30T15:00:01.002Z","type":"response_item","payload":{"type":"message","id":"msg_g1","role":"assistant","content":[{"type":"output_text","text":"goal step 1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"g1"}}}
{"timestamp":"2026-09-30T15:00:01.003Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"g1","last_agent_message":"goal step 1"}}
{"timestamp":"2026-09-30T15:00:02.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"g2"}}
{"timestamp":"2026-09-30T15:00:02.001Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"g2","reason":"interrupted"}}
{"timestamp":"2026-09-30T15:00:03.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"g3"}}
{"timestamp":"2026-09-30T15:00:03.001Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<codex_internal_context source=\"goal\">\nContinue working toward the active thread goal.\n</codex_internal_context>"}],"internal_chat_message_metadata_passthrough":{"turn_id":"g3"}}}
{"timestamp":"2026-09-30T15:00:03.002Z","type":"response_item","payload":{"type":"message","id":"msg_g3a","role":"assistant","content":[{"type":"output_text","text":"working"}],"internal_chat_message_metadata_passthrough":{"turn_id":"g3"}}}
{"timestamp":"2026-09-30T15:00:03.003Z","type":"event_msg","payload":{"type":"user_message","client_id":"n6","message":"PING 6"}}
{"timestamp":"2026-09-30T15:00:03.004Z","type":"response_item","payload":{"type":"message","id":"msg_g3b","role":"assistant","content":[{"type":"output_text","text":"PONG 6"}],"internal_chat_message_metadata_passthrough":{"turn_id":"g3"}}}
{"timestamp":"2026-09-30T15:00:03.005Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"g3","last_agent_message":"PONG 6"}}
{"timestamp":"2026-09-30T15:00:04.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"g4"}}
{"timestamp":"2026-09-30T15:00:04.001Z","type":"response_item","payload":{"type":"function_call","name":"update_goal","arguments":"{\"status\":\"complete\"}","call_id":"call_g4","internal_chat_message_metadata_passthrough":{"turn_id":"g4"}}}
`

// The turns codex started itself are read as its own: their items and their
// ends name the turn and no input, under the ids the live Session gave them.
// A turn that took an input in ends there, and the rest is the input's.
func TestReadRecordOfOwnTurns(t *testing.T) {
	e := newRecordEnv(t)
	e.write(t, goalRollout)
	r, err := Profile{}.Record(e.src)
	if err != nil {
		t.Fatal(err)
	}
	items, cp := readAll(t, r)
	var got []string
	for _, o := range items {
		s := o.ID + " turn=" + o.TurnID + " input=" + o.InputID
		if o.Kind == contract.KindTurnEnded {
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			s += " " + string(d.Outcome) + " " + d.Text
		}
		got = append(got, strings.TrimSpace(s))
	}
	want := []string{
		"assistant_text:msg_g1:0 turn=a_g1 input=",
		"turn_ended:a_g1 turn=a_g1 input= completed goal step 1",
		"turn_ended:a_g2 turn=a_g2 input= interrupted",
		"assistant_text:msg_g3a:0 turn=a_g3 input=",
		"turn_ended:a_g3 turn=a_g3 input= interrupted",
		"user_input:u:n6 turn=t_in_6 input=in_6",
		"assistant_text:msg_g3b:0 turn=t_in_6 input=in_6",
		"turn_ended:in_6 turn=t_in_6 input=in_6 completed PONG 6",
		"tool_use:call_g4 turn=a_g4 input=",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if adapter.AutoTurnID("g1") != "a_g1" {
		t.Errorf("AutoTurnID(g1) = %s", adapter.AutoTurnID("g1"))
	}

	// Reopened at the checkpoint, inside codex's own turn, the reader still
	// knows whose turn it is in.
	e.write(t, goalRollout+`{"timestamp":"2026-09-30T15:00:04.002Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_g4","output":"{}"}}
{"timestamp":"2026-09-30T15:00:04.003Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"g4","last_agent_message":"done"}}
`)
	src := e.src
	src.Checkpoint = cp
	r2, err := Profile{}.Record(src)
	if err != nil {
		t.Fatal(err)
	}
	items, _ = readAll(t, r2)
	if got := summary(items); strings.Join(got, "\n") != "tool_result:call_g4\nturn_ended:a_g4  completed done" || items[0].TurnID != "a_g4" || items[1].TurnID != "a_g4" {
		t.Errorf("after the checkpoint: %q, turns %s %s", got, items[0].TurnID, items[1].TurnID)
	}
	if got, err := r2.Recover(context.Background(), adapter.Marker{InputID: "in_6", Native: "n6", SessionID: thread}); err != nil || got.Outcome != contract.RecoveredCompleted {
		t.Errorf("Recover of the input codex folded into its own turn = %+v %v, want completed", got, err)
	}
}
