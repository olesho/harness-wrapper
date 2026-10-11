package claudecode

import (
	"context"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// The pre-token fixtures' sessions (claude 2.1.296, the mock API): in each,
// a session's first prompt STALL 30 interrupted before its first token; a
// reply; then STALL 30, HUSH 30 (no response at all) and HUSH 30 at once
// after the turn's start, each interrupted before its first token and
// followed by a reply; and a last STALL 30 interrupted so, after which the
// Session closed and was reopened for PING 9.
const (
	tuiPreTokenSession = "7b1d2e40-5c1a-4f2b-9d3e-1a2b3c4d5e6f"
	sjPreTokenSession  = "7b1d2e40-5c1a-4f2b-9d3e-1a2b3c4d5e70"
)

// The TUI's inputs, by their prompt ids (the TUI binds an input to its
// prompt's promptId).
var tuiPreTokenInputs = map[string]string{
	"in-first": "17e1fff1-c702-49f6-aef5-f81d0b2cdf0f",
	"in-p0":    "e81cb61f-c826-4900-8459-98ea1ccfe420",
	"in-stall": "256e2176-22c6-4287-a361-78b94124db54",
	"in-p1":    "5035a966-8471-4565-a4cf-a51f05eb9ce6",
	"in-hush":  "943d4ee9-1fa5-4bcf-b57a-1cf00cd0f611",
	"in-p2":    "952af926-2b38-4f02-b627-6dd004e03f93",
	"in-hush0": "8f7f8547-1c58-4b17-90db-f74b42d26e03",
	"in-p3":    "26be5151-3019-439c-9c79-2099772688c2",
	"in-last":  "3c0f7d93-e506-4fb3-b3fb-716a605e3281",
	"in-p9":    "1c4ac82b-65d4-436e-8ac6-c1134ab55e93",
}

// The stream-json inputs, by their messages' uuids. claude wrote no prompt
// entry for in-hush, in-hush0 and in-last, only an interrupt entry.
var sjPreTokenInputs = map[string]string{
	"in-first": "af930084-d3d3-45b8-9595-432a84663eca",
	"in-p0":    "23cc6b8d-4c44-4f3c-ab7a-0ef04c305d41",
	"in-stall": "fccdf8d9-44c1-4497-a754-9bea8b3e0f55",
	"in-p1":    "2b1d673e-058c-4421-9720-0673757b1ebd",
	"in-hush":  "d84a8df9-1011-4d89-adb0-5d4d6c1a9922",
	"in-p2":    "f817e44a-eb15-480d-964d-b548e89b7719",
	"in-hush0": "94de4b49-e4b0-4a2c-9f39-1a4bb74fb20f",
	"in-p3":    "8cf6380a-cfe6-41fb-8f8e-6d7e4763c514",
	"in-last":  "b7b0f936-c46e-42db-bade-de6fb580fa1b",
	"in-p9":    "5524bb54-e695-489a-9113-52ad516a1fca",
}

// byPromptID is the TUI's record options, with an input bound to its
// prompt's id.
var byPromptID = RecordOptions{
	Native:    func(e *claudecode.Entry) string { return e.PromptID },
	PromptLag: time.Millisecond,
}

// preTokenRecord reads a fixture's record whole, at maxBytes a read, and
// recovers each input: the record-origin ends, and Recover's outcomes.
func preTokenRecord(t *testing.T, session, file string, inputs map[string]string, opts RecordOptions, maxBytes int) (map[string]contract.TurnEndedData, map[string]contract.RecoveredOutcome) {
	t.Helper()
	l, oc, m := recordAgent(t, session, file, inputs)
	r, err := NewRecord(adapter.RecordSource{SessionID: session, OpenConfig: oc, Layout: l, Markers: m}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var items []contract.Observation
	for i := 0; ; i++ {
		ch, err := r.Read(context.Background(), maxBytes)
		if err != nil {
			t.Fatal(err)
		}
		if len(ch.Items) == 0 && ch.Checkpoint == nil && opts.PromptLag > 0 {
			// Perhaps a held read: wait the lag out once.
			time.Sleep(2 * opts.PromptLag)
			ch, _ = r.Read(context.Background(), maxBytes)
		}
		if len(ch.Items) == 0 && ch.Checkpoint == nil {
			break
		}
		if i > 10000 {
			t.Fatal("the record never ends")
		}
		items = append(items, ch.Items...)
		if err := r.Commit(ch); err != nil {
			t.Fatal(err)
		}
	}
	recovered := map[string]contract.RecoveredOutcome{}
	for in := range inputs {
		mk, _, _ := m.Lookup(in)
		got, err := r.Recover(context.Background(), mk)
		if err != nil {
			t.Fatal(err)
		}
		recovered[in] = got.Outcome
	}
	return turnEnds(items), recovered
}

// In claude's TUI an interrupt before the first token leaves the prompt's
// entry alone, and the next prompt follows the entry before it: the record
// ends that turn interrupted, at the next prompt, and Recover says so. The
// session's first prompt, which the next one follows, and the last, after
// which the session ended (and was resumed), have no end the record can
// prove.
func TestRecordWithdrawnPrompt(t *testing.T) {
	for _, maxBytes := range []int{contract.MaxObserveBytes, 1500} {
		ends, recovered := preTokenRecord(t, tuiPreTokenSession, "../../transcript/claudecode/testdata/pretoken-tui-2.1.296.jsonl", tuiPreTokenInputs, byPromptID, maxBytes)
		for in, want := range map[string]struct {
			end       contract.TurnOutcome
			recovered contract.RecoveredOutcome
		}{
			"in-first": {"", contract.RecoveredUnknown},
			"in-p0":    {contract.TurnCompleted, contract.RecoveredCompleted},
			"in-stall": {contract.TurnInterrupted, contract.RecoveredInterrupted},
			"in-p1":    {contract.TurnCompleted, contract.RecoveredCompleted},
			"in-hush":  {contract.TurnInterrupted, contract.RecoveredInterrupted},
			"in-p2":    {contract.TurnCompleted, contract.RecoveredCompleted},
			"in-hush0": {contract.TurnInterrupted, contract.RecoveredInterrupted},
			"in-p3":    {contract.TurnCompleted, contract.RecoveredCompleted},
			"in-last":  {"", contract.RecoveredUnknown},
			"in-p9":    {contract.TurnCompleted, contract.RecoveredCompleted},
		} {
			if got := ends[in]; got.Outcome != want.end {
				t.Errorf("max %d: %s ended %+v, want %q", maxBytes, in, got, want.end)
			}
			if got := recovered[in]; got != want.recovered {
				t.Errorf("max %d: Recover(%s) = %s, want %s", maxBytes, in, got, want.recovered)
			}
		}
	}
}

// A session that ended right after a prompt interrupted before its first
// token holds the prompt's entry and nothing after it, as one still in
// flight, or whose claude died, does: the record cannot say.
func TestRecordWithdrawnPromptLast(t *testing.T) {
	inputs := map[string]string{}
	for in, n := range tuiPreTokenInputs {
		if in != "in-p9" {
			inputs[in] = n
		}
	}
	ends, recovered := preTokenRecord(t, tuiPreTokenSession, "../../transcript/claudecode/testdata/pretoken-tui-ended-2.1.296.jsonl", inputs, byPromptID, contract.MaxObserveBytes)
	if d, ok := ends["in-last"]; ok {
		t.Errorf("in-last ended %+v", d)
	}
	if got := recovered["in-last"]; got != contract.RecoveredUnknown {
		t.Errorf("Recover(in-last) = %s, want unknown", got)
	}
	if got := recovered["in-hush0"]; got != contract.RecoveredInterrupted {
		t.Errorf("Recover(in-hush0) = %s, want interrupted", got)
	}
}

// Under stream-json claude writes an interrupt entry for an interrupt
// before the first token, after the prompt's entry when it wrote one: the
// record ends those turns interrupted. When it wrote no prompt entry —
// interrupted before claude got to it — nothing in the record is the
// input's, and the rule for a withdrawn prompt finds nothing to end.
func TestRecordPreTokenStreamJSON(t *testing.T) {
	ends, recovered := preTokenRecord(t, sjPreTokenSession, "../../transcript/claudecode/testdata/pretoken-sj-2.1.296.jsonl", sjPreTokenInputs, RecordOptions{}, contract.MaxObserveBytes)
	for in, want := range map[string]struct {
		end       contract.TurnOutcome
		recovered contract.RecoveredOutcome
	}{
		"in-first": {contract.TurnInterrupted, contract.RecoveredInterrupted},
		"in-p0":    {contract.TurnCompleted, contract.RecoveredCompleted},
		"in-stall": {contract.TurnInterrupted, contract.RecoveredInterrupted},
		"in-hush":  {"", contract.RecoveredUnknown},
		"in-hush0": {"", contract.RecoveredUnknown},
		"in-last":  {"", contract.RecoveredUnknown},
		"in-p9":    {contract.TurnCompleted, contract.RecoveredCompleted},
	} {
		if got := ends[in]; got.Outcome != want.end {
			t.Errorf("%s ended %+v, want %q", in, got, want.end)
		}
		if got := recovered[in]; got != want.recovered {
			t.Errorf("Recover(%s) = %s, want %s", in, got, want.recovered)
		}
	}
}

// withdrawn holds for a prompt that follows the entry an earlier prompt with
// no reply followed, and for nothing else.
func TestWithdrawn(t *testing.T) {
	next := func(mod func(e *claudecode.Entry)) *claudecode.Entry {
		e := &claudecode.Entry{Type: transcript.TypeUser, UUID: "q", PromptID: "pq", Parent: "a"}
		if mod != nil {
			mod(e)
		}
		return e
	}
	for _, c := range []struct {
		name           string
		prompt, parent string
		replied        bool
		e              *claudecode.Entry
		want           bool
	}{
		{"a prompt that rewound one with no reply", "p", "a", false, next(nil), true},
		{"the turn had a reply", "p", "a", true, next(nil), false},
		{"the first prompt follows nothing", "p", "", false, next(func(e *claudecode.Entry) { e.Parent = "" }), false},
		{"the next prompt follows the turn", "p", "a", false, next(func(e *claudecode.Entry) { e.Parent = "p" }), false},
		{"the prompt itself", "p", "a", false, next(func(e *claudecode.Entry) { e.UUID = "p" }), false},
		{"no prompt: an interrupt or a tool result", "p", "a", false, next(func(e *claudecode.Entry) { e.PromptID = "" }), false},
		{"a task notification", "p", "a", false, next(func(e *claudecode.Entry) { e.TaskNotification = "t1" }), false},
		{"an assistant entry", "p", "a", false, next(func(e *claudecode.Entry) { e.Type = transcript.TypeAssistant }), false},
		{"no turn", "", "a", false, next(nil), false},
	} {
		if got := withdrawn(c.prompt, c.parent, c.replied, c.e); got != c.want {
			t.Errorf("%s: withdrawn = %v, want %v", c.name, got, c.want)
		}
	}
}
