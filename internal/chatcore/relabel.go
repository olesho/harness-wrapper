package chatcore

import (
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/internal/resettime"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// assistantText returns the clean assistant reply for a completed turn: when
// the adapter implements turns.MessageExtractor (e.g. claude-code) and can
// isolate the message from the rendered TUI, that cleaned text is used;
// otherwise we fall back to the raw screen snapshot. This keeps screen-derived
// Turn.Text parseable (a one-shot reply, not a full-screen dump) without
// depending on a persisted transcript.
func (c *Conversation) assistantText(snap screen.Snapshot) string {
	if ex, ok := c.adapter.(turns.MessageExtractor); ok {
		if msg, ok := ex.ExtractMessage(snap); ok {
			return msg
		}
	}
	return snap.Text
}

// cleanAssistantText is the adapter's extracted assistant reply with NO
// whole-screen fallback: "" when the adapter has no extractor or finds no reply
// (unlike assistantText, which returns the whole screen in that case). It is the
// "did this turn actually produce a reply?" signal used by authRelabel.
func (c *Conversation) cleanAssistantText(snap screen.Snapshot) string {
	if ex, ok := c.adapter.(turns.MessageExtractor); ok {
		if msg, ok := ex.ExtractMessage(snap); ok {
			return strings.TrimSpace(msg)
		}
	}
	return ""
}

// authRelabel converts a turn that "completed" but produced NO real assistant
// reply, on a settled screen showing a logged-out / not-onboarded banner, into
// the canonical ReasonAuthRequired failure. Without it a logged-out claude-code
// turn — which ends on a "✻ … for 0s" end-of-turn thinking marker, not an error
// — is persisted as a SUCCESS with the raw banner screen as its "reply" (the
// false-success bug). Gated on an EMPTY clean extraction, so a genuine reply
// (which produces a "⏺" bullet) is never touched even if it mentions "/login".
// Returns true if it relabeled the turn.
func (c *Conversation) authRelabel(turn *Turn, snap screen.Snapshot) bool {
	if c.cleanAssistantText(snap) != "" {
		return false
	}
	if !authRequired(c.opts.Harness, snap.Text) {
		return false
	}
	turn.State = TurnStateErrored
	turn.Reason = ReasonAuthRequired
	turn.Code = CodeAuthRequired
	turn.Text = ""
	return true
}

// relabelTerminal applies the relabels in strength order to a turn that
// reached a terminal point looking like a success, and reports whether any of
// them took it.
//
// The order is the whole point:
//
//  1. The harness's last word — what the HARNESS recorded about its own API
//     call. A categorical statement, correlated to this turn by the pre-send
//     watermark, and the only one that can name a billing wall.
//  2. usageLimitRelabel — the quota wall, which claude paints as an assistant
//     bubble. Deliberately NOT gated on an empty extraction, because the wall
//     IS the extraction, which is why it must precede the auth check.
//  3. authRelabel — a logged-out / onboarding screen with no real reply.
//  4. heldRelabel — a held turn the harness's record did not settle.
//
// Each declines cleanly when it has nothing to say, so a turn that really did
// complete passes through all of them untouched.
func (c *Conversation) relabelTerminal(turn *Turn, snap screen.Snapshot) bool {
	word := c.lastWordOfCurrentTurn()
	return word.apply(turn, c.opts.Harness) ||
		c.usageLimitRelabel(turn, snap) ||
		c.authRelabel(turn, snap) ||
		c.heldRelabel(turn, word)
}

// holdsTurns reports whether a Blocked leaves the in-flight turn pending rather
// than ending it: in a keep-alive conversation (ADR-006) the harness ends its
// turns, and one whose adapter reads transcripts can have the outcome decided
// by the harness's own record when it does. An adapter without a transcript
// reader, and every default-mode conversation — whose run-to-completion callers
// rely on a Blocked ending the turn — keep that behaviour.
func (c *Conversation) holdsTurns() bool {
	if !c.opts.KeepAliveOnClassification {
		return false
	}
	_, reads := c.adapter.(turns.TranscriptReader)
	return reads
}

// heldRelabel ends a held turn errored with the Blocked it was held on, when
// the harness's own record did not settle it: its transcript could not be
// read, or holds no word on this turn. A success nobody can confirm is a wrong
// verdict (principle 2). A turn whose transcript shows a reply after the error
// recovered, and is left complete.
func (c *Conversation) heldRelabel(turn *Turn, word transcriptWord) bool {
	c.mu.Lock()
	reason := c.heldReason
	c.mu.Unlock()
	if reason == "" || word.replied {
		return false
	}
	turn.State = TurnStateErrored
	turn.Reason = reason
	turn.Text = ""
	return true
}

// usageLimitRelabel converts a turn that "completed" while the harness was out of
// subscription quota into the canonical ReasonUsageLimited failure. claude-code
// paints its usage/session-limit wall as an assistant bubble, so the turn ends on
// a normal end-of-turn marker and the wall itself is what ExtractMessage returns —
// persisting it would be a false SUCCESS whose "reply" is the wall, which a
// downstream validator then rejects as a bogus answer, retrying until a TRANSIENT
// quota outage blocks the task.
//
// Unlike authRelabel this is deliberately NOT gated on an empty extraction — the
// wall IS the extraction — so it must run FIRST at each completion site (an
// empty-gated authRelabel would decline anyway, but the ordering is what makes the
// intent explicit). The probe prefers the clean extracted reply and falls back to
// the whole screen, so it fires whether the CLI renders the wall as a "⏺" bubble
// (captured) or only as a "⎿" decoration (not captured, still on screen).
// Returns true if it relabeled the turn.
func (c *Conversation) usageLimitRelabel(turn *Turn, snap screen.Snapshot) bool {
	probe := c.cleanAssistantText(snap)
	if strings.TrimSpace(probe) == "" {
		probe = snap.Text
	}
	msg, ok := usageLimitMessage(c.opts.Harness, probe)
	if !ok {
		return false
	}
	turn.State = TurnStateErrored
	turn.Reason = ReasonUsageLimited + " (" + msg + ")"
	turn.Code = CodeUsageLimited
	turn.ResumeAt = resumeAtFrom(msg)
	turn.Text = ""
	return true
}

// resumeAtFrom is the reset time a usage wall names, zero when it names none —
// read with the parser the wrapper's session-limit matcher uses, so a wall
// reports one reset time whichever layer saw it.
func resumeAtFrom(wall string) time.Time {
	at, _ := resettime.Parse(wall, time.Now())
	return at
}
