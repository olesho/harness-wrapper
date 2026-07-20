package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/transcript/codex"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
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

	// Strip Claude Code's nesting markers so the spawned harness persists a
	// transcript (see runOneShot for the full rationale), then apply the
	// opt-in --sandbox-defaults injection on top.
	harnessArgs, env := parsed.HarnessArgs, cleanedEnv()
	if parsed.SandboxDefaults {
		harnessArgs, env = applySandboxDefaults(parsed.HarnessName, harnessArgs, env)
	}

	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       parsed.HarnessName,
		BinaryPath:    binPath,
		Args:          harnessArgs,
		Effort:        parsed.Effort,
		Model:         parsed.Model,
		WorkingDir:    wd,
		Env:           env,
		Prompt:        prompt,
		ExitAfterTurn: true,
		// Unattended structured run: no client to answer Codex's update menu, so
		// auto-Skip it rather than wedge the run on the pending prompt.
		AutoSkipCodexUpdateNotice: true,
		InputPolicy: &chat.InputPolicy{
			ByKind: map[string]chat.Disposition{
				"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
			},
		},
		OnInputRequest: autoAcceptAnswer,
	})

	status, reason, exit := classifyStructuredResult(res, err)

	result := turnproto.StructuredTurnResult{
		Status:            status,
		HarnessSessionID:  res.Session.HarnessSessionID,
		TranscriptEntries: []transcript.Event{},
		WorkingDir:        wd,
		Reason:            reason,
	}
	if status == turnproto.StatusCompleted {
		result.Reply = cleanReply(res)
	}

	// Read the canonical transcript back in-guest — best-effort, so a Reader
	// failure never erases a successful reply. An empty/absent session id makes
	// Read error (missing files), which is tolerated: entries stay empty and the
	// failure is recorded in transcript_error.
	entries, terr := readStructuredTranscript(parsed.HarnessName, res.Session.HarnessSessionID, wd)
	if terr != nil {
		result.TranscriptError = terr.Error()
	} else {
		result.TranscriptEntries = entries
	}

	emitStructured(result)
	if status == turnproto.StatusDeadline {
		fmt.Fprintln(os.Stderr, turnproto.DeadlineLine)
	}
	return exit
}

// classifyStructuredResult maps a RunTurn (result, err) pair to the protocol
// status, an optional reason, and the process exit code. It branches on the
// RETURNED ERROR, not Turn.State — the critical fidelity fix (a mid-turn
// transport failure returns Turn.State == "").
func classifyStructuredResult(res harness.TurnResult, err error) (turnproto.TurnStatus, string, int) {
	switch {
	case err == nil:
		if res.Turn.State == chat.TurnStateComplete {
			return turnproto.StatusCompleted, "", turnproto.ExitOK
		}
		// RunTurn only returns nil on a completed turn; anything else is a
		// defensive fallback.
		return turnproto.StatusErrored, "turn ended in unexpected state", turnproto.ExitError
	case errors.Is(err, context.DeadlineExceeded):
		return turnproto.StatusDeadline, "", turnproto.ExitDeadline
	case errors.Is(err, harness.ErrTurnErrored):
		reason := res.Turn.Reason
		if reason == "" {
			reason = "turn errored"
		}
		return turnproto.StatusErrored, reason, turnproto.ExitError
	case errors.Is(err, chat.ErrClosed):
		// Mid-turn transport failure: the events channel closed after the turn
		// had already started via conv.Send.
		return turnproto.StatusErrored, err.Error(), turnproto.ExitError
	default:
		// Either a mid-turn ev.Err (the turn had started, so a chat Session was
		// opened and snapshotTurnResult populated Session.ID) or a pre-turn
		// startup failure (chat.Open / AcquireControl / Send returns a ZERO
		// TurnResult). Distinguish by whether a session was ever opened.
		if res.Session.ID != "" {
			return turnproto.StatusErrored, err.Error(), turnproto.ExitError
		}
		return turnproto.StatusStartupError, err.Error(), turnproto.ExitError
	}
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
