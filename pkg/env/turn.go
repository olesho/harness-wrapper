// Package env hosts the HOST-side structured-turn client: the analog of
// meta-harness src/env/turn.ts runStructuredTurn(ctx, ws: Workspace, cfg).
//
// RunStructuredTurn drives ONE turn over a B1 Workspace (internal/env.Workspace)
// — the exec + upload/download transport — NOT a Containment (which is an
// orthogonal policy decorator contributing a ContainmentLayer and exposes no
// exec). The round-trip it performs mirrors MH exactly:
//
//	upload(prompt) → exec(harness-wrapper structured-run) → parse last JSON line
//	→ (optional) download(transcript) → return *turnproto.StructuredTurnResult
//
// The guest runner is the `harness-wrapper structured-run` subcommand: it drives
// the real interactive harness, then emits EXACTLY ONE turnproto.StructuredTurnResult
// JSON object as the LAST stdout line and exits with the protocol exit code. The
// host parses that last line via turnproto.ParseLastJSONLine, which tolerates
// harness banner / log noise printed before it.
package env

import (
	"context"
	"fmt"
	"os"
	"strings"

	ienv "github.com/olesho/harness-wrapper/internal/env"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// defaultRunner is the guest command that invokes the structured-run subcommand
// when StructuredTurnConfig.Runner is empty. Tests point Runner at a freshly
// built harness-wrapper binary for a hermetic round-trip.
var defaultRunner = []string{"harness-wrapper", "structured-run"}

// defaultPromptName is the guest filename the prompt is uploaded to under the
// workspace tmp path when StructuredTurnConfig.PromptGuestPath is empty.
const defaultPromptName = "structured-turn-prompt.txt"

// StructuredTurnConfig are the inputs for one host-driven structured turn. It
// mirrors the guest structured-run invocation shape
// (`[wrapper flags] <harness> -- <harness args>`) plus the transport-level
// upload/download and env-override seams.
type StructuredTurnConfig struct {
	// Runner is the guest command that invokes the structured-run subcommand.
	// Empty defaults to {"harness-wrapper", "structured-run"}. A test overrides
	// it with the path to a freshly built harness-wrapper binary so the
	// round-trip is hermetic.
	Runner []string

	// Harness is the short harness name (e.g. "claude", "codex"). Required.
	Harness string
	// HarnessArgs are passed verbatim to the harness after the "--" separator.
	HarnessArgs []string
	// Effort / Model are optional wrapper flags (`--effort` / `--model`).
	Effort string
	Model  string
	// SandboxDefaults, when true, passes `--sandbox-defaults` to the guest
	// runner: claude only — the wrapper injects
	// --dangerously-skip-permissions into the harness args and IS_SANDBOX=1
	// into the harness env (meta-harness parity); a no-op for other
	// harnesses.
	SandboxDefaults bool

	// Prompt is uploaded into the workspace and fed to the turn via
	// `--prompt-file` — a prompt with quotes / newlines / leading dashes can
	// never corrupt argv.
	Prompt string

	// Env is overlaid on the guest exec env. It carries the
	// HARNESS_BINARY_<NAME> / HARNESS_BINARY override so a test can point the
	// inner turn engine at internal/fakeharness, plus any run-timeout override.
	Env map[string]string

	// PromptGuestPath overrides where the prompt is uploaded in the guest.
	// Empty defaults to <GuestPath(PathTmp)>/structured-turn-prompt.txt.
	PromptGuestPath string

	// TranscriptGuestPath and TranscriptHostPath, when BOTH set, download a guest
	// transcript artifact after the turn — best-effort, mirroring MH's optional
	// download. The Go guest embeds transcript_entries directly in the JSON
	// result, so this is an optional secondary artifact and a download failure
	// never fails the turn.
	TranscriptGuestPath string
	TranscriptHostPath  string
}

// RunStructuredTurn runs one structured turn over ws and returns the parsed
// result. It uploads the prompt, execs the guest structured-run runner capturing
// stdout, parses the last JSON object line, optionally downloads a transcript
// artifact, and returns the *turnproto.StructuredTurnResult.
//
// The JSON payload is the source of truth: a deadline or errored turn still
// returns a non-nil result (with the matching Status) and a nil error — the
// caller inspects result.Status. An error is returned only for a transport
// failure (upload / spawn) or when NO JSON object line is present on stdout (the
// runner produced no protocol payload at all).
//
// Exit-code table (agreeing with the guest classifyStructuredResult): status
// "completed" → ExitOK(0), "deadline" → ExitDeadline(124), "errored" /
// "startup_error" → ExitError(1). The exec code is a coarse mirror of the
// payload; when the payload is unparseable it is surfaced in the returned error
// so an orchestrator can still distinguish a deadline (124) from a plain error.
func RunStructuredTurn(ctx context.Context, ws ienv.Workspace, cfg StructuredTurnConfig) (*turnproto.StructuredTurnResult, error) {
	if strings.TrimSpace(cfg.Harness) == "" {
		return nil, fmt.Errorf("env.RunStructuredTurn: empty harness name")
	}

	guestPrompt := cfg.PromptGuestPath
	if guestPrompt == "" {
		guestPrompt = ws.GuestPath(ienv.PathTmp) + "/" + defaultPromptName
	}
	if err := uploadPrompt(ctx, ws, cfg.Prompt, guestPrompt); err != nil {
		return nil, err
	}

	argv := buildRunnerArgv(cfg, guestPrompt)
	res, err := ws.Exec(ctx, argv, &ienv.ExecOpts{Env: cfg.Env})
	if err != nil {
		// A spawn/transport error (command not found, ctx cancellation); a
		// non-zero guest exit is NOT an error here — Workspace.Exec resolves it
		// with the code and the JSON payload on stdout.
		return nil, fmt.Errorf("env.RunStructuredTurn: exec runner: %w", err)
	}

	parsed, ok := turnproto.ParseLastJSONLine([]byte(res.Stdout))
	if !ok {
		return nil, fmt.Errorf("env.RunStructuredTurn: no structured result on stdout (exit %d): %s",
			res.Code, stderrTail(res.Stderr))
	}

	// Optional, best-effort transcript download — the Go guest already embeds
	// transcript_entries in the payload, so a failure never fails the turn.
	if cfg.TranscriptGuestPath != "" && cfg.TranscriptHostPath != "" {
		_ = ws.Download(ctx, cfg.TranscriptGuestPath, cfg.TranscriptHostPath)
	}

	return parsed, nil
}

// uploadPrompt writes the prompt to a host temp file and uploads it to guestPath
// — the safe host upload transport (a prompt with quotes / newlines / leading
// dashes can never corrupt argv).
func uploadPrompt(ctx context.Context, ws ienv.Workspace, prompt, guestPath string) error {
	f, err := os.CreateTemp("", "structured-turn-prompt-*.txt")
	if err != nil {
		return fmt.Errorf("env.RunStructuredTurn: stage prompt: %w", err)
	}
	hostPath := f.Name()
	defer func() { _ = os.Remove(hostPath) }()
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		return fmt.Errorf("env.RunStructuredTurn: write prompt: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("env.RunStructuredTurn: write prompt: %w", err)
	}
	if err := ws.Upload(ctx, hostPath, guestPath); err != nil {
		return fmt.Errorf("env.RunStructuredTurn: upload prompt: %w", err)
	}
	return nil
}

// buildRunnerArgv assembles the guest argv:
//
//	<runner...> --prompt-file <guestPrompt> [--effort E] [--model M] [--sandbox-defaults] <harness> -- <harnessArgs...>
//
// mirroring the structured-run subcommand's own extractPromptFile +
// parseHarnessWrapperArgs contract (`[wrapper flags] <name> -- <args>`).
func buildRunnerArgv(cfg StructuredTurnConfig, guestPrompt string) []string {
	runner := cfg.Runner
	if len(runner) == 0 {
		runner = defaultRunner
	}
	argv := make([]string, 0, len(runner)+len(cfg.HarnessArgs)+8)
	argv = append(argv, runner...)
	argv = append(argv, "--prompt-file", guestPrompt)
	if cfg.Effort != "" {
		argv = append(argv, "--effort", cfg.Effort)
	}
	if cfg.Model != "" {
		argv = append(argv, "--model", cfg.Model)
	}
	if cfg.SandboxDefaults {
		argv = append(argv, "--sandbox-defaults")
	}
	argv = append(argv, cfg.Harness, "--")
	argv = append(argv, cfg.HarnessArgs...)
	return argv
}

// stderrTail returns a trimmed, length-bounded tail of stderr for error
// messages, so a noisy runner failure stays legible.
func stderrTail(stderr string) string {
	s := strings.TrimSpace(stderr)
	const max = 512
	if len(s) > max {
		return "…" + s[len(s)-max:]
	}
	if s == "" {
		return "(no stderr)"
	}
	return s
}
