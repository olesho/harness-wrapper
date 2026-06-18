package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

// runOneShot is the "proper substitution for `claude -p`": it drives ONE turn
// through the real interactive harness via harness.RunTurn (PTY + turn
// detection), prints the assistant's clean final reply to stdout, and exits —
// instead of asking the harness for its own non-interactive print mode
// (`claude -p`, `codex exec`). Same ergonomics as `-p` (prompt on stdin, clean
// reply on stdout, exit code reflects success), but routed through the wrapper
// so every run is supervised, classified, and trace-able like an interactive
// session.
//
//	echo "do the thing" | harness-wrapper run claude -- --dangerously-skip-permissions
//
// The prompt is read from stdin. Args after `--` are the NORMAL interactive
// harness args (e.g. --dangerously-skip-permissions), not print/headless args.
// ExitAfterTurn stops the harness gracefully once the turn completes. The reply
// text is the adapter's clean message extract (TUI chrome stripped), or the
// harness transcript when one is available. `--timeout` via the
// HARNESS_WRAPPER_RUN_TIMEOUT env var (default 15m).
func runOneShot(args []string) int {
	parsed, err := parseHarnessWrapperArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-wrapper run:", err)
		return 2
	}
	binPath, err := resolveHarness(parsed.HarnessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-wrapper run:", err)
		return 2
	}

	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-wrapper run: read prompt from stdin:", err)
		return 1
	}
	if len(strings.TrimSpace(string(prompt))) == 0 {
		fmt.Fprintln(os.Stderr, "harness-wrapper run: empty prompt on stdin")
		return 2
	}

	timeout := 15 * time.Minute
	if v := os.Getenv("HARNESS_WRAPPER_RUN_TIMEOUT"); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	wd, _ := os.Getwd()
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       parsed.HarnessName,
		BinaryPath:    binPath,
		Args:          parsed.HarnessArgs,
		WorkingDir:    wd,
		Prompt:        string(prompt),
		ExitAfterTurn: true, // stop after the turn (graceful) → clean, bounded run
		// Auto-accept blocking prompts (e.g. the folder-trust dialog) so an
		// unattended one-shot never hangs waiting for a human.
		InputPolicy: &chat.InputPolicy{
			ByKind: map[string]chat.Disposition{
				"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
			},
		},
		OnInputRequest: func(req chat.InputRequest) (chat.InputAnswer, bool) {
			if opt := affirmativeOption(req); opt != nil {
				return chat.InputAnswer{OptionID: opt.ID}, true
			}
			if len(req.Options) > 0 {
				return chat.InputAnswer{OptionID: req.Options[0].ID}, true
			}
			return chat.InputAnswer{}, false
		},
	})
	// ErrTurnErrored carries a populated TurnResult; any other error is fatal.
	if err != nil && !errors.Is(err, harness.ErrTurnErrored) {
		fmt.Fprintln(os.Stderr, "harness-wrapper run:", err)
		return 1
	}

	text := cleanReply(res)
	fmt.Fprint(os.Stdout, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(os.Stdout)
	}

	if errors.Is(err, harness.ErrTurnErrored) {
		reason := res.Turn.Reason
		if reason == "" {
			reason = "turn errored"
		}
		fmt.Fprintln(os.Stderr, "harness-wrapper run:", reason)
		return 1
	}
	return 0
}

// cleanReply returns the assistant's final message. RunTurn already populates
// res.Turn.Text with the adapter's clean message extract (or transcript text
// when a session id was captured); res.History is the transcript-backed
// fallback. Both are scoped to the just-completed turn.
func cleanReply(res harness.TurnResult) string {
	if strings.TrimSpace(res.Turn.Text) != "" {
		return res.Turn.Text
	}
	return lastAssistant(res.History)
}

func lastAssistant(turns []chat.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == chat.RoleAssistant && strings.TrimSpace(turns[i].Text) != "" {
			return turns[i].Text
		}
	}
	return ""
}

// affirmativeOption picks the "yes/accept/trust" option from an input request,
// so auto-answering a trust/confirm dialog proceeds rather than declines.
func affirmativeOption(req chat.InputRequest) *chat.InputOption {
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
