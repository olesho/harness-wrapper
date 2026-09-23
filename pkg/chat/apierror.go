package chat

import (
	"errors"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The harness already knows why a turn failed, and writes it down.
//
// Claude Code records a failed turn in its own session transcript as a
// synthetic assistant line — model "<synthetic>", the rendered error as its
// only text block — carrying `isApiErrorMessage: true` and a machine-readable
// `error` tag. Every layer between that file and a consumer used to drop both
// fields, so a wall was recovered by re-reading the rendered SCREEN with
// anchored regexes, per harness and per version, and a billing failure had no
// representation at all.
//
// This file reads the tag instead. It is a categorical statement from the
// harness about its own API call, not an inference from prose: an agent that
// merely PRINTS "credit balance too low" cannot produce one, which is the
// exact failure mode that sank the screen-scrape wall detector this fleet
// removed (11 detections, 0 true positives).
//
// Three rules keep it from over-reaching:
//
//   - CORRELATION. Only an entry at or beyond the pre-send watermark can speak
//     for this turn. A resumed session's transcript holds every previous
//     turn's tags, and a stale one condemning a healthy turn is the same bug
//     LogFileStartOffset exists to prevent one layer down.
//   - LAST WORD ONLY. The tag must be on the LATEST assistant entry. A turn
//     that hit a 529, retried, and then answered is a success; its transcript
//     holds both.
//   - NO GUESSING. A tag whose class is genuinely ambiguous yields no verdict,
//     and so does one outside the known vocabulary. Falling through leaves the
//     screen relabels in charge — exactly today's behaviour.
//
// Cost: two extra full transcript parses per turn — one at send for the
// watermark, one at the terminal point. Measured on the largest real
// transcript to hand (17 MB, 2939 events): 203ms each, and ~5ms at the median
// size. Against a turn measured in minutes that is not worth an incremental
// reader, and the one-shot driver already pays a full parse for History.

// watermarkUnknown marks "we could not establish how far the transcript
// already extended", which is a decline, not a zero.
const watermarkUnknown = -1

// apiErrorVerdict is what a transcript tag decided about a turn.
type apiErrorVerdict struct {
	// reason is the canonical Turn.Reason for a WALL, and empty for every
	// other mapped tag — those get a generic errored reason naming the tag,
	// because there is no canonical reason for "the API returned a 500".
	reason string
	// code is the wall token, and empty for every non-wall verdict. A
	// consumer that raises wall markers reads exactly this: absent means
	// "not a wall", never "unclassified".
	code TurnCode
	tag  string // the harness's own tag, carried into the reason as evidence
	text string // the rendered error the harness printed, filled in at match
}

// apiErrorClasses maps Claude Code's `error` vocabulary onto a verdict.
//
// Three tags name a WALL — a condition no retry fixes on its own — and carry a
// TurnCode a consumer can switch on. The rest error the turn without a code:
// they are real failures the harness recorded, so completing them would hand
// back an error string as the agent's answer, but they are not walls and
// inventing a wall token for them would put a 500 on the same footing as an
// unpayable account.
//
// rate_limit is a wall but a self-healing one, which is why it maps to the
// usage reason rather than the billing one. account_on_hold maps to BILLING,
// not auth: renewing the login would not change it, and that distinction is
// what decides whether a consumer retries or stops.
//
// The tags deliberately absent yield no verdict at all and fall through to the
// screen relabels, leaving today's behaviour exactly as it was:
//
//	invalid_request   — a malformed request. A prompt problem, not a failure
//	                    of the turn's environment.
//	max_output_tokens — the reply hit its ceiling; the turn ran and produced
//	                    output.
//	unknown           — the harness itself declined to classify it, so neither
//	                    can we.
//	policy_denied     — not an API-error tag at all; `error` also carries hook
//	                    results ("warn", "debug", "policy_denied"), which is
//	                    why apiErrorTagOf gates on isApiErrorMessage.
var apiErrorClasses = map[string]apiErrorVerdict{
	// Walls: coded.
	"billing_error":          {reason: ReasonBillingWall, code: CodeBillingWall},
	"account_on_hold":        {reason: ReasonBillingWall, code: CodeBillingWall},
	"authentication_failed":  {reason: ReasonAuthRequired, code: CodeAuthRequired},
	"oauth_org_not_allowed":  {reason: ReasonAuthRequired, code: CodeAuthRequired},
	"verification_required":  {reason: ReasonAuthRequired, code: CodeAuthRequired},
	"cloud_credential_error": {reason: ReasonAuthRequired, code: CodeAuthRequired},
	"rate_limit":             {reason: ReasonUsageLimited, code: CodeUsageLimited},

	// Failures, but not walls: errored with the tag and the harness's own text
	// in the reason, and no code. A consumer that classifies failures from text
	// reads the same words the harness printed, which is what it reads today —
	// the change here is that the turn is no longer reported as a SUCCESS whose
	// reply is "API Error: 529 Overloaded".
	"server_error":    {},
	"overloaded":      {},
	"model_not_found": {},
}

// captureTranscriptWatermark records how far the harness's transcript already
// extends, to be called from Send just before the prompt is submitted.
//
// Returns watermarkUnknown when there is nothing to measure against: no
// transcript reader, no harness session id yet, or a read that failed for any
// reason other than "no rollout on disk". A missing rollout is a real zero —
// the session has written nothing, so every entry that appears afterwards
// belongs to this turn.
//
// Never returns an error: a transcript problem must not fail a Send.
func (c *Conversation) captureTranscriptWatermark() int {
	reader, ok := c.adapter.(turns.TranscriptReader)
	if !ok {
		return watermarkUnknown
	}
	c.mu.Lock()
	sessionID := c.session.HarnessID()
	c.mu.Unlock()
	if sessionID == "" {
		// A fresh session has no prior history to confuse us with. Claude Code
		// only mints its id once the first turn runs, so this is the ordinary
		// first-Send case, not a failure.
		return 0
	}
	tturns, err := reader.ReadTranscript(sessionID, c.transcriptDir())
	switch {
	case err == nil:
		return len(tturns)
	case errors.Is(err, fs.ErrNotExist):
		// The rollout is genuinely not on disk, so it holds nothing that could
		// belong to an earlier turn. This is the only failure that may answer
		// zero: any other one leaves us unable to tell an empty transcript from
		// an unreadable one, and answering zero there would let a resumed
		// session's existing entries speak for this turn.
		return 0
	default:
		return watermarkUnknown
	}
}

// apiErrorRelabel converts a turn the HARNESS tagged as failed into the
// matching terminal failure, and reports whether it did.
//
// It runs at each completion site BEFORE the screen relabels, because it is
// the stronger evidence: the screen relabels recover a verdict by re-reading
// rendered pixels, this one reads what the harness recorded about its own API
// call. It declines — leaving every existing path exactly as it is — whenever
// it cannot establish that a tag belongs to THIS turn.
func (c *Conversation) apiErrorRelabel(turn *Turn) bool {
	return c.lastWordOfCurrentTurn().apply(turn, c.opts.Harness)
}

// transcriptWord is what the harness's own transcript says about the turn now
// finishing: a verdict it recorded, a reply, or — when neither — nothing it
// could be asked for, or nothing yet.
type transcriptWord struct {
	verdict apiErrorVerdict
	tagged  bool // the last entry is a tag this maps to a verdict
	replied bool // the last entry is a real reply, whatever failed before it
}

// apply errors turn with the verdict when there is one, and reports whether it
// did. The "reply" was the rendered error text; keeping it would hand the
// caller an error message as the turn's answer.
func (w transcriptWord) apply(turn *Turn, harness string) bool {
	if !w.tagged {
		return false
	}
	turn.State = TurnStateErrored
	turn.Reason = w.verdict.turnReason(harness)
	turn.Code = w.verdict.code
	if w.verdict.code == CodeUsageLimited {
		turn.ResumeAt = resumeAtFrom(w.verdict.text)
	}
	turn.Text = ""
	return true
}

// lastWordOfCurrentTurn reads the harness's last word on the turn now
// finishing. The zero value means no word — no reader, no session id, no
// watermark, a transcript that could not be read, no assistant entry beyond
// the watermark, or a tag this does not map.
func (c *Conversation) lastWordOfCurrentTurn() transcriptWord {
	reader, ok := c.adapter.(turns.TranscriptReader)
	if !ok {
		return transcriptWord{}
	}
	c.mu.Lock()
	sessionID := c.session.HarnessID()
	watermark := c.sentTranscriptWatermark
	c.mu.Unlock()
	if sessionID == "" || watermark == watermarkUnknown {
		return transcriptWord{}
	}

	tturns, err := reader.ReadTranscript(sessionID, c.transcriptDir())
	if err != nil {
		// The harness may simply not have flushed yet. One pause, one retry —
		// the same wait applySwallowedPromptVerdict already pays on this file.
		// A second failure yields no word; it never yields a word of "nothing
		// wrong".
		if !c.waitForTranscriptFlush() {
			return transcriptWord{}
		}
		if tturns, err = reader.ReadTranscript(sessionID, c.transcriptDir()); err != nil {
			return transcriptWord{}
		}
	}
	v, tagged, replied := apiErrorVerdictFrom(tturns, watermark)
	return transcriptWord{verdict: v, tagged: tagged, replied: replied}
}

// waitForTranscriptFlush pauses once for the harness to flush, reporting false
// if the conversation closed first.
func (c *Conversation) waitForTranscriptFlush() bool {
	select {
	case <-c.closed:
		return false
	case <-time.After(transcriptFlushRetryGap):
		return true
	}
}

// apiErrorVerdictFrom applies the LAST WORD ONLY rule: scan backwards from the
// end for the most recent assistant entry at or beyond the watermark, and let
// only THAT entry decide. A tagged entry followed by a real reply means the
// harness retried and succeeded.
//
// Split out from its caller so the rule is testable without a conversation.
// tagged reports a verdict; replied reports that the last entry is a real
// reply; neither means the transcript holds no assistant entry for this turn,
// or ends on a tag this does not map.
func apiErrorVerdictFrom(tturns []transcript.Turn, watermark int) (v apiErrorVerdict, tagged, replied bool) {
	if watermark < 0 {
		return apiErrorVerdict{}, false, false
	}
	for i := len(tturns) - 1; i >= watermark; i-- {
		t := tturns[i]
		if Role(t.Role) != RoleAssistant {
			continue
		}
		if t.APIError == "" {
			// The latest assistant entry is a real reply: whatever failed
			// before it, the turn recovered.
			return apiErrorVerdict{}, false, true
		}
		v, known := apiErrorClasses[t.APIError]
		if !known {
			// A tag outside the vocabulary — a newer harness, or one of the
			// deliberately unmapped ones. Decline rather than guess; the
			// screen relabels still get their turn.
			return apiErrorVerdict{}, false, false
		}
		v.tag = t.APIError
		v.text = t.Text
		return v, true, false
	}
	return apiErrorVerdict{}, false, false
}

// apiErrorDetailCap bounds how much of the harness's rendered error rides in
// the reason. Reasons are logged and persisted; the messages are one or two
// lines today, and a cap keeps a future multi-paragraph one out of a log line.
const apiErrorDetailCap = 240

// turnReason renders the Turn.Reason for this verdict: the canonical wall
// reason where there is one, otherwise a generic errored reason naming the
// harness. Either way the harness's own tag and its rendered text ride along
// as the evidence for the verdict — an operator reading the reason sees WHY,
// in the harness's words, not only that something failed.
func (v apiErrorVerdict) turnReason(harness string) string {
	head := v.reason
	if head == "" {
		head = harness + ": harness API error"
	}
	detail := "harness tag: " + v.tag
	if text := oneLineCapped(v.text, apiErrorDetailCap); text != "" {
		detail += "; " + text
	}
	return head + " (" + detail + ")"
}

// oneLineCapped flattens text to a single line and truncates it on a rune
// boundary, so a reason stays safe to put in a log line or a JSON state file.
func oneLineCapped(s string, max int) string {
	s = strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s))
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}
