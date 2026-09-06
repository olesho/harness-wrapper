package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/oneshot"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/transcript/codex"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// runStructuredRun is the STRUCTURED SIBLING of runOneShot: it drives ONE turn
// through the real interactive harness via harness.RunTurn (PTY + turn
// detection), then — instead of printing the plain reply — emits EXACTLY ONE
// machine-readable turnproto.StructuredTurnResult JSON object as the LAST line
// on stdout and exits with the protocol exit code. It is the guest-side runner
// a host orchestrator drives; the host parses the last stdout line via
// turnproto.ParseLastJSONLine, so leading harness banner/log noise is fine.
//
//	echo "do the thing" | harness-wrapper structured-run claude -- --dangerously-skip-permissions
//	harness-wrapper structured-run --prompt-file /path/prompt.txt claude -- --dangerously-skip-permissions
//
// The prompt comes from stdin, or from --prompt-file <path> (the host upload
// path — a prompt with quotes/newlines/leading-dashes can never corrupt argv).
// The status → exit-code mapping is keyed on the ERROR RETURNED by RunTurn (not
// on Turn.State), which is the fidelity fix: only a deadline changes the exit
// code (124); errored, mid-turn transport failure, and startup_error all exit 1;
// a clean completion exits 0.
func runStructuredRun(args []string) int {
	wd := structuredWorkingDir()

	promptFile, rest, err := extractPromptFile(args)
	if err != nil {
		return emitStartupError(wd, err)
	}
	parsed, err := parseHarnessWrapperArgs(rest)
	if err != nil {
		return emitStartupError(wd, err)
	}
	binPath, err := resolveHarness(parsed.HarnessName)
	if err != nil {
		return emitStartupError(wd, err)
	}

	prompt, err := readStructuredPrompt(promptFile)
	if err != nil {
		return emitStartupError(wd, err)
	}
	if len(strings.TrimSpace(prompt)) == 0 {
		return emitStartupError(wd, errors.New("empty prompt"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveRunTimeout())
	defer cancel()

	// Strip Claude Code's nesting markers (minus the credential exemption in
	// nestingExemptEnvKeys) so the spawned harness persists a transcript AND
	// stays authenticated (see runOneShot for the full rationale), then apply the
	// opt-in --sandbox-defaults injection on top. Both are env/arg POLICY and
	// stay a cmd/ concern: pkg/oneshot receives the ALREADY-CLEANED Env/Args.
	// The permission mode is passed in so a bypass rung composes: the env half
	// (IS_SANDBOX=1) still lands here while pkg/wrapper owns the argv directive.
	harnessArgs, env := parsed.HarnessArgs, cleanedEnv()
	if parsed.SandboxDefaults {
		harnessArgs, env = applySandboxDefaults(parsed.HarnessName, parsed.PermissionMode, harnessArgs, env)
	}

	// The classification core + auto-accept-trust wiring + reply extraction now
	// live in pkg/oneshot (the in-process one-shot library). This guest runner
	// composes that core with the exit map (turnproto.ExitCode), the JSON emit,
	// and the in-guest transcript read.
	outcome, oerr := oneshot.RunOneShotDetailed(ctx, oneshot.Config{
		Harness:        parsed.HarnessName,
		BinaryPath:     binPath,
		Args:           harnessArgs,
		Effort:         parsed.Effort,
		Model:          parsed.Model,
		PermissionMode: parsed.PermissionMode,
		WorkingDir:     wd,
		Env:            env,
		Prompt:         prompt,
	})
	if oerr != nil {
		// A non-nil error is an unclassifiable/infra failure (an invalid config);
		// every classified turn returns a status with a nil error.
		return emitStartupError(wd, oerr)
	}

	status := outcome.Status
	result := turnproto.StructuredTurnResult{
		Status:            status,
		HarnessSessionID:  outcome.HarnessSessionID,
		TranscriptEntries: []transcript.Event{},
		WorkingDir:        wd,
		Reason:            outcome.Reason,
	}
	if status == turnproto.StatusCompleted {
		result.Reply = outcome.Reply
	}

	// A startup_error means no harness was launched, so there is no rung to
	// report; deadline/errored DID launch and the rung is exactly the record an
	// orchestrator wants. One guard is SUFFICIENT, not merely conservative: a
	// pre-turn failure returns a ZERO TurnResult, so res.Session.ID == "" and
	// oneshot.Classify's default arm can only classify it as startup_error
	// (pkg/oneshot/oneshot.go:243-251) — a rejected config can never surface as
	// errored. harnessArgs is read AFTER applySandboxDefaults so the injected
	// --dangerously-skip-permissions is in scope, and pre-pkg/wrapper injection,
	// which is exactly the input EffectiveLaunchRung expects (it is idempotent
	// over injection either way). See
	// turnproto.StructuredTurnResult.PermissionMode for what the value promises
	// — in particular that a restrictive rung is NOT an enforcement claim here.
	if status != turnproto.StatusStartupError {
		result.PermissionMode = wrapper.EffectiveLaunchRung(
			parsed.HarnessName, harnessArgs, parsed.PermissionMode,
		)
	}

	// Read the canonical transcript back in-guest — best-effort, so a Reader
	// failure never erases a successful reply. An empty/absent session id makes
	// Read error (missing files), which is tolerated: entries stay empty and the
	// failure is recorded in transcript_error.
	entries, terr := readStructuredTranscript(parsed.HarnessName, outcome.HarnessSessionID, wd)
	if terr != nil {
		result.TranscriptError = terr.Error()
	} else {
		result.TranscriptEntries = entries
	}

	// Populate token accounting best-effort, mirroring meta-harness's
	// `usage ?? undefined`: a Reader that also implements transcript.UsageReader
	// is asked for usage, but a ReadUsage failure, an absent/empty session id, or
	// a (nil, nil) result must NEVER erase a good reply or change the exit code.
	// Usage is optional — on any of those paths result.Usage stays nil and the
	// omitempty tag drops the field. No usage_error sibling is emitted (a
	// failure-observability field would be an additive follow-up, not this
	// ticket).
	if reader, ok := transcriptReaderFor(parsed.HarnessName); ok {
		if ur, ok := reader.(transcript.UsageReader); ok {
			if u, uerr := ur.ReadUsage(outcome.HarnessSessionID, wd); uerr == nil && u != nil {
				result.Usage = u
			}
		}
	}

	emitStructured(result)
	exit := turnproto.ExitCode(status)
	if status == turnproto.StatusDeadline {
		fmt.Fprintln(os.Stderr, turnproto.DeadlineLine)
	}
	return exit
}

// readStructuredTranscript reads the canonical Event stream for the session
// using the transcript Reader selected by HARNESS SHORT NAME ("claude"/"codex")
// — NOT the chat-adapter name ("claude-code"). It reads the raw on-disk stream,
// which preserves tool-call events that res.History folds away.
func readStructuredTranscript(harnessName, harnessSessionID, workingDir string) ([]transcript.Event, error) {
	reader, ok := transcriptReaderFor(harnessName)
	if !ok {
		return nil, fmt.Errorf("no transcript reader for harness %q", harnessName)
	}
	return reader.Read(harnessSessionID, workingDir)
}

// transcriptReaderFor selects the per-harness transcript.Reader by short name.
// Deliberately NOT chat.resolveAdapter: that returns a turns.Adapter keyed on
// the chat-adapter name ("claude-code"), a different interface and a different
// key space.
func transcriptReaderFor(harnessName string) (transcript.Reader, bool) {
	switch harnessName {
	case "claude":
		return claudecode.New(), true
	case "codex":
		return codex.New(), true
	default:
		return nil, false
	}
}

// structuredWorkingDir mirrors MH: the guest worktree path if the host set it,
// else the process working directory.
func structuredWorkingDir() string {
	if p := strings.TrimSpace(os.Getenv("LOOM_WORKTREE_PATH")); p != "" {
		return p
	}
	wd, _ := os.Getwd()
	return wd
}

// readStructuredPrompt reads the prompt from --prompt-file when given (the safe
// host upload transport), else from stdin.
func readStructuredPrompt(promptFile string) (string, error) {
	if promptFile != "" {
		data, err := os.ReadFile(promptFile) //nolint:gosec // caller-supplied path
		if err != nil {
			return "", fmt.Errorf("read prompt file %q: %w", promptFile, err)
		}
		return string(data), nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read prompt from stdin: %w", err)
	}
	return string(data), nil
}

// extractPromptFile pulls a leading `--prompt-file <path>` (or
// `--prompt-file=<path>`) out of the args BEFORE the `--` separator, returning
// the value and the remaining args in the shape parseHarnessWrapperArgs expects
// (`<name> -- <harness args>`). Tokens at or after `--` are passed through
// untouched.
func extractPromptFile(args []string) (string, []string, error) {
	sep := len(args)
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	promptFile := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < sep; i++ {
		a := args[i]
		if a == "--prompt-file" {
			if i+1 >= sep {
				return "", nil, errors.New("--prompt-file requires a value")
			}
			promptFile = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, "--prompt-file=") {
			promptFile = a[len("--prompt-file="):]
			continue
		}
		rest = append(rest, a)
	}
	rest = append(rest, args[sep:]...)
	return promptFile, rest, nil
}

// emitStructured writes the result as EXACTLY ONE JSON object line on stdout —
// the last line the host's ParseLastJSONLine scans back to.
func emitStructured(res turnproto.StructuredTurnResult) {
	data, err := json.Marshal(res)
	if err != nil {
		// Marshaling a fixed-shape struct cannot realistically fail; fall back to
		// a minimal hand-rolled object so the host still sees a parseable line.
		// This path emits a FIXED, hand-rolled minimal startup_error shape
		// regardless of what it was handed — it is a last-resort parseable line,
		// not a second copy of the result shape. Do not add optional keys to it.
		_, _ = fmt.Fprintf(os.Stdout, `{"status":"startup_error","reply":"","harnessSessionID":"","transcript_entries":[],"working_dir":%q,"reason":%q}`+"\n",
			res.WorkingDir, err.Error())
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, string(data))
}

// emitStartupError emits a startup_error result line and returns the exit code
// for any pre-turn/setup failure (bad args, unresolved harness, empty prompt).
func emitStartupError(workingDir string, err error) int {
	emitStructured(turnproto.StructuredTurnResult{
		Status:            turnproto.StatusStartupError,
		HarnessSessionID:  "",
		TranscriptEntries: []transcript.Event{},
		WorkingDir:        workingDir,
		Reason:            err.Error(),
	})
	return turnproto.ExitError
}
