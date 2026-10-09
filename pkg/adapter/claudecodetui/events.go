package claudecodetui

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// tracker turns what claude reports — its live hooks and its debug log's lines
// — into the transport's events, for one claude process. It knows one input's
// turn at a time: the transport types an input only once the turn before it
// has settled, so every line and hook it reads concerns that turn. It does no
// I/O, so its tests need no claude.
//
//   - UserPromptSubmit that bound the prompt to the input is the input's
//     receipt, and its turn's start.
//   - Stop ends the turn completed, with last_assistant_message as its text;
//     StopFailure ends it errored, classified as the stream-json profile
//     classifies claude's failures.
//   - The debug log's `[engine] turn N start` and `… end` bracket the turn:
//     the start is the gate below, and the end says claude can take the next
//     input. Its API errors give a failure's HTTP status, and each one claude
//     retries is reported as it comes (Retrying).
//   - An interrupt (Esc) has no hook: the debug log's `[onCancel]`, then the
//     turn's end, end it interrupted. A turn end whose stop reason is an
//     interrupt's (null: a reply stopped, tool_use: a tool stopped) ends it at
//     once; with another reason the turn finished as Esc landed, and Stop or
//     StopFailure says how, or, when neither comes within hookGrace, it ends
//     interrupted (probes/tui-hybrid, Closing the gaps).
//
// The gate: claude's first turn must log `[engine] turn N start`. A claude
// whose does not logs lines this profile cannot read, so the turn ends errored
// and the transport stops claude (ADR-012, decision 6).
type tracker struct {
	id   string
	turn *flight
	// ended counts the turns that ended in this process.
	ended int
	// up: claude's SessionStart for this Session came; engine: its debug log
	// carries engine lines.
	up, engine bool
	// foreign is another session claude reported running.
	foreign string
	// wipe is how many Ctrl-U the composer takes before the next input is
	// typed: Esc was pressed, and claude may have put the interrupted prompt
	// back in its composer.
	wipe int
}

// flight is the input's turn the tracker follows.
type flight struct {
	native string
	// lines is how many lines the input has.
	lines    int
	promptID string
	started  bool
	ended    bool
	// turnLine: the debug log logged the turn's start; endLine: its end.
	turnLine, endLine bool
	// status is the HTTP status of the turn's last failed API call.
	status int
	// parked is the first turn's end, waiting for the debug log (the gate).
	parked   *live.Event
	parkedAt time.Time
	// retries are the turn's retries the debug log logged before its
	// receipt, reported once it starts.
	retries []contract.RetryingData
	// retried is the last retry reported, or held.
	retried int
	// cancel: claude logged [onCancel] while the turn ran.
	cancel bool
	// endAt is when the debug log logged the turn's end after a cancel, with
	// a stop reason no interrupt gives: Stop or StopFailure may still come.
	endAt time.Time
}

// step is what one report of claude's comes to.
type step struct {
	events []adapter.Event
	// fatal, when set, says why claude must be stopped.
	fatal string
}

func newTracker(id string) *tracker { return &tracker{id: id} }

// begin follows the input native's turn, of lines lines, which the
// transport is typing.
func (k *tracker) begin(native string, lines int) {
	k.turn = &flight{native: native, lines: max(lines, 1)}
}

// maxWipe bounds the Ctrl-U an interrupted input is cleared with.
const maxWipe = 2048

// interrupting notes that Esc is pressed for the input's turn: claude may put
// its prompt back in the composer. Ctrl-U clears a line, and on an empty line
// joins it to the one before (claude 2.1.283), so a prompt of n lines takes
// 2n-1.
func (k *tracker) interrupting() {
	if f := k.turn; f != nil {
		k.wipe = max(k.wipe, min(2*f.lines, maxWipe))
	}
}

// settled reports whether claude can take an input: no input's turn is
// followed, or the one followed has ended and claude's debug log has logged
// its end.
func (k *tracker) settled() bool {
	return k.turn == nil || k.turn.ended && k.turn.endLine
}

// received reports whether claude took the input native.
func (k *tracker) received(native string) bool {
	return k.turn != nil && k.turn.native == native && k.turn.started
}

// live takes one live hook event.
func (k *tracker) live(ev live.Event, now time.Time) step {
	switch ev.Hook {
	case live.HookSessionStart:
		if ev.SessionID != "" && !strings.EqualFold(ev.SessionID, k.id) {
			if k.foreign == "" {
				k.foreign = ev.SessionID
			}
			return step{fatal: "claude runs session " + ev.SessionID + ", not " + k.id}
		}
		k.up = true
	case live.HookUserPrompt:
		f := k.turn
		if f == nil || f.started || ev.Native == "" || ev.Native != f.native {
			return step{}
		}
		f.started, f.promptID = true, ev.PromptID
		evs := []adapter.Event{{Kind: adapter.Started, Native: f.native, Time: now}}
		for _, r := range f.retries {
			evs = append(evs, adapter.Event{Kind: adapter.Retrying, Native: f.native, Time: now, Retry: r})
		}
		f.retries = nil
		return step{events: evs}
	case live.HookStop, live.HookStopFailure:
		f := k.turn
		if f == nil || !f.started || f.ended || f.parked != nil || ev.PromptID != f.promptID {
			return step{}
		}
		if k.ended == 0 && !f.turnLine {
			// The gate: wait for the debug log, which claude may write
			// after the hook.
			f.parked, f.parkedAt = &ev, now
			return step{}
		}
		return k.end(ev, false, now)
	}
	return step{}
}

// gateFailed says why the gate stops claude.
const gateFailed = "claude's debug log logged no [engine] turn start for its first turn: the profile cannot read this claude"

// gateGrace is how long a first turn's end waits for the debug log's turn
// start.
const gateGrace = 3 * time.Second

// end ends the input's turn as ev says, or errored when the gate failed.
func (k *tracker) end(ev live.Event, gate bool, now time.Time) step {
	f := k.turn
	f.ended, f.parked = true, nil
	k.ended++
	e := adapter.Event{Kind: adapter.Ended, Native: f.native, Time: now}
	var s step
	switch {
	case gate:
		e.Outcome, e.Error = contract.TurnErrored, &contract.TurnError{Class: contract.ErrorInternal}
		s.fatal = gateFailed
	case ev.Hook == live.HookStop:
		e.Outcome, e.Text = contract.TurnCompleted, ev.Message
	default:
		status := f.status
		if status == 0 {
			status = statusIn(ev.Message)
		}
		e.Outcome = contract.TurnErrored
		e.Error = claudecode.TurnError(ev.Error, status, ev.Message, false, now)
	}
	s.events = []adapter.Event{e}
	return s
}

// debug takes one line of claude's debug log. Lines about a turn count only
// while the input's turn is in flight.
func (k *tracker) debug(l debugLine, now time.Time) step {
	if l.kind != lineOther {
		k.engine = true
	}
	f := k.turn
	if f == nil {
		return step{}
	}
	switch l.kind {
	case lineTurnStart:
		if !f.ended {
			f.turnLine = true
		}
		if f.parked != nil {
			return k.end(*f.parked, false, now)
		}
	case lineAPIError:
		if f.ended {
			break
		}
		f.status = l.status
		if l.max < 2 || l.attempt >= l.max {
			break // the last attempt: no retry follows
		}
		// attempt k of N failed, and claude makes retry k of N-1. Its
		// first failure is retried at once without advancing k, so attempt
		// 1 may come twice: it is reported once.
		if l.attempt <= f.retried {
			break
		}
		f.retried = l.attempt
		r := contract.RetryingData{Attempt: l.attempt, Max: l.max - 1, HTTPStatus: l.status}
		if !f.started {
			f.retries = append(f.retries, r)
			break
		}
		return step{events: []adapter.Event{{Kind: adapter.Retrying, Native: f.native, Time: now, Retry: r}}}
	case lineCancel:
		if f.started && !f.ended {
			f.cancel = true
		}
	case lineTurnEnd:
		f.endLine = true
		if f.parked != nil {
			return k.end(*f.parked, !f.turnLine, now)
		}
		if f.cancel && f.started && !f.ended {
			if l.stop == "null" || l.stop == "tool_use" {
				return k.interrupted(now)
			}
			f.endAt = now
		}
	}
	return step{}
}

// hookGrace is how long a turn whose end the debug log logged after a cancel,
// with a stop reason no interrupt gives, waits for Stop or StopFailure before
// it ends interrupted.
const hookGrace = 2 * time.Second

// interrupted ends the input's turn interrupted: claude took the interrupt,
// and ended the turn with no hook.
func (k *tracker) interrupted(now time.Time) step {
	f := k.turn
	f.ended = true
	k.ended++
	return step{events: []adapter.Event{{Kind: adapter.Ended, Native: f.native, Time: now, Outcome: contract.TurnInterrupted}}}
}

// tick fails the gate once a first turn's end has waited gateGrace for the
// debug log, and ends interrupted a cancelled turn that ended with no hook
// within hookGrace.
func (k *tracker) tick(now time.Time) step {
	f := k.turn
	switch {
	case f == nil || f.ended:
	case f.parked != nil && now.Sub(f.parkedAt) >= gateGrace:
		return k.end(*f.parked, true, now)
	case !f.endAt.IsZero() && now.Sub(f.endAt) >= hookGrace:
		return k.interrupted(now)
	}
	return step{}
}

// reAPIStatus finds the status in claude's text for a failed call: "API
// Error: 529 …".
var reAPIStatus = regexp.MustCompile(`API Error: (\d{3})\b`)

func statusIn(text string) int {
	m := reAPIStatus.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
