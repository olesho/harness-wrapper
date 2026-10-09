package claudecodetui

import (
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

const (
	sid    = "a30208b1-8202-47b1-a589-0a5793636303"
	native = "in-native"
	prompt = "7299545c-aa9f-4374-94f5-af0996e0075d"
)

var t0 = time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)

func line(s string) debugLine { return parseDebugLine("2026-10-07T15:44:51.114Z [DEBUG] " + s) }

// started is a tracker that is following the input native's turn, received.
func started(t *testing.T) *tracker {
	t.Helper()
	k := newTracker(sid)
	k.live(live.Event{Hook: live.HookSessionStart, SessionID: sid, Source: "startup"}, t0)
	k.debug(line("[engine] send set_model model=claude-sonnet-5"), t0)
	if !k.up || !k.engine || !k.settled() {
		t.Fatalf("after start: up %v engine %v settled %v", k.up, k.engine, k.settled())
	}
	k.begin(native, 1)
	if k.settled() {
		t.Fatal("settled while an input is typed")
	}
	s := k.live(live.Event{Hook: live.HookUserPrompt, SessionID: sid, PromptID: prompt, Prompt: "PING 1", Native: native}, t0)
	if len(s.events) != 1 || s.events[0].Kind != adapter.Started || s.events[0].Native != native || !k.received(native) {
		t.Fatalf("the receipt: %+v", s)
	}
	return k
}

func ended(t *testing.T, s step) adapter.Event {
	t.Helper()
	if len(s.events) != 1 || s.events[0].Kind != adapter.Ended || s.events[0].Native != native {
		t.Fatalf("want the input's end, got %+v", s)
	}
	return s.events[0]
}

// A turn: the receipt starts it, Stop completes it with its text, and claude
// takes the next input once the debug log logged the turn's end.
func TestTrackerCompleted(t *testing.T) {
	k := started(t)
	k.debug(line("[engine] turn 1 start"), t0)
	e := ended(t, k.live(live.Event{Hook: live.HookStop, SessionID: sid, PromptID: prompt, Message: "PONG 1"}, t0))
	if e.Outcome != contract.TurnCompleted || e.Text != "PONG 1" {
		t.Errorf("end %+v", e)
	}
	if k.settled() {
		t.Error("settled before the debug log logged the turn's end")
	}
	k.debug(line("[engine] turn 1 end (turns=1 usage in=10 out=5 cost=$0.0001 api=47ms stop=end_turn resultLen=6)"), t0)
	if !k.settled() {
		t.Error("not settled after the turn's end")
	}
	// A second Stop for it, or one of another prompt, ends nothing.
	if s := k.live(live.Event{Hook: live.HookStop, PromptID: prompt}, t0); len(s.events) != 0 {
		t.Errorf("a repeated Stop: %+v", s)
	}
}

// StopFailure errors the turn, classified as the stream-json profile
// classifies claude's failures: the status from the debug log's API errors,
// or from claude's text.
func TestTrackerErrored(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
		ev    live.Event
		class contract.ErrorClass
	}{
		{
			"retries exhausted",
			[]string{"[engine] turn 1 start", `[ERROR] API error (attempt 3/3): 529 529 {"error":{}}`},
			live.Event{Error: "server_error", Message: "API Error: 529 mock overloaded_error 4/99."},
			contract.ErrorOverloaded,
		},
		{
			"status from the text",
			[]string{"[engine] turn 1 start"},
			live.Event{Error: "server_error", Message: "API Error: 529 mock overloaded_error 4/99."},
			contract.ErrorOverloaded,
		},
		{
			"a server error",
			[]string{"[engine] turn 1 start"},
			live.Event{Error: "server_error", Message: "API Error: 500 boom"},
			contract.ErrorAPI,
		},
		{
			"usage wall",
			[]string{"[engine] turn 1 start", `[ERROR] API error (attempt 1/3): 429 429 {}`},
			live.Event{Error: "rate_limit", Message: "You've hit your session limit · resets 7:29pm (Europe/Istanbul)"},
			contract.ErrorUsageLimit,
		},
		{
			"auth",
			[]string{"[engine] turn 1 start"},
			live.Event{Error: "authentication_failed", Message: "Invalid API key"},
			contract.ErrorAuth,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := started(t)
			for _, l := range c.lines {
				k.debug(line(l), t0)
			}
			ev := c.ev
			ev.Hook, ev.PromptID = live.HookStopFailure, prompt
			e := ended(t, k.live(ev, t0))
			if e.Outcome != contract.TurnErrored || e.Error == nil || e.Error.Class != c.class {
				t.Errorf("end %+v %+v, want errored %s", e, e.Error, c.class)
			}
			if c.class == contract.ErrorUsageLimit && e.Error.ResumeAt == nil {
				t.Error("a usage wall with no reset time")
			}
		})
	}
}

// The gate: a first turn whose start the debug log never logs ends errored,
// and the transport is told to stop claude; one whose start comes after the
// hook ends as the hook says.
func TestTrackerGate(t *testing.T) {
	stop := live.Event{Hook: live.HookStop, SessionID: sid, PromptID: prompt, Message: "PONG 1"}

	k := started(t)
	if s := k.live(stop, t0); len(s.events) != 0 {
		t.Fatalf("the first turn ended before the debug log said it started: %+v", s)
	}
	e := ended(t, k.debug(line("[engine] turn 1 start"), t0))
	if e.Outcome != contract.TurnCompleted {
		t.Errorf("a late turn start: %+v", e)
	}

	k = started(t)
	k.live(stop, t0)
	if s := k.tick(t0.Add(gateGrace / 2)); len(s.events) != 0 {
		t.Fatalf("the gate failed early: %+v", s)
	}
	s := k.tick(t0.Add(gateGrace))
	if e := ended(t, s); e.Outcome != contract.TurnErrored || e.Error.Class != contract.ErrorInternal || s.fatal == "" {
		t.Errorf("the gate: %+v", s)
	}

	k = started(t)
	k.live(stop, t0)
	s = k.debug(line("[engine] turn 1 end (turns=1 stop=end_turn resultLen=6)"), t0)
	if e := ended(t, s); e.Outcome != contract.TurnErrored || s.fatal == "" {
		t.Errorf("a turn end with no start: %+v", s)
	}

	// After the first turn, the gate is passed.
	k = started(t)
	k.debug(line("[engine] turn 1 start"), t0)
	k.live(stop, t0)
	k.debug(line("[engine] turn 1 end (turns=1 stop=end_turn resultLen=6)"), t0)
	k.begin("second", 1)
	k.live(live.Event{Hook: live.HookUserPrompt, PromptID: "p2", Native: "second"}, t0)
	if s := k.live(live.Event{Hook: live.HookStop, PromptID: "p2", Message: "PONG 2"}, t0); len(s.events) != 1 || s.events[0].Outcome != contract.TurnCompleted {
		t.Errorf("the second turn: %+v", s)
	}
}

// What is not the input's ends nothing: a prompt bound to no input (claude's
// own), a queued one, a Stop of another prompt; and a SessionStart of another
// session is fatal.
func TestTrackerStrays(t *testing.T) {
	k := started(t)
	for _, ev := range []live.Event{
		{Hook: live.HookUserPrompt, PromptID: "other", Prompt: "<task-notification>…"},
		{Hook: live.HookUserPrompt, PromptID: prompt, Prompt: "again", Queued: true},
		{Hook: live.HookStop, PromptID: "other", Message: "BG DONE"},
		{Hook: live.HookStopFailure, PromptID: ""},
	} {
		if s := k.live(ev, t0); len(s.events) != 0 || s.fatal != "" {
			t.Errorf("%+v: %+v", ev, s)
		}
	}
	if s := k.live(live.Event{Hook: live.HookSessionStart, SessionID: "0f0e0d0c-0000-4000-8000-000000000000"}, t0); s.fatal == "" || k.foreign == "" {
		t.Errorf("another session: %+v", s)
	}
	// Lines about a turn while none is followed change nothing.
	k = newTracker(sid)
	k.debug(line("[engine] turn 7 end (stop=end_turn resultLen=1)"), t0)
	if !k.settled() || k.turn != nil {
		t.Error("a turn line with no input's turn")
	}
}

func TestStatusIn(t *testing.T) {
	for text, want := range map[string]int{
		"API Error: 529 mock overloaded_error 4/99. This is a server-side issue": 529,
		"API Error: Connection refused":                                          0,
		"":                                                                       0,
	} {
		if got := statusIn(text); got != want {
			t.Errorf("statusIn(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestTyped(t *testing.T) {
	for in, want := range map[string]string{
		"PING 1":      "PING 1",
		"a\nb":        pasteStart + "a\nb" + pasteEnd,
		"a\r\nb":      pasteStart + "a\nb" + pasteEnd,
		"tab\there":   pasteStart + "tab\there" + pasteEnd,
		"x\x1b[201~y": pasteStart + "xy" + pasteEnd,
	} {
		if got := typed(in); got != want {
			t.Errorf("typed(%q) = %q, want %q", in, got, want)
		}
	}
	if got := typed("x" + pasteEnd + "y"); got != pasteStart+"xy"+pasteEnd {
		t.Errorf("a paste's end inside the text: %q", got)
	}
}

func TestPlain(t *testing.T) {
	if got := plain("\x1b[31mError:\x1b[0m session \x1b]0;t\x07already in use\r\n"); got != "Error: session already in use\n" {
		t.Errorf("plain = %q", got)
	}
}
