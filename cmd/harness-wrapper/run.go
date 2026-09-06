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

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/oneshot"
	"golang.org/x/term"
)

// runErrPrefix labels fatal one-shot errors on stderr.
const runErrPrefix = "harness-wrapper run:"

// resolveRunTimeout returns the per-turn deadline, defaulting to 15m and
// honoring a valid HARNESS_WRAPPER_RUN_TIMEOUT duration override.
func resolveRunTimeout() time.Duration {
	timeout := 15 * time.Minute
	if v := os.Getenv("HARNESS_WRAPPER_RUN_TIMEOUT"); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil {
			timeout = d
		}
	}
	return timeout
}

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
// hangs. See resolveInputMode / selectAnswer / oneshot.AutoAcceptAnswer.
func runOneShot(args []string) int {
	parsed, err := parseHarnessWrapperArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, runErrPrefix, err)
		return 2
	}
	binPath, err := resolveHarness(parsed.HarnessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, runErrPrefix, err)
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

	ctx, cancel := context.WithTimeout(context.Background(), resolveRunTimeout())
	defer cancel()

	// Resolve interactive vs. auto-accept ONCE, up front. A /dev/tty handle (if
	// opened) is owned by this function for the whole run and closed on return.
	tty, interactive := resolveInputMode(parsed.AutoAccept)
	if tty != nil {
		defer func() { _ = tty.Close() }()
	}

	// Strip Claude Code's nesting markers (CLAUDECODE / CLAUDE_CODE_*, minus
	// the credential exemption in nestingExemptEnvKeys): when harness-wrapper
	// itself runs inside a Claude Code session, a nested `claude` disables
	// session persistence and never writes the JSONL transcript we read back.
	// A top-level env makes it persist normally.
	// Then apply the opt-in --sandbox-defaults injection on top. The permission
	// mode is passed in so a bypass rung composes: applySandboxDefaults then
	// contributes the IS_SANDBOX=1 env half only and pkg/wrapper owns the
	// permission directive in argv.
	harnessArgs, env := parsed.HarnessArgs, cleanedEnv()
	if parsed.SandboxDefaults {
		harnessArgs, env = applySandboxDefaults(parsed.HarnessName, parsed.PermissionMode, harnessArgs, env)
	}

	wd, _ := os.Getwd()
	cfg := harness.TurnConfig{
		Harness:        parsed.HarnessName,
		BinaryPath:     binPath,
		Args:           harnessArgs,
		Effort:         parsed.Effort,
		Model:          parsed.Model,
		PermissionMode: parsed.PermissionMode,
		WorkingDir:     wd,
		Env:            env,
		Prompt:         string(prompt),
		ExitAfterTurn:  true, // stop after the turn (graceful) → clean, bounded run
	}

	cfg.InputPolicy, cfg.OnInputRequest = inputHandling(ctx, interactive, tty)
	// In interactive mode the OnInputRequest callback drives a TTY chooser, so
	// surface Codex's update menu and let the human pick. Unattended, there is
	// no one to answer it, so auto-Skip it rather than wedge the run.
	cfg.AutoSkipCodexUpdateNotice = !interactive

	res, err := harness.RunTurn(ctx, cfg)
	// ErrTurnErrored carries a populated TurnResult; any other error is fatal.
	if err != nil && !errors.Is(err, harness.ErrTurnErrored) {
		fmt.Fprintln(os.Stderr, runErrPrefix, err)
		return 1
	}

	text := oneshot.Reply(res)
	_, _ = fmt.Fprint(os.Stdout, text)
	if !strings.HasSuffix(text, "\n") {
		_, _ = fmt.Fprintln(os.Stdout)
	}

	if errors.Is(err, harness.ErrTurnErrored) {
		reason := res.Turn.Reason
		if reason == "" {
			reason = "turn errored"
		}
		fmt.Fprintln(os.Stderr, runErrPrefix, reason)
		return 1
	}
	return 0
}

// nestingEnvKey is Claude Code's exact-match session-nesting marker;
// nestingEnvPrefix is the family prefix every other marker shares.
const (
	nestingEnvKey    = "CLAUDECODE"
	nestingEnvPrefix = "CLAUDE_CODE_"
)

// nestingExemptEnvKeys are keys that match nestingEnvPrefix but are NOT nesting
// markers, so they must survive the scrub.
//
// CLAUDE_CODE_OAUTH_TOKEN is claude's long-lived headless credential, minted by
// `claude setup-token`. It is the equivalent of a ~/.claude login, not something
// a running claude exports into its children — it just happens to share the
// prefix. Stripping it made the spawned harness start UNAUTHENTICATED anywhere
// the token is the only working auth (a fresh host, a container, a CI runner):
// claude paints the login wall, no turn ever completes, and the run dies at the
// deadline (PUPPET-317; meta-harness saw the same as PUPPET-309, measured as
// ~285s deadline deaths in LIVE_CLAUDE=1 live_question.test.ts).
//
// Precedent for exempting a credential from an env filter: meta-harness
// src/chat/env.ts NESTING_EXEMPT (commit 379d162), loomcli
// internal/cli/envfilter/envfilter.go (exact allowlist) and
// internal/driver/env.go trustedLocalProviderCredentials.
//
// BOUNDARY: this is a NESTING predicate, not a containment filter. It says
// nothing about what may cross into a guest or sandbox —
// internal/env/daytona/leak_probe.go independently and correctly lists
// CLAUDE_CODE_OAUTH_TOKEN in CredentialSensitiveEnvNames. Do not reuse this set
// there, and do not "reconcile" the two: they answer different questions and
// both answers are right.
//
// Exact match only. CLAUDE_CODE_OAUTH_TOKEN_FILE, CLAUDE_CODE_OAUTH_TOKENX and
// friends are still stripped.
var nestingExemptEnvKeys = map[string]bool{
	"CLAUDE_CODE_OAUTH_TOKEN": true,
}

// isClaudeNestingEnvKey reports whether key is one of Claude Code's session
// nesting markers. The exemption set is consulted first; see
// nestingExemptEnvKeys.
func isClaudeNestingEnvKey(key string) bool {
	if nestingExemptEnvKeys[key] {
		return false
	}
	return key == nestingEnvKey || strings.HasPrefix(key, nestingEnvPrefix)
}

// filterNestingEnv returns src minus the Claude Code nesting markers, keeping
// the relative order of the survivors (a later duplicate key wins in exec, so
// reordering would be observable). Pure — it takes the environment as an
// argument so the policy is testable without mutating the process environment.
func filterNestingEnv(src []string) []string {
	out := make([]string, 0, len(src))
	for _, kv := range src {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if isClaudeNestingEnvKey(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// cleanedEnv returns the current environment minus Claude Code's nesting
// markers, so a spawned `claude` runs as a top-level (persisting) session.
// CLAUDE_CODE_OAUTH_TOKEN is deliberately preserved — see nestingExemptEnvKeys.
func cleanedEnv() []string { return filterNestingEnv(os.Environ()) }

// inputHandling builds the InputPolicy + OnInputRequest callback for the chosen
// mode. Factored out of runOneShot so the two wirings are unit-testable without
// a real /dev/tty or a live harness.
//
//   - interactive: NO InputPolicy (nil) — policyOption resolves trust_prompt
//     BEFORE OnInputRequest (pkg/chat/input.go), so a trust_prompt policy entry
//     would silently auto-accept folder trust and it would never reach the
//     human; nil makes every kind fall through to the callback. The callback
//     surfaces the prompt on tty via selectAnswer (bounded by ctx), then falls
//     back to oneshot.AutoAcceptAnswer on EOF/invalid/deadline so a partial
//     interaction still resolves rather than failing the run with ErrInputPending.
//     TurnConfig.Output is left unset by the caller: raw PTY bytes would garble
//     the clean menu on the same tty (see runOneShot).
//   - unattended: today's behavior — a trust_prompt auto-answer policy plus an
//     oneshot.AutoAcceptAnswer callback, so an unattended one-shot never hangs.
//     That ONE trust_prompt entry deliberately covers TWO different screens:
//     claude-code's folder-trust dialog AND its --dangerously-skip-permissions
//     ("Bypass Permissions mode") acceptance screen, which claudecode.DetectInput
//     emits under the same Kind — so unattended, a bypass launch is accepted by
//     the entry written for folder trust. This coupling is pinned
//     (TestInputHandling_UnattendedAutoAcceptsBypassAcceptance) pending the
//     follow-up that splits the detector's Kind (e.g. bypass_acceptance);
//     splitting it is a behavior change to a shipped detector and is out of
//     scope here. pkg/oneshot.turnConfig holds a SECOND, independent copy of
//     this same policy — change both together.
//     Note also what this callback does NOT gate: claude's per-tool permission
//     dialog is not detected at all, so restrictive --permission-mode rungs
//     stall an unattended turn to the deadline (exit 124), and codex's
//     KindApproval is auto-answered "proceed" by AutoAcceptAnswer regardless of
//     -a. Both are pinned as tests; see pkg/oneshot/permission_pin_test.go.
func inputHandling(ctx context.Context, interactive bool, tty *os.File) (*chat.InputPolicy, func(chat.InputRequest) (chat.InputAnswer, bool)) {
	if interactive {
		return nil, func(req chat.InputRequest) (chat.InputAnswer, bool) {
			if ans, ok := interactiveSelect(ctx, req, tty); ok {
				return ans, true
			}
			return oneshot.AutoAcceptAnswer(req)
		}
	}
	policy := &chat.InputPolicy{
		ByKind: map[string]chat.Disposition{
			"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
		},
	}
	return policy, func(req chat.InputRequest) (chat.InputAnswer, bool) {
		return oneshot.AutoAcceptAnswer(req)
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
//     signal to fall back to oneshot.AutoAcceptAnswer. Never hangs, never panics.
func selectAnswer(req chat.InputRequest, in io.Reader, out io.Writer) (chat.InputAnswer, bool) {
	r := bufio.NewReader(in)
	if len(req.Options) == 0 {
		return freeTextAnswer(r, req, out)
	}

	for attempt := 0; attempt < selectInputMaxAttempts; attempt++ {
		renderOptions(req, out)

		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return chat.InputAnswer{}, false
		}
		choice := strings.TrimSpace(line)
		if n, perr := parseIndex(choice, len(req.Options)); perr == nil {
			return chat.InputAnswer{OptionID: req.Options[n].ID}, true
		}
		_, _ = fmt.Fprintf(out, "Invalid choice %q.\n", choice)
		if err != nil {
			// Reader is exhausted (EOF after a partial line): stop rather than
			// spin re-prompting a closed reader.
			break
		}
	}
	return chat.InputAnswer{}, false
}

// freeTextAnswer handles the no-options case: print the prompt, read one line,
// and return it as InputAnswer.Text. An EOF with no data returns (_, false).
func freeTextAnswer(r *bufio.Reader, req chat.InputRequest, out io.Writer) (chat.InputAnswer, bool) {
	_, _ = fmt.Fprintln(out, req.Prompt)
	_, _ = fmt.Fprint(out, "Enter response: ")
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return chat.InputAnswer{}, false
	}
	return chat.InputAnswer{Text: strings.TrimRight(line, "\r\n")}, true
}

// renderOptions prints the prompt, a 1-based numbered list of options (with
// "(Alias)" when present), and the "Select [1-N]:" trailer.
func renderOptions(req chat.InputRequest, out io.Writer) {
	_, _ = fmt.Fprintln(out, req.Prompt)
	for i := range req.Options {
		o := &req.Options[i]
		if o.Alias != "" {
			_, _ = fmt.Fprintf(out, "  %d) %s (%s)\n", i+1, o.Label, o.Alias)
		} else {
			_, _ = fmt.Fprintf(out, "  %d) %s\n", i+1, o.Label)
		}
	}
	_, _ = fmt.Fprintf(out, "Select [1-%d]: ", len(req.Options))
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
