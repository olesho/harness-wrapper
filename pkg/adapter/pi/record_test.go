package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The sessions these tests read are pi 1.0.4's own, written by probes/pirpc
// against the mock API with two retries (pkg/transcript/pi/testdata). Their
// inputs are tagged in-1, in-2, ….
const sessions = "../../transcript/pi/testdata/1.0.4"

// recordEnv is an agent dir holding one captured session, a scratch root
// with markers for its inputs, and the record source over them.
type recordEnv struct {
	session, scratch, dir string
	markers               *adapter.Markers
	src                   adapter.RecordSource
}

// newRecordEnv lays out the captured session name, with markers that make
// each tag in tags the input of the same name with "-" as "_".
func newRecordEnv(t *testing.T, name string, tags ...string) *recordEnv {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessions, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return newRecordEnvOf(t, b, tags...)
}

// newRecordEnvOf is newRecordEnv for a session's content.
func newRecordEnvOf(t *testing.T, b []byte, tags ...string) *recordEnv {
	t.Helper()
	var header struct {
		ID string `json:"id"`
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(nil, 1<<20)
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &header) != nil || header.ID == "" {
		t.Fatal("the session has no header")
	}
	e := &recordEnv{session: header.ID, scratch: t.TempDir(), dir: t.TempDir()}
	sessionDir := filepath.Join(e.dir, sessionsDir)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "2026-10-06T09-00-00-000Z_"+header.ID+".jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := adapter.OpenMarkers(e.scratch)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		if err := m.Write(adapter.Marker{InputID: strings.ReplaceAll(tag, "-", "_"), Native: tag, SessionID: header.ID, At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	oc, _ := json.Marshal(openConfig{
		Binary: "/nowhere/pi", Extension: "/nowhere/hw-tag.ts", WorkingDir: "/w",
		AgentDir: e.dir, SessionDir: sessionDir, Provider: "anthropic", MaxRetries: 2,
	})
	e.markers = m
	e.src = adapter.RecordSource{SessionID: header.ID, OpenConfig: oc, Layout: contract.Layout{Scratch: e.scratch}, Markers: m}
	return e
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

// summary is each item as its kind, its input and, for a turn's end, its
// outcome, its class and its text.
func summary(items []contract.Observation) []string {
	var out []string
	for _, o := range items {
		s := string(o.Kind) + " " + o.InputID
		if o.Kind == contract.KindTurnEnded {
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			s += " " + string(d.Outcome)
			if d.Error != nil {
				s += " " + string(d.Error.Class)
			}
			if d.Text != "" {
				s += " " + d.Text
			}
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func read(t *testing.T, e *recordEnv) []string {
	t.Helper()
	r, err := Profile{}.Record(e.src)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := readAll(t, r)
	return summary(items)
}

func same(t *testing.T, name string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n%s\nwant:\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A run with a tool: the input's user message, the tool's call and result
// (each as tool_use and tool_started, tool_result and tool_finished), the
// answer, and the run's end, all the input's.
func TestReadRecord(t *testing.T) {
	e := newRecordEnv(t, "tool", "in-1")
	same(t, "tool", read(t, e), []string{
		"user_input in_1",
		"tool_use in_1", "tool_started in_1",
		"tool_result in_1", "tool_finished in_1",
		"assistant_text in_1",
		"turn_ended in_1 completed TOOL DONE: minimal",
	})
}

// How runs end in the record: an abort mid-stream or before the first token
// interrupts; an abort mid-tool reads as a failure unless the profile noted
// that it interrupted the input; a retried failure is dropped, and the
// answer after it completes the run; an exhausted one fails it at its last
// attempt; a crash ends nothing.
func TestRecordEnds(t *testing.T) {
	same(t, "abort mid-stream", read(t, newRecordEnv(t, "abort-mid-stream", "in-1")), []string{
		"user_input in_1", "assistant_text in_1", "turn_ended in_1 interrupted",
	})
	same(t, "abort before the first token", read(t, newRecordEnv(t, "abort-before-first-token", "in-1")), []string{
		"user_input in_1", "turn_ended in_1 interrupted",
	})
	tool := []string{"user_input in_1", "tool_use in_1", "tool_started in_1", "tool_result in_1", "tool_finished in_1"}
	same(t, "abort mid-tool, unnoted", read(t, newRecordEnv(t, "abort-mid-tool", "in-1")), append(append([]string{}, tool...), "turn_ended in_1 errored internal"))
	noted := newRecordEnv(t, "abort-mid-tool", "in-1")
	if err := noteInterrupt(noted.scratch, "in-1"); err != nil {
		t.Fatal(err)
	}
	same(t, "abort mid-tool, noted", read(t, noted), append(append([]string{}, tool...), "turn_ended in_1 interrupted"))
	same(t, "retried", read(t, newRecordEnv(t, "retried", "in-1")), []string{
		"user_input in_1", "assistant_text in_1", "turn_ended in_1 completed RECOVERED",
	})
	same(t, "exhausted", read(t, newRecordEnv(t, "error-exhausted", "in-1")), []string{
		"user_input in_1", "turn_ended in_1 errored overloaded",
	})
	same(t, "crash mid-tool", read(t, newRecordEnv(t, "crash-mid-tool", "in-1")), []string{
		"user_input in_1", "tool_use in_1", "tool_started in_1",
	})
	// The crashed input's run never ends; the next input's does.
	same(t, "crash mid-stream, reopened", read(t, newRecordEnv(t, "crash-mid-stream-reopened", "in-1", "in-2")), []string{
		"user_input in_1", "user_input in_2", "assistant_text in_2", "turn_ended in_2 completed PONG 2",
	})
}

// A refused input's tag has no user message: the running input's answer is
// still its own, and the refused input has no item.
func TestRecordRefusedTag(t *testing.T) {
	same(t, "refused while busy", read(t, newRecordEnv(t, "refused-while-busy", "in-1", "in-2")), []string{
		"user_input in_1", "assistant_text in_1", "turn_ended in_1 completed " + slowAnswer(t),
	})
}

// slowAnswer is the answer of refused-while-busy's running input: SLOW 30.
func slowAnswer(*testing.T) string {
	var b strings.Builder
	for i := range 30 {
		fmt.Fprintf(&b, "slow%d ", i)
	}
	return strings.TrimSpace(b.String())
}

// Recover answers from the session file: an outcome where the run's end
// proves one, unknown where it cannot.
func TestRecoverFromRecord(t *testing.T) {
	for _, c := range []struct {
		name, tag string
		note      bool
		want      contract.RecoveredOutcome
	}{
		{"tool", "in-1", false, contract.RecoveredCompleted},
		{"abort-mid-stream", "in-1", false, contract.RecoveredInterrupted},
		{"abort-mid-tool", "in-1", false, contract.RecoveredErrored},
		{"abort-mid-tool", "in-1", true, contract.RecoveredInterrupted},
		{"error-exhausted", "in-1", false, contract.RecoveredErrored},
		{"retried", "in-1", false, contract.RecoveredCompleted},
		{"crash-mid-tool", "in-1", false, contract.RecoveredUnknown},
		{"crash-mid-stream-reopened", "in-1", false, contract.RecoveredUnknown},
		{"crash-mid-stream-reopened", "in-2", false, contract.RecoveredCompleted},
		{"refused-while-busy", "in-2", false, contract.RecoveredUnknown},
		{"refused-while-busy", "in-9", false, contract.RecoveredUnknown},
	} {
		e := newRecordEnv(t, c.name, c.tag)
		if c.note {
			if err := noteInterrupt(e.scratch, c.tag); err != nil {
				t.Fatal(err)
			}
		}
		r, err := Profile{}.Record(e.src)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Recover(context.Background(), adapter.Marker{InputID: "x", Native: c.tag, SessionID: e.session})
		if err != nil || got.Outcome != c.want {
			t.Errorf("%s %s (noted %v): %+v %v, want %s", c.name, c.tag, c.note, got, err, c.want)
		}
	}
}

// A checkpoint: the reader reopened there delivers what pi wrote since, in
// the run it is in; one it cannot read makes it read from the start, with a
// rescan and the same ids.
func TestRecordCheckpoint(t *testing.T) {
	e := newRecordEnv(t, "crash-mid-stream-reopened", "in-1", "in-2")
	path := filepath.Join(e.dir, sessionsDir, "2026-10-06T09-00-00-000Z_"+e.session+".jsonl")
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(full), "\n")
	// Cut after in-2's user message: its answer comes later.
	cut := 0
	for i, l := range lines {
		if strings.Contains(l, `"role":"user"`) && strings.Contains(l, "PING 2") {
			cut = i + 1
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines[:cut], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := Profile{}.Record(e.src)
	first, cp := readAll(t, r)
	same(t, "before", summary(first), []string{"user_input in_1", "user_input in_2"})
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	src := e.src
	src.Checkpoint = cp
	r2, err := Profile{}.Record(src)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := readAll(t, r2)
	same(t, "after the checkpoint", summary(after), []string{"assistant_text in_2", "turn_ended in_2 completed PONG 2"})

	src.Checkpoint = &contract.Checkpoint{Format: 9999, Data: []byte("x")}
	r3, err := Profile{}.Record(src)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := r3.Read(context.Background(), 1<<20)
	if err != nil || ch.Rescan == nil {
		t.Fatalf("first read: %+v %v, want a rescan", ch.Rescan, err)
	}
	r4, _ := Profile{}.Record(e.src)
	all, _ := readAll(t, r4)
	var ids, again []string
	for _, o := range all {
		ids = append(ids, o.ID)
	}
	for _, o := range ch.Items {
		again = append(again, o.ID)
	}
	if strings.Join(ids, ",") != strings.Join(again, ",") {
		t.Errorf("the rescan's ids differ:\n%v\n%v", again, ids)
	}
}

// Before pi writes the session's file there is nothing to read.
func TestRecordBeforeTheFile(t *testing.T) {
	e := newRecordEnv(t, "tool", "in-1")
	if err := os.RemoveAll(filepath.Join(e.dir, sessionsDir)); err != nil {
		t.Fatal(err)
	}
	r, err := Profile{}.Record(e.src)
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := readAll(t, r); len(items) != 0 {
		t.Errorf("items with no session file: %v", summary(items))
	}
	if got, _ := r.Recover(context.Background(), adapter.Marker{Native: "in-1", SessionID: e.session}); got.Outcome != contract.RecoveredUnknown {
		t.Errorf("Recover with no session file: %+v", got)
	}
}

// session is a session file of entries as pi 1.0.4 writes them, each line
// given as its type, id, parent and the rest of its fields.
func session(entries ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"session","version":3,"id":"0199c0de-0000-7000-8000-000000000001","timestamp":"2026-10-06T09:00:00.000Z","cwd":"/w"}` + "\n")
	for _, e := range entries {
		b.WriteString(e + "\n")
	}
	return []byte(b.String())
}

func entry(typ, id, parent, rest string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return `{"type":"` + typ + `","id":"` + id + `","parentId":` + p + `,"timestamp":"2026-10-06T09:00:01.000Z"` + rest + `}`
}

func tag(id, parent, input string) string {
	return entry("custom", id, parent, `,"customType":"hw.input","data":{"id":"`+input+`"}`)
}

func message(id, parent, msg string) string { return entry("message", id, parent, `,"message":`+msg) }

const (
	userPing   = `{"role":"user","content":[{"type":"text","text":"PING"}]}`
	systemMsg  = `{"role":"system","content":""}`
	answerPong = `{"role":"assistant","content":[{"type":"text","text":"PONG"}],"stopReason":"stop"}`
	failed529  = `{"role":"assistant","content":[],"stopReason":"error","errorMessage":"529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}"}`
)

// pi writes an input's tag, then may compact the context before the user
// message (AgentSession.prompt runs the input hooks, then the compaction
// check): the tag still names the input, through the compaction and the
// system message. An entry of another extension's is passed through too.
func TestRecordTagBeforeACompaction(t *testing.T) {
	e := newRecordEnvOf(t, session(
		tag("t1", "", "in-1"),
		message("u1", "t1", userPing),
		message("a1", "u1", answerPong),
		tag("t2", "a1", "in-2"),
		entry("compaction", "c2", "t2", `,"summary":"earlier work","firstKeptEntryId":"u1","tokensBefore":180000`),
		entry("custom", "x2", "c2", `,"customType":"other.extension","data":{}`),
		entry("model_change", "m2", "x2", `,"provider":"anthropic","modelId":"claude-haiku-4-5"`),
		message("s2", "m2", systemMsg),
		message("u2", "s2", userPing),
		message("a2", "u2", answerPong),
	), "in-1", "in-2")
	same(t, "compacted", read(t, e), []string{
		"user_input in_1", "assistant_text in_1", "turn_ended in_1 completed PONG",
		"user_input in_2", "assistant_text in_2", "turn_ended in_2 completed PONG",
	})
}

// A tag pairs only with a user message after it with nothing of the
// conversation between them: a refused input's tag, mid-run, is no
// ancestor of the next input's user message, even with that input's own tag
// missing.
func TestRecordStaleTag(t *testing.T) {
	e := newRecordEnvOf(t, session(
		tag("t1", "", "in-1"),
		message("u1", "t1", userPing),
		tag("tr", "u1", "in-refused"),
		message("a1", "tr", answerPong),
		message("u2", "a1", userPing),
		message("a2", "u2", answerPong),
	), "in-1", "in-refused")
	same(t, "stale tag", read(t, e), []string{
		"user_input in_1", "assistant_text in_1", "turn_ended in_1 completed PONG",
		"user_input", "assistant_text",
	})
}

// An interrupt while pi waits to retry leaves the run at its dropped
// failure: the next input's user message ends it, interrupted when the
// profile noted the interrupt, and with no end otherwise.
func TestRecordCancelledRetry(t *testing.T) {
	content := session(
		tag("t1", "", "in-1"),
		message("u1", "t1", userPing),
		message("f1", "u1", failed529),
		entry("context_edit", "e1", "f1", `,"targetId":"f1","replacement":null`),
		tag("t2", "e1", "in-2"),
		message("u2", "t2", userPing),
		message("a2", "u2", answerPong),
	)
	unnoted := newRecordEnvOf(t, content, "in-1", "in-2")
	same(t, "unnoted", read(t, unnoted), []string{
		"user_input in_1", "user_input in_2", "assistant_text in_2", "turn_ended in_2 completed PONG",
	})
	noted := newRecordEnvOf(t, content, "in-1", "in-2")
	if err := noteInterrupt(noted.scratch, "in-1"); err != nil {
		t.Fatal(err)
	}
	same(t, "noted", read(t, noted), []string{
		"user_input in_1", "turn_ended in_1 interrupted", "user_input in_2", "assistant_text in_2", "turn_ended in_2 completed PONG",
	})
}
