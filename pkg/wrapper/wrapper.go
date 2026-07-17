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
	return nil
}

// normHarness normalizes a harness name for switch matching. The chat layer
// uses adapter-style names ("claude-code") while the CLI/effort code
// historically switched on short names ("claude"); normalizing lets both
// reach the same per-harness translation. Mirrors classifier.go.
func normHarness(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

func harnessSupportsEffort(harness string) bool {
	switch normHarness(harness) {
	case "claude", "claude-code", "codex":
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
	case "claude", "claude-code":
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
	case "claude", "claude-code":
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
