// Package wrapper supervises an external CLI agent harness running under
// a pseudoterminal. It runs the harness, observes its output and
// lifecycle, and returns a normalized status when the harness exits or
// is terminated.
//
// It began with only terminal states (idle, failed, interrupted,
// unknown) and now also recognizes a small set of actionable,
// non-terminal harness states from recent output. The wrapper does not
// persist state; callers own persistence.
//
// Concurrency: the package is safe for multiple concurrent Run calls
// only in headless mode (non-TTY stdin/stdout). Concurrent foreground
// Run calls produce undefined behavior because they would compete for
// terminal control.
package wrapper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

// Config configures a single Run.
//
// Fields with zero values get sensible defaults documented per-field.
// Construct Config using keyed struct literals; positional initialization
// is unsupported and will break across versions.
type Config struct {
	// BinaryPath is the absolute or PATH-resolvable path to the harness
	// executable. Required.
	BinaryPath string

	// Args are passed verbatim to the harness as arguments after the
	// binary name.
	Args []string

	// WorkingDir is the harness's working directory. Defaults to the
	// current process's working directory.
	WorkingDir string

	// Env is the harness's environment. If nil, the current process
	// environment is inherited.
	Env []string

	// Stdin is the source forwarded into the harness's pseudoterminal
	// input. If nil, no input is forwarded; the harness will block if it
	// tries to read stdin.
	//
	// Pass *os.File (e.g. os.Stdin) for foreground TTY mode — the wrapper
	// will detect that and put the terminal into raw mode with SIGWINCH
	// forwarding. Pass any other io.Reader (e.g. strings.NewReader) for
	// headless input; raw-mode setup is skipped.
	Stdin io.Reader

	// Stdout is the sink that receives the harness's PTY output bytes.
	// Required (must be non-nil). Pass os.Stdout for foreground use, or
	// any io.Writer (file, bytes.Buffer, io.Discard) for headless capture.
	//
	// The wrapper writes raw PTY bytes including ANSI escapes. Callers
	// wanting a cleaned transcript should wrap the writer themselves.
	//
	// When both Stdin and Stdout are *os.File and both are TTYs, the
	// wrapper enables raw-mode passthrough and SIGWINCH forwarding;
	// otherwise it stays in headless mode.
	Stdout io.Writer

	// IdleQuiet is the duration of no output after which the wrapper
	// considers the harness "quiet." Quiet gates prompt detection
	// (waiting_for_input) and sets the classifier poll cadence.
	// Defaults to 15s.
	IdleQuiet time.Duration

	// IdleClassify is the duration of no output after which the wrapper
	// classifies the run as idle. Must be >= IdleQuiet. Defaults to 60s.
	IdleClassify time.Duration

	// StaleThreshold is the duration of no PTY output after which the
	// wrapper emits a non-terminal StatusStale SessionEvent and a
	// harness_stale trace event. Distinct from IdleClassify: stale is a
	// mid-run advisory, not a basis for terminating the harness — the
	// run continues and a fresh StatusStale fires after each subsequent
	// quiet stretch.
	//
	// Must be >= IdleClassify when both are non-zero. Defaults to 5
	// minutes. Set to a negative value (e.g. -1) to disable; the wrapper
	// will then emit no StatusStale events regardless of quiet duration.
	StaleThreshold time.Duration

	// WaitDelay is how long to wait after sending SIGTERM before
	// escalating to SIGKILL on context cancellation. Defaults to 5s.
	WaitDelay time.Duration

	// Trace receives diagnostic events emitted by the wrapper. If nil,
	// events are discarded.
	//
	// Trace is for observability, not control flow. Callers should not
	// make decisions based on trace event ordering or presence; the
	// trace vocabulary is not part of the API stability surface.
	Trace trace.Emitter

	// Harness names a built-in per-harness classifier (e.g. "claude",
	// "codex"). If both Harness and Classifier are set, Classifier
	// wins. Unknown names fall through to the default classifier.
	Harness string

	// Effort requests a harness-specific reasoning effort level for this
	// run. Empty leaves the harness default. Supported harnesses map this
	// to their native controls (for example, Claude Code --effort and
	// Codex model_reasoning_effort).
	Effort string

	// Model requests a specific model for this run. Empty leaves the harness
	// default. Supported harnesses map this to their native flag (Claude Code
	// --model, Codex -c model="…").
	Model string

	// PermissionMode requests a launch-time permission posture for this run.
	// Empty leaves the harness default. Supported harnesses map this to their
	// native flags (Claude Code --permission-mode, Codex -s/--sandbox plus
	// -a/--ask-for-approval); see argsWithHarnessPermissionMode for the full
	// rung -> argv mapping table.
	//
	// Accepted values are the canonical rungs ("plan", "manual", "ask",
	// "auto", "bypass") and the native spellings of the TARGET harness —
	// mixing vocabularies across harnesses is an error, never a silent
	// no-op. "plan" is rejected for codex (no launch-time flag exists).
	PermissionMode string

	// Classifier inspects recent harness output and produces actionable
	// status classifications (blocked_by_cost, retry_later,
	// waiting_for_input). If nil, a built-in classifier matching the
	// Harness field — or, failing that, a generic cost/quota
	// classifier — is used.
	Classifier Classifier

	// OnLine, if non-nil, is the DURABLE internal line tap. It receives every
	// complete line of the harness's RAW PTY output, in order, with no drops:
	// it is invoked synchronously in the PTY read loop, so a slow OnLine
	// back-pressures the harness (slows it) rather than losing a line. This is
	// the load-bearing tap for session-id capture and live transcript parsing,
	// which must not drop.
	//
	// Framing: bytes are split on '\n'; a trailing '\r' (from '\r\n') is
	// trimmed; a final unterminated line is flushed once when the PTY closes.
	// There is NO line-length cap — a multi-MB single line is delivered whole.
	// Bytes are RAW: ANSI/control sequences are NOT stripped, so the consumer
	// (the pkg/harness orchestrator, feeding ExtractSessionID / ParseStreamLine)
	// must tolerate non-JSON / ANSI-polluted lines and skip them.
	//
	// This is the low-level supervisor tap and is deliberately distinct from a
	// best-effort, drop-under-load display callback (which belongs one layer up,
	// in pkg/harness). pkg/wrapper stays stateless: it owns no persistence and
	// does not import pkg/harness.
	OnLine func(line string)
}

// Status is the normalized run status returned by the wrapper.
type Status string

const (
	// StatusIdle indicates the harness exited cleanly or its output
	// remained unchanged past the configured classification threshold
	// with no actionable state detected.
	StatusIdle Status = "idle"

	// StatusFailed indicates the harness exited with a non-zero code.
	StatusFailed Status = "failed"

	// StatusBlockedByCost indicates the harness cannot continue until
	// budget, credits, quota, or rate limits allow continuation.
	StatusBlockedByCost Status = "blocked_by_cost"

	// StatusRetryLater indicates the harness hit a transient condition
	// that the engine should re-attempt after a backoff. It is reported
	// by classifiers when they recognize transient API errors, network
	// blips, or "try again later" prompts.
	StatusRetryLater Status = "retry_later"

	// StatusAPIError indicates the harness's upstream model API returned
	// a recognized error (HTTP 4xx/5xx, transport failure). Unlike
	// StatusRetryLater this is non-terminal: the wrapper keeps the
	// harness alive. The accompanying SessionEvent carries HTTPCode
	// (0 when the harness's output did not include a numeric code,
	// e.g. transport errors) and RetryAfter (0 when no retry hint was
	// parseable). External clients subscribe to Session.Events and
	// dispatch on HTTPCode to attach per-error behavior.
	StatusAPIError Status = "api_error"

	// StatusWaitingForInput indicates the harness is paused at an
	// interactive prompt and needs a human (or attached client) to
	// answer. Unlike the other actionable statuses, it is reported
	// mid-run: the wrapper does not terminate the process.
	StatusWaitingForInput Status = "waiting_for_input"

	// StatusStale is a non-terminal mid-run advisory: the harness has
	// produced no PTY output for cfg.StaleThreshold and may need
	// attention, but it is still alive and has not been classified as
	// idle, blocked, or otherwise actionable. StatusStale never appears
	// in Result.Status (which is the terminal status reported by Wait);
	// it is only emitted on Session.Events() and as a harness_stale
	// trace event.
	StatusStale Status = "stale"

	// StatusInterrupted indicates the harness was terminated by signal,
	// either because the caller cancelled the context or because the
	// wrapper forwarded a foreground interrupt.
	StatusInterrupted Status = "interrupted"

	// StatusUnknown indicates the wrapper could not classify the run
	// outcome. Result.Reason should explain why.
	StatusUnknown Status = "unknown"

	// StatusBinaryNotFound indicates the configured harness binary was
	// not present on PATH (or at the configured BinaryPath). This is a
	// terminal status reported by Run when Start returns
	// ErrBinaryNotFound: ExitCode is -1, Reason carries the underlying
	// "executable file not found" message. Consumers should treat this
	// as non-retryable until the binary becomes available — burning
	// restart budget against a missing CLI is wasted work.
	StatusBinaryNotFound Status = "binary_not_found"
)

// Result describes the outcome of a Run.
type Result struct {
	// Status is the normalized outcome.
	Status Status

	// Class is the canonical harness-output error taxonomy for the run.
	// It carries the terminal classification's class, or — when the run
	// exited Failed without a terminal classification — the last
	// meaningful (non-ErrNone) class seen mid-run (e.g. a non-terminal
	// API error that preceded a Failed exit). ErrNone for clean/idle/
	// interrupted outcomes.
	Class ErrorClass

	// ExitCode is the harness process's exit code, or 128+signum if the
	// process was terminated by a signal. -1 if the process never
	// started.
	ExitCode int

	// Signal is the signal name (e.g. "terminated") if the process was
	// terminated by signal, empty otherwise.
	Signal string

	// Reason is a short human-readable description, populated for
	// Failed, Interrupted, and Unknown statuses. Not stable for parsing.
	Reason string

	// PID is the harness process ID while it was running, 0 if it never
	// started.
	PID int

	// StartedAt is when the harness process started.
	StartedAt time.Time

	// EndedAt is when the harness process exited or was terminated.
	EndedAt time.Time

	// LastOutputAt is the time of the most recent byte received from
	// the harness PTY. Zero if no output was observed.
	LastOutputAt time.Time
}

// Sentinel errors. Callers can use errors.Is to distinguish wrapper-level
// failures from harness-level outcomes. A non-nil err from Run always
// means the wrapper itself failed; harness outcomes are always returned
// via Result with err == nil.
var (
	ErrInvalidConfig  = errors.New("wrapper: invalid config")
	ErrBinaryNotFound = errors.New("wrapper: binary not found")
	ErrPTYAllocation  = errors.New("wrapper: pty allocation failed")
	ErrPTYRead        = errors.New("wrapper: pty read failed")
)

// Run starts the configured harness under a pseudoterminal, supervises
// it until it exits or ctx is cancelled, and returns the normalized
// outcome. It is a blocking convenience wrapper around Start+Wait
// preserved for callers that don't need a live session handle.
//
// Errors are returned only when the wrapper itself fails to do its job
// (invalid configuration, missing binary, PTY allocation failure, IO
// errors on the master fd). Harness-level outcomes — clean exit,
// non-zero exit, signal termination, idle classification — are always
// reported through the returned Result with a nil error.
//
// Context cancellation is handled by sending the harness a termination
// signal. The returned Result will have Status == StatusInterrupted;
// ctx.Err() is not propagated as the returned error.
func Run(ctx context.Context, cfg Config) (Result, error) {
	s, err := Start(ctx, cfg)
	if err != nil {
		res := Result{ExitCode: -1}
		if errors.Is(err, ErrBinaryNotFound) {
			res.Status = StatusBinaryNotFound
			res.Reason = err.Error()
		}
		return res, err
	}
	return s.Wait()
}

// Start launches the configured harness under a pseudoterminal and
// returns a live Session. Unlike Run, Start returns immediately; the
// caller observes lifecycle through Session.Events / Session.Snapshot
// and retrieves the final outcome via Session.Wait.
//
// Errors are returned only when the wrapper itself fails to start
// (invalid configuration, missing binary, PTY allocation failure).
// Once Start has returned a non-nil Session, every harness outcome
// flows through Wait with a nil error.
func Start(ctx context.Context, cfg Config) (*Session, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	applyDefaults(&cfg)
	cfg.Args = argsWithHarnessEffort(cfg.Harness, cfg.Args, cfg.Effort)
	cfg.Args = argsWithHarnessModel(cfg.Harness, cfg.Args, cfg.Model)
	cfg.Args = argsWithHarnessPermissionMode(cfg.Harness, cfg.Args, cfg.PermissionMode)
	return startSession(ctx, cfg)
}

func validateConfig(cfg *Config) error {
	if cfg.BinaryPath == "" {
		return fmt.Errorf("%w: BinaryPath is required", ErrInvalidConfig)
	}
	if cfg.Stdout == nil {
		return fmt.Errorf("%w: Stdout is required", ErrInvalidConfig)
	}
	if cfg.IdleClassify > 0 && cfg.IdleQuiet > 0 && cfg.IdleClassify < cfg.IdleQuiet {
		return fmt.Errorf("%w: IdleClassify (%v) must be >= IdleQuiet (%v)", ErrInvalidConfig, cfg.IdleClassify, cfg.IdleQuiet)
	}
	if cfg.StaleThreshold > 0 && cfg.IdleClassify > 0 && cfg.StaleThreshold < cfg.IdleClassify {
		return fmt.Errorf("%w: StaleThreshold (%v) must be >= IdleClassify (%v)", ErrInvalidConfig, cfg.StaleThreshold, cfg.IdleClassify)
	}
	if cfg.Effort != "" {
		if !isSupportedEffort(cfg.Effort) {
			return fmt.Errorf("%w: Effort must be one of low, medium, high, xhigh, max", ErrInvalidConfig)
		}
		if !harnessSupportsEffort(cfg.Harness) {
			return fmt.Errorf("%w: Effort is only supported for claude and codex harnesses", ErrInvalidConfig)
		}
	}
	if err := validatePermissionMode(cfg); err != nil {
		return err
	}
	return nil
}

// validatePermissionMode rejects, BEFORE the harness process is launched, every
// PermissionMode that cannot be honoured faithfully. A value the caller believes
// restricts the harness must never be dropped on the floor: each case below is
// chosen over a silent downgrade, which would launch a LESS restricted harness
// than the caller asked for.
func validatePermissionMode(cfg *Config) error {
	mode := cfg.PermissionMode
	if mode == "" {
		return nil
	}
	if !harnessSupportsPermissionMode(cfg.Harness) {
		return fmt.Errorf("%w: PermissionMode is only supported for claude and codex harnesses", ErrInvalidConfig)
	}
	if normHarness(cfg.Harness) == "codex" && mode == permissionModePlan {
		// codex has no launch-time equivalent of the plan rung, and a no-op
		// would launch codex with NO launch-time restriction at all for a
		// caller who explicitly asked for the non-executing rung.
		return fmt.Errorf("%w: permission mode %q is not supported by the codex harness (no launch-time flag; use /plan after launch)", ErrInvalidConfig, mode)
	}
	if !isSupportedPermissionMode(cfg.Harness, mode) {
		return fmt.Errorf("%w: PermissionMode %q is not valid for the %s harness", ErrInvalidConfig, mode, normHarness(cfg.Harness))
	}
	// Contradictory argv. A bare --permission-mode / -s / -a already in Args is
	// NOT handled here: that is the caller restating the same axis, so plain
	// last-wins suppression (in argsWithHarnessPermissionMode) is right. Only a
	// hard bypass flag paired with a non-bypass mode is contradictory, because
	// resolving it toward suppression would silently launch an UNRESTRICTED
	// harness.
	//
	// This fires only on caller-supplied HarnessArgs. --sandbox-defaults appends
	// SkipPermissionsFlag in run/structured-run before wrapper.Start sees args,
	// but the only surviving --sandbox-defaults + --permission-mode combination
	// is bypass, where the CLI skips the arg append. Do not mistake this for a
	// second composition check.
	bypassMode := IsBypassPermissionMode(mode)
	if normHarness(cfg.Harness) == "codex" {
		bypassMode = isCodexBypassMode(mode)
	}
	if !bypassMode {
		for _, flag := range BypassEnablingFlags(cfg.Harness) {
			if argsContainAnyFlag(cfg.Args, flag) {
				return fmt.Errorf("%w: PermissionMode %q contradicts %s in Args", ErrInvalidConfig, mode, flag)
			}
		}
	}
	return nil
}

// normHarness normalizes a harness name for switch matching. The chat layer
// uses adapter-style names ("claude-code") while the CLI/effort code
// historically switched on short names ("claude"); normalizing lets both
// reach the same per-harness translation. Mirrors classifier.go.
func normHarness(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

// harnessClaudeCode is the adapter-style name for the Claude Code harness.
const harnessClaudeCode = "claude-code"

func harnessSupportsEffort(harness string) bool {
	switch normHarness(harness) {
	case "claude", harnessClaudeCode, "codex":
		return true
	default:
		return false
	}
}

func isSupportedEffort(effort string) bool {
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func argsWithHarnessEffort(harness string, args []string, effort string) []string {
	if effort == "" {
		return args
	}
	switch normHarness(harness) {
	case "claude", harnessClaudeCode:
		if argsContainFlag(args, "--effort") {
			return args
		}
		return prependArgs(args, "--effort", effort)
	case "codex":
		if argsContainConfigKey(args, "model_reasoning_effort") {
			return args
		}
		return prependArgs(args, "-c", "model_reasoning_effort=\""+codexEffort(effort)+"\"")
	default:
		return args
	}
}

func codexEffort(effort string) string {
	if effort == "max" {
		return "xhigh"
	}
	return effort
}

// argsWithHarnessModel prepends a per-harness model override (Claude Code
// --model, Codex -c model="…"). Empty leaves the harness default. An explicit
// model flag already in args wins (so spec.harnessArgs beats the mode policy).
func argsWithHarnessModel(harness string, args []string, model string) []string {
	if model == "" {
		return args
	}
	switch normHarness(harness) {
	case "claude", harnessClaudeCode:
		if argsContainFlag(args, "--model") {
			return args
		}
		return prependArgs(args, "--model", model)
	case "codex":
		if argsContainConfigKey(args, "model") {
			return args
		}
		return prependArgs(args, "-c", "model=\""+model+"\"")
	default:
		return args
	}
}

// SkipPermissionsFlag is Claude Code's blanket permission-bypass flag. It is a
// flag, not a PermissionMode value: it is never emitted by
// argsWithHarnessPermissionMode and is never accepted as a mode, only
// recognized as an already-present token in Config.Args.
const SkipPermissionsFlag = "--dangerously-skip-permissions"

// codexBypassFlag is codex's blanket approval+sandbox bypass flag. Like
// SkipPermissionsFlag it is recognized in Args but never emitted.
const codexBypassFlag = "--dangerously-bypass-approvals-and-sandbox"

// Canonical permission rungs, ordered least to most permissive. These are
// harness-independent; each supported harness translates them to its own
// native flags in argsWithHarnessPermissionMode.
const (
	permissionModePlan   = "plan"
	permissionModeManual = "manual"
	permissionModeAsk    = "ask"
	permissionModeAuto   = "auto"
	permissionModeBypass = "bypass"
)

// IsBypassPermissionMode reports whether mode resolves to claude-code's
// bypassPermissions directive — the canonical rung "bypass" and its
// claude-native spelling "bypassPermissions", and NOTHING else.
//
// Four call sites, all of which need exactly this question:
//  1. cmd/harness-wrapper.applySandboxDefaults — compose (env half only).
//  2. cmd/harness-wrapper.parseHarnessWrapperArgs — the --sandbox-defaults
//     exclusion check, which is deliberately harness-INDEPENDENT.
//  3. pkg/wrapper.validateConfig — the contradictory-argv rejection.
//  4. pkg/env.RunStructuredTurn — the host-side mirror of call site 2,
//     hoisted so a contradictory config never spends a container round-trip.
//
// codex's "danger-full-access" is deliberately NOT included even though it is
// codex's bypass-equivalent: call site 2 runs before the harness is known, so
// treating it as bypass would let `--sandbox-defaults --permission-mode
// danger-full-access codex --` slip past the exclusion check. codex's own
// bypass handling lives in isCodexBypassMode.
func IsBypassPermissionMode(mode string) bool {
	return mode == permissionModeBypass || mode == claudeModeBypassPermissions
}

// isCodexBypassMode reports whether mode leaves codex unrestricted: the
// canonical rung "bypass" and codex's native "danger-full-access" sandbox
// value. Unexported on purpose — it keeps codex vocabulary out of cmd/, which
// only ever needs the harness-independent IsBypassPermissionMode.
func isCodexBypassMode(mode string) bool {
	return mode == permissionModeBypass || mode == codexSandboxDangerFullAccess
}

// Claude Code's native --permission-mode values (claude-code 2.1.217).
const (
	claudeModeAcceptEdits       = "acceptEdits"
	claudeModeDontAsk           = "dontAsk"
	claudeModeBypassPermissions = "bypassPermissions"
)

// codex's native -s/--sandbox values (codex-cli 0.144.5).
const (
	codexSandboxReadOnly         = "read-only"
	codexSandboxWorkspaceWrite   = "workspace-write"
	codexSandboxDangerFullAccess = "danger-full-access"
)

func harnessSupportsPermissionMode(harness string) bool {
	switch normHarness(harness) {
	case "claude", harnessClaudeCode, "codex":
		return true
	default:
		return false
	}
}

// isSupportedPermissionMode accepts a value only if it is a canonical rung or a
// native spelling valid for the TARGET harness. A native spelling belonging to
// the other harness is rejected rather than silently ignored, because a mode
// the caller believes restricts the harness must never be dropped on the floor.
//
// "plan", "manual" and "auto" are simultaneously canonical rungs and
// claude-native spellings; they resolve to the same argv either way, so there
// is no ambiguity.
func isSupportedPermissionMode(harness, mode string) bool {
	switch mode {
	case permissionModePlan, permissionModeManual, permissionModeAsk, permissionModeAuto, permissionModeBypass:
		return true
	}
	switch normHarness(harness) {
	case "claude", harnessClaudeCode:
		switch mode {
		case claudeModeAcceptEdits, claudeModeDontAsk, claudeModeBypassPermissions:
			return true
		}
	case "codex":
		switch mode {
		case codexSandboxReadOnly, codexSandboxWorkspaceWrite, codexSandboxDangerFullAccess:
			return true
		}
	}
	return false
}

// argsWithHarnessPermissionMode prepends a per-harness launch-time permission
// posture. Empty leaves the harness default; an explicit permission flag
// already in args wins. The canonical rung -> argv mapping:
//
//	rung     claude / claude-code                  codex
//	plan     --permission-mode plan                (rejected in validateConfig)
//	manual   --permission-mode manual              -s read-only      -a untrusted
//	ask      --permission-mode acceptEdits         -s workspace-write -a on-request
//	auto     --permission-mode auto                -s workspace-write -a never
//	bypass   --permission-mode bypassPermissions   -s danger-full-access -a never
//
// Native spellings pass through verbatim for their own harness (claude's
// acceptEdits/dontAsk/bypassPermissions; codex's sandbox values, which set the
// -s axis ONLY and leave -a untouched). Cross-harness spellings never reach
// here — validateConfig rejects them.
//
// codex 0.144.5 REMOVED --full-auto and hard-errors on it; nothing here may
// ever emit it.
func argsWithHarnessPermissionMode(harness string, args []string, mode string) []string {
	if mode == "" {
		return args
	}
	switch normHarness(harness) {
	case "claude", harnessClaudeCode:
		// --permission-mode: plain last-wins suppression — the caller is
		// restating the same axis.
		//
		// SkipPermissionsFlag: reachable ONLY when the mode is bypass-class.
		// Every other mode paired with that flag is rejected by validateConfig
		// before Start reaches injection, so this arm exists purely so that
		// bypass + an explicitly-passed --dangerously-skip-permissions yields
		// exactly ONE permission directive in argv.
		if argsContainAnyFlag(args, "--permission-mode", SkipPermissionsFlag) {
			return args
		}
		// claude's parser is last-wins, so an explicit later flag would still
		// win at the harness's own arg parsing — a second line of defence
		// behind the suppression check above, not a substitute for it.
		return prependArgs(args, "--permission-mode", claudePermissionMode(mode))
	case "codex":
		// Whole-directive wins: if ANY permission-axis flag is already present
		// we skip injection entirely on BOTH axes. A caller who set only -s
		// keeps their argv exactly as written rather than receiving a
		// half-injected combination of their sandbox and our approval policy.
		if argsContainAnyFlag(args, "-s", "--sandbox", "-a", "--ask-for-approval", codexBypassFlag) {
			return args
		}
		// Flag position vs subcommands — SETTLED, do not re-derive. -s / -a are
		// accepted AHEAD of a codex subcommand; both forms exit 0 on
		// codex-cli 0.144.5 (probed twice, 2026-07-22):
		//
		//	codex -s read-only exec --help                        -> exit 0
		//	codex -s workspace-write -a on-request resume --help  -> exit 0
		//
		// The live subcommand path in this repo is `resume`, not `exec`:
		// pkg/turns/harness/codex/codex.go:153-155 ((*Adapter).ResumeArgs
		// returns {"resume", harnessSessionID}) feeds pkg/chat/conversation.go:326-330
		// ("Prepend the resume fragment AHEAD of the caller's args"), which
		// hands the result straight to wrapper.Config.Args. prependArgs is
		// therefore safe for both the bare-TUI and the subcommand shapes; no
		// subcommand-detection branch is needed.
		//
		// clap is last-wins too, so an explicit later flag would still win at
		// codex's own arg parsing — again a second line of defence, not a
		// substitute for the suppression check above.
		sandbox, approval := codexPermissionMode(mode)
		if approval == "" {
			// A codex-native sandbox value sets the -s axis ONLY.
			return prependArgs(args, "-s", sandbox)
		}
		return prependArgs(args, "-s", sandbox, "-a", approval)
	default:
		return args
	}
}

// claudePermissionMode maps an accepted mode to claude-code's native
// --permission-mode value. Native spellings map to themselves.
func claudePermissionMode(mode string) string {
	switch mode {
	case permissionModeAsk:
		return claudeModeAcceptEdits
	case permissionModeBypass:
		return claudeModeBypassPermissions
	default:
		// plan / manual / auto are both canonical rungs and claude-native
		// spellings; acceptEdits / dontAsk / bypassPermissions are native.
		return mode
	}
}

// codexPermissionMode maps an accepted mode to codex's -s sandbox value and -a
// approval policy. A codex-native sandbox value returns an empty approval,
// meaning "set the -s axis only, leave -a untouched".
func codexPermissionMode(mode string) (sandbox, approval string) {
	switch mode {
	case permissionModeManual:
		return codexSandboxReadOnly, "untrusted"
	case permissionModeAsk:
		return codexSandboxWorkspaceWrite, "on-request"
	case permissionModeAuto:
		return codexSandboxWorkspaceWrite, "never"
	case permissionModeBypass:
		return codexSandboxDangerFullAccess, "never"
	default:
		return mode, ""
	}
}

// PermissionRungs returns the canonical rungs, ordered least to most
// permissive — the same order the unexported consts are declared in.
//
// A fresh slice per call: callers (pkg/chat builds a permission ring out of it)
// may sort, truncate or reverse the result without corrupting a later call.
func PermissionRungs() []string {
	return []string{
		permissionModePlan,
		permissionModeManual,
		permissionModeAsk,
		permissionModeAuto,
		permissionModeBypass,
	}
}

// MorePermissive reports whether rung a is strictly more permissive than b,
// by index in PermissionRungs.
//
// Unknown rungs are never more permissive (fail closed): an empty string, a
// native spelling ("acceptEdits", "danger-full-access") or a typo yields false
// for a, so a caller asking "may I stay where I am?" never gets a yes it did
// not earn. Note b being unknown ALSO yields false, so the answer is false
// whenever either side is not a canonical rung.
func MorePermissive(a, b string) bool {
	ai, bi := rungIndex(a), rungIndex(b)
	if ai < 0 || bi < 0 {
		return false
	}
	return ai > bi
}

// rungIndex returns the position of rung in PermissionRungs, or -1 when rung is
// not a canonical rung.
func rungIndex(rung string) int {
	for i, r := range PermissionRungs() {
		if r == rung {
			return i
		}
	}
	return -1
}

// BypassEnablingFlags returns the harness argv flags that, when present at
// launch, leave the harness able to reach the bypass rung. Single source of
// truth for validatePermissionMode's contradiction check and pkg/chat's
// ring-length calculation.
//
// Only two such flags exist: claude's SkipPermissionsFlag and codex's
// --dangerously-bypass-approvals-and-sandbox. Harnesses with no launch-time
// permission axis at all return nil.
func BypassEnablingFlags(harness string) []string {
	switch normHarness(harness) {
	case "claude", harnessClaudeCode:
		return []string{SkipPermissionsFlag}
	case "codex":
		return []string{codexBypassFlag}
	default:
		return nil
	}
}

// EffectiveLaunchRung reports the rung the harness ACTUALLY launched with,
// given the caller's argv and the Config.PermissionMode knob — i.e. it replays
// argsWithHarnessPermissionMode's suppression rule rather than trusting the
// knob alone. Unlike argsContainAnyFlag, which answers PRESENCE only, this
// extracts the VALUE from both "--permission-mode=x" and the separated
// "--permission-mode x" form and normalizes native spellings (acceptEdits ->
// ask, bypassPermissions -> bypass, codex's -s values -> their rungs).
//
// A bypass-enabling flag (SkipPermissionsFlag, codexBypassFlag) in argv is
// itself reported as a definite bypass: it suppresses injection AND leaves the
// harness unrestricted, so there is nothing unknown about the result.
//
// Returns "" when argv carries a permission flag whose value cannot be resolved
// (a trailing flag with no operand, an unrecognized spelling), when only
// codex's -a axis is set (which suppresses injection but leaves the sandbox at
// the harness default), and when neither argv nor mode says anything. "" means
// UNKNOWN, never "default" — callers must not treat it as a definite non-bypass
// answer.
//
// Passing ALREADY-INJECTED args is safe — the function is idempotent over
// argsWithHarnessPermissionMode, because injection self-suppresses: once the
// axis is in argv, argsContainAnyFlag short-circuits the second pass and the
// argv arm reads back the value that was injected. Formally,
// EffectiveLaunchRung(h, argsWithHarnessPermissionMode(h, args, mode), mode)
// == EffectiveLaunchRung(h, args, mode).
func EffectiveLaunchRung(harness string, args []string, mode string) string {
	switch normHarness(harness) {
	case "claude", harnessClaudeCode:
		if argsContainAnyFlag(args, SkipPermissionsFlag) {
			return permissionModeBypass
		}
		if value, ok := flagValue(args, "--permission-mode"); ok {
			// argv wins, mirroring the suppression rule.
			return claudeRung(value)
		}
		return claudeRung(mode)
	case "codex":
		if argsContainAnyFlag(args, codexBypassFlag) {
			return permissionModeBypass
		}
		if value, ok := flagValue(args, "-s", "--sandbox"); ok {
			return codexSandboxRung(value)
		}
		if argsContainAnyFlag(args, "-a", "--ask-for-approval") {
			// Whole-directive suppression fired with no sandbox value to read.
			return ""
		}
		return codexRung(mode)
	default:
		// No launch-time permission axis: nothing was injected and nothing in
		// argv is ours to interpret.
		return ""
	}
}

// claudeRung normalizes a canonical rung or a claude-native --permission-mode
// value to a canonical rung. The inverse of claudePermissionMode, except that
// claudeModeDontAsk has NO canonical rung and so reports unknown ("") rather
// than being guessed into ask or auto.
func claudeRung(value string) string {
	switch value {
	case claudeModeAcceptEdits:
		return permissionModeAsk
	case claudeModeBypassPermissions:
		return permissionModeBypass
	}
	if rungIndex(value) >= 0 {
		return value
	}
	return ""
}

// codexRung normalizes the Config.PermissionMode knob for codex: canonical
// rungs pass through, codex-native sandbox values map to their rung.
func codexRung(value string) string {
	if rungIndex(value) >= 0 {
		return value
	}
	return codexSandboxRung(value)
}

// codexSandboxRung maps a codex -s/--sandbox value to its canonical rung —
// the inverse of codexPermissionMode's sandbox half. Anything else is unknown.
func codexSandboxRung(value string) string {
	switch value {
	case codexSandboxReadOnly:
		return permissionModeManual
	case codexSandboxWorkspaceWrite:
		return permissionModeAsk
	case codexSandboxDangerFullAccess:
		return permissionModeBypass
	default:
		return ""
	}
}

// flagValue extracts the operand of the LAST occurrence of any of flags, in
// each of the spellings argsContainAnyFlag recognizes: the attached long form
// ("--permission-mode=plan"), clap's attached short form ("-sread-only") and
// the separated form ("--permission-mode plan"), which argsContainAnyFlag
// cannot read at all.
//
// LAST occurrence, not first, mirroring claude's and clap's own last-wins
// parsers (see the notes in argsWithHarnessPermissionMode): on a duplicated
// flag the harness launches at the LATER value, so reporting the earlier one
// would under-report permissiveness — the one direction a safety field must
// never fail in. Injection is unaffected: it only runs when argv carries none
// of these flags and then emits exactly one, so first and last coincide.
//
// ok reports PRESENCE, exactly as argsContainAnyFlag would. A present-but-
// unreadable flag (trailing, no operand) returns ("", true) — the caller must
// distinguish that from ("", false) only if absence and unknown differ to it;
// EffectiveLaunchRung maps both to "".
func flagValue(args []string, flags ...string) (string, bool) {
	value, found := "", false
	for i, arg := range args {
		for _, flag := range flags {
			switch {
			case arg == flag:
				if i+1 < len(args) {
					value = args[i+1]
				} else {
					value = ""
				}
			case strings.HasPrefix(arg, flag+"="):
				value = strings.TrimPrefix(arg, flag+"=")
			case isShortFlag(flag) && len(arg) > 2 && strings.HasPrefix(arg, flag):
				value = arg[len(flag):]
			default:
				continue
			}
			found = true
		}
	}
	return value, found
}

func prependArgs(args []string, prefix ...string) []string {
	out := make([]string, 0, len(args)+len(prefix))
	out = append(out, prefix...)
	out = append(out, args...)
	return out
}

func argsContainFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// argsContainAnyFlag reports whether args already carry any of flags, in any of
// the three spellings a caller can write: the bare token ("-s"), the attached
// long form ("--sandbox=read-only"), and clap's attached SHORT form
// ("-sread-only"). Sibling of argsContainFlag, which matches the exact token
// only.
//
// The attached-short-form rule is a PREFIX match, so it also matches any
// hypothetical single-dash token that merely begins with "-s"/"-a" (e.g.
// "-auto-something"). codex/clap expose no such flag today, and the failure
// direction is one-sided: a false positive SUPPRESSES injection (the caller's
// argv is left exactly as written) rather than emitting a second -s/-a. It is
// still a silent drop of the requested mode, so it is called out here.
func argsContainAnyFlag(args []string, flags ...string) bool {
	for _, arg := range args {
		for _, flag := range flags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return true
			}
			if isShortFlag(flag) && len(arg) > 2 && strings.HasPrefix(arg, flag) {
				return true
			}
		}
	}
	return false
}

// isShortFlag reports whether flag is a single-dash, single-letter flag
// (matching ^-[a-z]$) — the only shape for which clap accepts an attached
// value with no separator.
func isShortFlag(flag string) bool {
	return len(flag) == 2 && flag[0] == '-' && flag[1] >= 'a' && flag[1] <= 'z'
}

func argsContainConfigKey(args []string, key string) bool {
	for i, arg := range args {
		if arg == "-c" || arg == "--config" {
			if i+1 < len(args) && configArgHasKey(args[i+1], key) {
				return true
			}
			continue
		}
		if strings.HasPrefix(arg, "-c") && len(arg) > len("-c") && configArgHasKey(arg[len("-c"):], key) {
			return true
		}
		if strings.HasPrefix(arg, "--config=") && configArgHasKey(strings.TrimPrefix(arg, "--config="), key) {
			return true
		}
	}
	return false
}

func configArgHasKey(arg, key string) bool {
	arg = strings.TrimSpace(arg)
	return arg == key || strings.HasPrefix(arg, key+"=")
}

func applyDefaults(cfg *Config) {
	if cfg.IdleQuiet == 0 {
		cfg.IdleQuiet = 15 * time.Second
	}
	if cfg.IdleClassify == 0 {
		cfg.IdleClassify = 60 * time.Second
	}
	if cfg.StaleThreshold == 0 {
		cfg.StaleThreshold = 5 * time.Minute
	}
	if cfg.WaitDelay == 0 {
		cfg.WaitDelay = 5 * time.Second
	}
	if cfg.Trace == nil {
		cfg.Trace = trace.Discard
	}
}
