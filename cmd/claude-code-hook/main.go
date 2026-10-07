// Command claude-code-hook is the Claude Code profile's hook helper: the
// command claude's settings.json runs for each hook, from the harness
// distribution (pkg/adapter/claudecode.HookPath). It hands the hook's payload
// to hw's claude hook handler, which writes the events it reports durably to
// the spool named by HW_EVENT_SPOOL, where the profile's record reader finds
// them.
//
//	claude-code-hook claude <hook>
//	claude-code-hook tui <hook>
//
// The second form is the Claude Code TUI profile's live hooks
// (pkg/adapter/claudecodetui/live): what claude reports as it runs, written
// for its transport, which drives claude's TUI.
//
// It exits 2 when the handler says to block the tool (its decision on
// stdout), and 0 otherwise: a failure is reported on stderr and never blocks
// claude.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	_ "github.com/olesho/harness-wrapper/pkg/harness/claude" // registers the "claude" hook profile
	"github.com/olesho/harness-wrapper/pkg/harnessname"
)

// maxPayload bounds what a hook may hand the handler on stdin.
const maxPayload = 16 << 20

func main() { os.Exit(run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr)) }

func run(args, env []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != harnessname.Claude && args[0] != live.Harness {
		_, _ = fmt.Fprintln(stderr, "usage: claude-code-hook claude|tui <hook>")
		return 0
	}
	payload, err := io.ReadAll(io.LimitReader(stdin, maxPayload+1))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "claude-code-hook: read payload: %v\n", err)
		return 0
	}
	if len(payload) > maxPayload {
		_, _ = fmt.Fprintf(stderr, "claude-code-hook: payload over %d bytes, not recorded\n", maxPayload)
		return 0
	}
	if args[0] == live.Harness {
		if err := live.HandleHook(args[1], env, payload); err != nil {
			_, _ = fmt.Fprintf(stderr, "claude-code-hook: %v\n", err)
		}
		return 0
	}
	out, err := harnesscore.HandleHookEvent(harnessname.Claude, args[1], env, payload)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "claude-code-hook: %v\n", err)
		return 0
	}
	if out.Block {
		_, _ = io.WriteString(stdout, out.BlockOutput)
		return 2
	}
	return 0
}
