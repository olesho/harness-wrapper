package claudecodetui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The debug lines of interrupts, as the pinned claude writes them
// (testdata/debug-2.1.283-interrupt.log: Esc mid-reply, Esc before the first
// token — claude took it before it logged the turn's start — and Esc while a
// tool ran), parse as the profile expects.
func TestDebugInterruptFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "debug-2.1.283-interrupt.log"))
	if err != nil {
		t.Fatal(err)
	}
	var got []debugLine
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if d := parseDebugLine(l); d.kind != lineOther && d.kind != lineEngine {
			got = append(got, d)
		}
	}
	want := []debugLine{
		{kind: lineTurnStart, turn: 1},
		{kind: lineCancel},
		{kind: lineTurnEnd, turn: 1, stop: "null"},
		{kind: lineCancel},
		{kind: lineTurnStart, turn: 1},
		{kind: lineTurnEnd, turn: 1, stop: "null"},
		{kind: lineTurnStart, turn: 1},
		{kind: lineCancel},
		{kind: lineTurnEnd, turn: 1, stop: "tool_use"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed\n%+v\nwant\n%+v", got, want)
	}
}

// feed gives the tracker the debug lines, and returns the events they made.
func feed(k *tracker, lines ...string) []adapter.Event {
	var out []adapter.Event
	for _, l := range lines {
		out = append(out, k.debug(line(l), t0).events...)
	}
	return out
}

// Esc ends a turn interrupted with no hook: the debug log's [onCancel], then
// the turn's end with an interrupt's stop reason — also when claude took Esc
// before it logged the turn's start. claude takes the next input once it has.
func TestTrackerInterrupted(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
	}{
		{"mid-reply", []string{"[engine] turn 1 start", "[onCancel] source=local streamMode=responding", "[engine] turn 1 end (turns=2 stop=null resultLen=0)"}},
		{"before the turn's start", []string{"[onCancel] source=local streamMode=responding", "[engine] turn 1 start", "[engine] turn 1 end (turns=2 stop=null resultLen=0)"}},
		{"mid-tool", []string{"[engine] turn 1 start", "[onCancel] source=local streamMode=tool-use", "[engine] turn 1 end (turns=3 stop=tool_use resultLen=0)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := started(t)
			k.interrupting()
			evs := feed(k, c.lines...)
			if len(evs) != 1 || evs[0].Kind != adapter.Ended || evs[0].Native != native || evs[0].Outcome != contract.TurnInterrupted {
				t.Fatalf("events %+v, want the input's end, interrupted", evs)
			}
			if !k.settled() {
				t.Error("not settled after the interrupted turn's end")
			}
			if k.wipe != 2 {
				t.Errorf("wipe %d, want 2 Ctrl-U for a one-line input", k.wipe)
			}
			// No hook follows an interrupt; one that did would end nothing.
			if s := k.live(live.Event{Hook: live.HookStop, PromptID: prompt}, t0); len(s.events) != 0 {
				t.Errorf("a Stop after the interrupt: %+v", s)
			}
		})
	}
}

// A turn end with no interrupt's stop reason after a cancel: the turn finished
// as Esc landed, and the hook says how; with no hook within hookGrace it ends
// interrupted. A turn end without a cancel waits for the hook as ever.
func TestTrackerCancelRaces(t *testing.T) {
	k := started(t)
	feed(k, "[engine] turn 1 start", "[onCancel] source=local streamMode=responding", "[engine] turn 1 end (turns=1 stop=end_turn resultLen=6)")
	e := ended(t, k.live(live.Event{Hook: live.HookStop, PromptID: prompt, Message: "PONG 1"}, t0))
	if e.Outcome != contract.TurnCompleted || e.Text != "PONG 1" {
		t.Errorf("Stop after a cancel: %+v", e)
	}

	k = started(t)
	feed(k, "[engine] turn 1 start", "[onCancel] source=local streamMode=responding", "[engine] turn 1 end (turns=1 stop=stop_sequence resultLen=0)")
	e = ended(t, k.live(live.Event{Hook: live.HookStopFailure, PromptID: prompt, Error: "server_error", Message: "API Error: 500 boom"}, t0))
	if e.Outcome != contract.TurnErrored {
		t.Errorf("StopFailure after a cancel: %+v", e)
	}

	k = started(t)
	feed(k, "[engine] turn 1 start", "[onCancel] source=local streamMode=responding", "[engine] turn 1 end (turns=1 stop=stop_sequence resultLen=0)")
	if s := k.tick(t0.Add(hookGrace / 2)); len(s.events) != 0 {
		t.Fatalf("ended before hookGrace: %+v", s)
	}
	if e := ended(t, k.tick(t0.Add(hookGrace))); e.Outcome != contract.TurnInterrupted {
		t.Errorf("no hook within hookGrace: %+v", e)
	}

	k = started(t)
	if evs := feed(k, "[engine] turn 1 start", "[engine] turn 1 end (turns=1 stop=null resultLen=0)"); len(evs) != 0 {
		t.Errorf("a turn end without a cancel: %+v", evs)
	}
	if s := k.tick(t0.Add(hookGrace)); len(s.events) != 0 {
		t.Errorf("a turn end without a cancel, after hookGrace: %+v", s)
	}
}

// A cancel while no input's turn runs, or once it ended, changes nothing.
func TestTrackerCancelStrays(t *testing.T) {
	k := newTracker(sid)
	if evs := feed(k, "[onCancel] source=local streamMode=responding"); len(evs) != 0 {
		t.Errorf("a cancel with no turn: %+v", evs)
	}
	k = started(t)
	feed(k, "[engine] turn 1 start")
	ended(t, k.live(live.Event{Hook: live.HookStop, PromptID: prompt, Message: "PONG 1"}, t0))
	if evs := feed(k, "[onCancel] source=local streamMode=responding", "[engine] turn 1 end (turns=1 stop=end_turn resultLen=6)"); len(evs) != 0 {
		t.Errorf("a cancel after the turn ended: %+v", evs)
	}
}

// An interrupted input of n lines is cleared with 2n Ctrl-U, at most maxWipe.
func TestTrackerWipe(t *testing.T) {
	k := newTracker(sid)
	k.interrupting()
	if k.wipe != 0 {
		t.Errorf("with no turn: %d", k.wipe)
	}
	k.begin(native, 3)
	k.interrupting()
	if k.wipe != 6 {
		t.Errorf("three lines: %d, want 6", k.wipe)
	}
	k.begin(native, 1<<20)
	k.interrupting()
	if k.wipe != maxWipe {
		t.Errorf("a huge input: %d, want %d", k.wipe, maxWipe)
	}
}

// Each API error claude retries is reported as a retry while the turn runs:
// retry k of N-1 for attempt k of N, once each, the last attempt not at all,
// and those logged before the receipt right after the turn's start.
func TestTrackerRetries(t *testing.T) {
	k := newTracker(sid)
	k.live(live.Event{Hook: live.HookSessionStart, SessionID: sid}, t0)
	k.begin(native, 1)
	early := feed(k, "[engine] turn 1 start", `[ERROR] API error (attempt 1/3): 529 529 {"error":{}}`)
	if len(early) != 0 {
		t.Fatalf("a retry before the receipt: %+v", early)
	}
	s := k.live(live.Event{Hook: live.HookUserPrompt, PromptID: prompt, Native: native}, t0)
	want := []adapter.Event{
		{Kind: adapter.Started, Native: native, Time: t0},
		{Kind: adapter.Retrying, Native: native, Time: t0, Retry: contract.RetryingData{Attempt: 1, Max: 2, HTTPStatus: 529}},
	}
	if !reflect.DeepEqual(s.events, want) {
		t.Fatalf("the receipt: %+v\nwant %+v", s.events, want)
	}
	got := feed(
		k,
		`[ERROR] API error (attempt 1/3): 529 529 {"error":{}}`,
		`[ERROR] API error (attempt 2/3): 529 529 {"error":{}}`,
		`[ERROR] API error (attempt 3/3): 529 529 {"error":{}}`,
	)
	var retries []contract.RetryingData
	for _, e := range got {
		if e.Kind != adapter.Retrying || e.Native != native {
			t.Fatalf("event %+v", e)
		}
		retries = append(retries, e.Retry)
	}
	// Attempt 1 again (claude's immediate retry) was reported already.
	if want := []contract.RetryingData{{Attempt: 2, Max: 2, HTTPStatus: 529}}; !reflect.DeepEqual(retries, want) {
		t.Errorf("retries %+v, want %+v", retries, want)
	}
	e := ended(t, k.live(live.Event{Hook: live.HookStopFailure, PromptID: prompt, Error: "server_error", Message: "API Error: 529 x"}, t0))
	if e.Error == nil || e.Error.Class != contract.ErrorOverloaded {
		t.Errorf("end %+v", e)
	}
	if evs := feed(k, `[ERROR] API error (attempt 1/3): 529 529 {}`); len(evs) != 0 {
		t.Errorf("a retry after the turn ended: %+v", evs)
	}
}
