package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

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
//
// Interactive vs. unattended. `run` reads the prompt from os.Stdin (a pipe), so
// os.Stdin can never be an interactive menu source. When a controlling terminal
// is genuinely attached (a developer running `harness-wrapper run claude <<<
// "prompt"` from a shell), a blocking trust/permission prompt is surfaced to the
// human via a freshly opened /dev/tty, bounded by the run deadline. When no
// /dev/tty is attached (CI, pipes, nohup) — or --auto-accept is set — the run
// stays fully unattended and auto-answers the affirmative option so it never
// hangs. See resolveInputMode / selectAnswer / autoAcceptAnswer.
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

	// Resolve interactive vs. auto-accept ONCE, up front. A /dev/tty handle (if
	// opened) is owned by this function for the whole run and closed on return.
	tty, interactive := resolveInputMode(parsed.AutoAccept)
	if tty != nil {
		defer tty.Close()
	}

	wd, _ := os.Getwd()
	cfg := harness.TurnConfig{
		Harness:    parsed.HarnessName,
		BinaryPath: binPath,
		Args:       parsed.HarnessArgs,
		Effort:     parsed.Effort,
		Model:      parsed.Model,
		WorkingDir: wd,
		// Strip Claude Code's nesting markers (CLAUDECODE / CLAUDE_CODE_*): when
		// harness-wrapper itself runs inside a Claude Code session, a nested
		// `claude` disables session persistence and never writes the JSONL
		// transcript we read back. A top-level env makes it persist normally.
		Env:           cleanedEnv(),
		Prompt:        string(prompt),
		ExitAfterTurn: true, // stop after the turn (graceful) → clean, bounded run
	}

	cfg.InputPolicy, cfg.OnInputRequest = inputHandling(ctx, interactive, tty)

	res, err := harness.RunTurn(ctx, cfg)
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

// cleanReply returns the assistant's final message, PREFERRING the harness's
// own transcript (res.History, read back via turns.TranscriptReader) — it is
// authoritative and complete, with no TUI chrome and no risk of the
// screen-extraction dropping content. It falls back to the screen-derived
// res.Turn.Text (adapter ExtractMessage) only when no transcript was available
// (e.g. the harness session id could not be captured). Set
// HARNESS_WRAPPER_RUN_DEBUG=1 to log which source was used.
//
// The source label is taken from res.HistorySource, NOT from whether
// res.History happens to be non-empty: the store fallback also returns turns,
// so len(History) can't distinguish the transcript from the lossy screen
// scrape. We only report "transcript" when History is genuinely
// transcript-backed AND carries assistant text.
func cleanReply(res harness.TurnResult) string {
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
	// Screen-derived fallback: prefer the completing turn's extracted message;
	// if that's empty, use the last assistant turn the store recorded (also
	// screen-derived) so we never silently drop a reply we did capture.
	if strings.TrimSpace(res.Turn.Text) != "" {
		return res.Turn.Text
	}
	return lastAssistant(res.History)
}

// cleanedEnv returns the current environment minus Claude Code's nesting
// markers, so a spawned `claude` runs as a top-level (persisting) session.
func cleanedEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
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

// autoAcceptAnswer is the shared three-tier unattended fallback: pick the
// affirmative option, else the first option, else decline (false). A false
// return surfaces the request on Events(); in runOneShot nothing consumes that,
// so falling through to the first option (rather than false) for a
// has-options-but-no-affirmative prompt is what keeps the run from failing with
// chat.ErrInputPending. Both the auto-accept-mode callback and the
// interactive-mode fallback route through this single function.
func autoAcceptAnswer(req chat.InputRequest) (chat.InputAnswer, bool) {
	if opt := affirmativeOption(req); opt != nil {
		return chat.InputAnswer{OptionID: opt.ID}, true
	}
	if len(req.Options) > 0 {
		return chat.InputAnswer{OptionID: req.Options[0].ID}, true
	}
	return chat.InputAnswer{}, false
}

// inputHandling builds the InputPolicy + OnInputRequest callback for the chosen
// mode. Factored out of runOneShot so the two wirings are unit-testable without
// a real /dev/tty or a live harness.
//
//   - interactive: NO InputPolicy (nil) — policyOption resolves trust_prompt
//     BEFORE OnInputRequest (pkg/chat/input.go), so a trust_prompt policy entry
//     would silently auto-accept folder trust and it would never reach the
//     human; nil makes every kind fall through to the callback. The callback
//     surfaces the prompt on tty via selectAnswer (bounded by ctx), then falls
//     back to autoAcceptAnswer on EOF/invalid/deadline so a partial interaction
//     still resolves rather than failing the run with ErrInputPending.
//     TurnConfig.Output is left unset by the caller: raw PTY bytes would garble
//     the clean menu on the same tty (see runOneShot).
//   - unattended: today's behavior — a trust_prompt auto-answer policy plus an
//     autoAcceptAnswer callback, so an unattended one-shot never hangs.
func inputHandling(ctx context.Context, interactive bool, tty *os.File) (*chat.InputPolicy, func(chat.InputRequest) (chat.InputAnswer, bool)) {
	if interactive {
		return nil, func(req chat.InputRequest) (chat.InputAnswer, bool) {
			if ans, ok := interactiveSelect(ctx, req, tty); ok {
				return ans, true
			}
			return autoAcceptAnswer(req)
		}
	}
	policy := &chat.InputPolicy{
		ByKind: map[string]chat.Disposition{
			"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
		},
	}
	return policy, func(req chat.InputRequest) (chat.InputAnswer, bool) {
		return autoAcceptAnswer(req)
	}
}

// resolveInputMode decides how blocking prompts are answered in `run`.
//   - --auto-accept always wins → auto-accept mode (nil tty, false).
//   - else /dev/tty must be openable AND a terminal → interactive mode
//     (returns the open *os.File; caller owns Close).
//   - else (no controlling terminal: CI, pipes, nohup) → auto-accept mode.
//
// Gating on /dev/tty rather than os.Stdin is deliberate: runOneShot has already
// drained os.Stdin (the prompt pipe) with io.ReadAll, so os.Stdin is never a
// usable menu source.
func resolveInputMode(autoAccept bool) (*os.File, bool) {
	if autoAccept {
		return nil, false
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, false
	}
	if !term.IsTerminal(int(tty.Fd())) {
		_ = tty.Close()
		return nil, false
	}
	return tty, true
}

// interactiveSelect runs selectAnswer against the terminal but bounds the
// blocking read by ctx: OnInputRequest is invoked synchronously on the chat
// watcher pump goroutine (pkg/chat/input.go handleInputRequested), so a read
// that outlived the run deadline would stall the pump. The read runs in a
// short-lived goroutine spawned PER callback invocation (the pump is
// synchronous, so at most one is live at a time); we select on it vs ctx.Done.
//
// On ctx cancellation the read goroutine is left blocked on tty and leaks until
// the process exits — acceptable for a one-shot `run` that exits right after the
// turn. Do NOT close tty here to unblock it: tty is owned by runOneShot for the
// whole run and closed by its deferred Close; racing that close is worse than
// the leak.
func interactiveSelect(ctx context.Context, req chat.InputRequest, tty *os.File) (chat.InputAnswer, bool) {
	type result struct {
		ans chat.InputAnswer
		ok  bool
	}
	ch := make(chan result, 1)
	go func() {
		ans, ok := selectAnswer(req, tty, tty)
		ch <- result{ans, ok}
	}()
	select {
	case <-ctx.Done():
		return chat.InputAnswer{}, false
	case r := <-ch:
		return r.ans, r.ok
	}
}

// selectInputMaxAttempts bounds the invalid-choice re-prompt loop so a stream of
// garbage input can never spin selectAnswer forever.
const selectInputMaxAttempts = 5

// selectAnswer renders req to out and reads the human's choice from in. It is a
// pure helper (no os.Stdin / tty globals) so it can be unit-tested with plain
// readers/writers.
//
//   - Numbered options: prints the prompt + a 1-based numbered list (Label, with
//     "(Alias)" when present) and reads a line; a valid 1-based index returns
//     that option's ID. Invalid choices re-prompt, bounded by
//     selectInputMaxAttempts.
//   - Free-text (no options): prints the prompt + "Enter response:" and returns
//     the line as InputAnswer.Text.
//   - EOF / closed reader / exhausted attempts return (_, false) — the caller's
//     signal to fall back to autoAcceptAnswer. Never hangs, never panics.
func selectAnswer(req chat.InputRequest, in io.Reader, out io.Writer) (chat.InputAnswer, bool) {
	r := bufio.NewReader(in)

	if len(req.Options) == 0 {
		fmt.Fprintln(out, req.Prompt)
		fmt.Fprint(out, "Enter response: ")
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return chat.InputAnswer{}, false
		}
		return chat.InputAnswer{Text: strings.TrimRight(line, "\r\n")}, true
	}

	for attempt := 0; attempt < selectInputMaxAttempts; attempt++ {
		fmt.Fprintln(out, req.Prompt)
		for i := range req.Options {
			o := &req.Options[i]
			if o.Alias != "" {
				fmt.Fprintf(out, "  %d) %s (%s)\n", i+1, o.Label, o.Alias)
			} else {
				fmt.Fprintf(out, "  %d) %s\n", i+1, o.Label)
			}
		}
		fmt.Fprintf(out, "Select [1-%d]: ", len(req.Options))

		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return chat.InputAnswer{}, false
		}
		choice := strings.TrimSpace(line)
		if n, perr := parseIndex(choice, len(req.Options)); perr == nil {
			return chat.InputAnswer{OptionID: req.Options[n].ID}, true
		}
		fmt.Fprintf(out, "Invalid choice %q.\n", choice)
		if err != nil {
			// Reader is exhausted (EOF after a partial line): stop rather than
			// spin re-prompting a closed reader.
			break
		}
	}
	return chat.InputAnswer{}, false
}

// parseIndex parses a 1-based menu selection into a 0-based slice index in
// [0,n). It rejects empty, non-numeric, and out-of-range input.
func parseIndex(s string, n int) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		v = v*10 + int(c-'0')
	}
	if v < 1 || v > n {
		return 0, fmt.Errorf("out of range")
	}
	return v - 1, nil
}
