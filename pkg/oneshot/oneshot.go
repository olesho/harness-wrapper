// Package oneshot is the IN-PROCESS Go library for the typed one-shot turn — the
// parity sibling of meta-harness's `oneshot` library subpath
// (runOneShot / runOneShotDetailed), returning the typed outcome union under a
// headless auto-accept-trust policy WITHOUT spawning a subprocess or crossing a
// workspace transport.
//
// The typed union already exists two ways in this tree: the guest
// `structured-run` subcommand (cmd/harness-wrapper) and the host-side
// pkg/env.RunStructuredTurn client. Both go over a boundary — a spawned process
// or a Workspace exec. oneshot fills the narrow remaining gap: an ordinary Go
// call that drives one real interactive harness turn via harness.RunTurn and
// hands back turnproto.TurnStatus directly.
//
// This is extract-and-consolidate, NOT a third re-implementation. oneshot reuses
// turnproto.TurnStatus (the frozen union) rather than defining a new one, and
// Classify is the (TurnResult, error) → status/reason core lifted VERBATIM from
// the guest classifyStructuredResult, preserving its exact branch order. The
// status → exit-code table is NOT here — it is turnproto.ExitCode, the single
// function all producers delegate to — so oneshot never leaks a process exit
// code into an in-process return.
//
// Scope: unattended / headless ONLY. oneshot wires the existing auto-accept
// trust policy plus the AutoAcceptAnswer callback and never touches the
// interactive /dev/tty machinery, which stays in cmd/harness-wrapper's runOneShot.
//
// Parity is documented as MINUS `usage` (turnproto deliberately drops token
// usage — Go has no usage reader) and MINUS `transcript_entries` (an in-process
// caller reconstructs them from HarnessSessionID + WorkingDir via the
// pkg/transcript readers). Neither omission is a parity miss.
package oneshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// Config carries the fields oneshot needs to build a harness.TurnConfig, EXCEPT
// the input-policy fields oneshot owns itself (InputPolicy / OnInputRequest /
// AutoSkipCodexUpdateNotice). It mirrors the harness.TurnConfig literal the guest
// runStructuredRun builds today.
//
// Env / arg POLICY is a cmd/ concern and stays out of this library: the caller
// runs cleanedEnv (strip CLAUDECODE / CLAUDE_CODE_*) and applySandboxDefaults
// (the opt-in --sandbox-defaults injection) itself, then passes the
// ALREADY-CLEANED Env / Args in here. oneshot does not re-implement either.
type Config struct {
	// Harness is the short harness name ("claude", "codex"). Required.
	Harness string
	// BinaryPath is the resolved harness executable. Required.
	BinaryPath string
	// Args are passed verbatim to the harness after the "--" separator. The
	// caller has already applied any --sandbox-defaults injection.
	Args []string
	// Effort and Model are optional execution-mode knobs.
	Effort string
	Model  string
	// WorkingDir is the directory the turn runs in.
	WorkingDir string
	// Env is the harness process environment. The caller has already stripped
	// Claude Code's nesting markers via cleanedEnv.
	Env []string
	// Prompt is submitted as one user message. Required.
	Prompt string
}

// Outcome is the detailed typed result of one one-shot turn. It deliberately
// omits `usage` (no Go usage reader) and `transcript_entries` — an in-process
// caller reconstructs the transcript from HarnessSessionID + WorkingDir via the
// pkg/transcript readers, so it is a documented narrowing, not a parity miss.
type Outcome struct {
	// Status is the coarse typed status of the turn.
	Status turnproto.TurnStatus
	// Reply is the clean assistant reply text — populated ONLY on
	// StatusCompleted, empty otherwise (the structured-runner rule, matching TS
	// runOneShotDetailed).
	Reply string
	// Reason is failure detail; present on errored / startup_error.
	Reason string
	// HarnessSessionID is the harness's own session id ("" when unrecoverable,
	// e.g. a startup_error before any session opened).
	HarnessSessionID string
}

// RunOneShot drives ONE headless turn and returns its typed status.
//
// Error contract (mirroring pkg/env.RunStructuredTurn): all four CLASSIFIED
// outcomes — completed / errored / deadline / startup_error — return the
// classified status with a NIL error; the caller inspects Status, never the
// error, to tell the arms apart. A non-nil error is reserved for a genuinely
// unclassifiable / infra failure that produces no usable outcome (an invalid
// Config). This avoids double-encoding the outcome, so a caller never has to
// decide whether the status or the error wins.
//
// The per-turn deadline is the caller's responsibility via ctx: oneshot reads no
// environment and owns no timeout, so it observes ctx.Err() == DeadlineExceeded
// regardless of who set the deadline. The HARNESS_WRAPPER_RUN_TIMEOUT → ctx
// resolution stays a cmd/ concern.
func RunOneShot(ctx context.Context, cfg Config) (turnproto.TurnStatus, error) {
	out, err := RunOneShotDetailed(ctx, cfg)
	return out.Status, err
}

// RunOneShotDetailed is RunOneShot's detailed form: same error contract, but it
// also returns the clean reply (on completion), the failure reason, and the
// harness session id. See RunOneShot for the contract.
func RunOneShotDetailed(ctx context.Context, cfg Config) (Outcome, error) {
	if err := validateConfig(cfg); err != nil {
		return Outcome{}, err
	}

	res, err := harness.RunTurn(ctx, turnConfig(cfg))

	status, reason := Classify(res, err)
	out := Outcome{
		Status:           status,
		Reason:           reason,
		HarnessSessionID: res.Session.HarnessSessionID,
	}
	// Follow the STRUCTURED runner rule: reply text only on a completed turn.
	if status == turnproto.StatusCompleted {
		out.Reply = Reply(res)
	}
	return out, nil
}

// validateConfig rejects a Config that cannot produce a usable outcome. These
// are the only conditions that yield a non-nil error from RunOneShot* — every
// RunTurn result is otherwise classified into a status with a nil error.
func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Harness) == "" {
		return fmt.Errorf("oneshot: empty harness name")
	}
	if strings.TrimSpace(cfg.BinaryPath) == "" {
		return fmt.Errorf("oneshot: empty binary path")
	}
	if strings.TrimSpace(cfg.Prompt) == "" {
		return fmt.Errorf("oneshot: empty prompt")
	}
	return nil
}

// turnConfig builds the harness.TurnConfig for a headless one-shot turn: the
// caller-supplied fields from cfg, plus the input-policy fields oneshot owns —
// the auto-accept trust policy, the AutoAcceptAnswer callback, and Codex
// update-menu auto-skip. ExitAfterTurn is always true (clean, bounded run).
func turnConfig(cfg Config) harness.TurnConfig {
	return harness.TurnConfig{
		Harness:       cfg.Harness,
		BinaryPath:    cfg.BinaryPath,
		Args:          cfg.Args,
		Effort:        cfg.Effort,
		Model:         cfg.Model,
		WorkingDir:    cfg.WorkingDir,
		Env:           cfg.Env,
		Prompt:        cfg.Prompt,
		ExitAfterTurn: true,
		// Headless: no live client to answer Codex's update menu, so auto-Skip it
		// rather than wedge the run on the pending prompt.
		AutoSkipCodexUpdateNotice: true,
		InputPolicy: &chat.InputPolicy{
			ByKind: map[string]chat.Disposition{
				"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
			},
		},
		OnInputRequest: AutoAcceptAnswer,
	}
}

// Classify maps a RunTurn (result, err) pair to the protocol status and an
// optional reason. It is the (TurnResult, error) → status/reason core lifted
// VERBATIM from the guest classifyStructuredResult, MINUS the process exit code
// (that is turnproto.ExitCode) — no exit code leaks into an in-process return.
//
// It branches on the RETURNED ERROR for the deadline / errored / startup
// distinction — the fidelity fix, because a mid-turn transport failure returns
// Turn.State == "". The `err == nil` arm is the one exception: it reads
// res.Turn.State and carries the defensive "turn ended in unexpected state"
// fallback for any state other than complete. Turn.State is load-bearing THERE
// and only there.
func Classify(res harness.TurnResult, err error) (turnproto.TurnStatus, string) {
	switch {
	case err == nil:
		if res.Turn.State == chat.TurnStateComplete {
			return turnproto.StatusCompleted, ""
		}
		// RunTurn only returns nil on a completed turn; anything else is a
		// defensive fallback.
		return turnproto.StatusErrored, "turn ended in unexpected state"
	case errors.Is(err, context.DeadlineExceeded):
		return turnproto.StatusDeadline, ""
	case errors.Is(err, harness.ErrTurnErrored):
		reason := res.Turn.Reason
		if reason == "" {
			reason = "turn errored"
		}
		return turnproto.StatusErrored, reason
	case errors.Is(err, chat.ErrClosed):
		// Mid-turn transport failure: the events channel closed after the turn
		// had already started via conv.Send.
		return turnproto.StatusErrored, err.Error()
	default:
		// Either a mid-turn ev.Err (the turn had started, so a chat Session was
		// opened and snapshotTurnResult populated Session.ID) or a pre-turn
		// startup failure (chat.Open / AcquireControl / Send returns a ZERO
		// TurnResult). Distinguish by whether a session was ever opened.
		if res.Session.ID != "" {
			return turnproto.StatusErrored, err.Error()
		}
		return turnproto.StatusStartupError, err.Error()
	}
}

// Reply returns the assistant's final message, PREFERRING the harness's own
// transcript (res.History, transcript-backed) — authoritative and complete, with
// no TUI chrome and no risk of the screen-extraction dropping content. It falls
// back to the screen-derived res.Turn.Text only when no transcript was available
// (e.g. the harness session id could not be captured). Set
// HARNESS_WRAPPER_RUN_DEBUG=1 to log which source was used.
//
// The source label is taken from res.HistorySource, NOT from whether res.History
// happens to be non-empty: the store fallback also returns turns, so
// len(History) can't distinguish the transcript from the lossy screen scrape.
func Reply(res harness.TurnResult) string {
	debug := os.Getenv("HARNESS_WRAPPER_RUN_DEBUG") == "1"

	if res.HistorySource == chat.HistorySourceTranscript {
		if t := lastAssistant(res.History); strings.TrimSpace(t) != "" {
			if debug {
				fmt.Fprintln(os.Stderr, "harness-wrapper run: reply source = transcript")
			}
			return t
		}
	}
	if debug {
		fmt.Fprintln(os.Stderr, "harness-wrapper run: reply source = screen-extract (no transcript)")
	}
	// Screen-derived fallback: prefer the completing turn's extracted message; if
	// that's empty, use the last assistant turn the store recorded (also
	// screen-derived) so we never silently drop a reply we did capture.
	if strings.TrimSpace(res.Turn.Text) != "" {
		return res.Turn.Text
	}
	return lastAssistant(res.History)
}

// lastAssistant returns the text of the most recent non-empty assistant turn.
func lastAssistant(turns []chat.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == chat.RoleAssistant && strings.TrimSpace(turns[i].Text) != "" {
			return turns[i].Text
		}
	}
	return ""
}

// AffirmativeOption picks the "yes/accept/trust" option from an input request, so
// auto-answering a trust/confirm dialog proceeds rather than declines.
func AffirmativeOption(req chat.InputRequest) *chat.InputOption {
	for i := range req.Options {
		o := &req.Options[i]
		l := strings.ToLower(o.Label + " " + o.Alias + " " + o.ID)
		if strings.Contains(l, "yes") || strings.Contains(l, "trust") ||
			strings.Contains(l, "accept") || strings.Contains(l, "allow") ||
			strings.Contains(l, "proceed") {
			return o
		}
	}
	return nil
}

// AutoAcceptAnswer is the shared three-tier unattended fallback: pick the
// affirmative option, else the first option, else decline (false). Falling
// through to the first option (rather than false) for a has-options-but-no-
// affirmative prompt keeps an unattended run from failing with
// chat.ErrInputPending when nothing consumes the surfaced request.
func AutoAcceptAnswer(req chat.InputRequest) (chat.InputAnswer, bool) {
	if opt := AffirmativeOption(req); opt != nil {
		return chat.InputAnswer{OptionID: opt.ID}, true
	}
	if len(req.Options) > 0 {
		return chat.InputAnswer{OptionID: req.Options[0].ID}, true
	}
	return chat.InputAnswer{}, false
}
