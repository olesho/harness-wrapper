package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// toolPermissionScreen is claude-code's PER-TOOL permission dialog — the thing
// `--permission-mode manual` / `acceptEdits` / `plan` exist to produce. It is a
// bordered menu just like the folder-trust dialog, but it carries NONE of the
// three anchors DetectInput knows (trustAnchor, trustAnchorAlt, bypassAnchor).
const toolPermissionScreen = `╭─────────────────────────────────────────────────╮
│ Bash command                                      │
│                                                   │
│   npm run build                                   │
│   Build the project                               │
│                                                   │
│ Do you want to proceed?                           │
│ ❯ 1. Yes                                          │
│   2. Yes, and don't ask again for npm commands    │
│   3. No, and tell Claude what to do differently   │
╰─────────────────────────────────────────────────╯`

// editPermissionScreen is the acceptEdits-mode variant: the same dialog shape
// raised for a file write rather than a command.
const editPermissionScreen = `╭─────────────────────────────────────────────────╮
│ Edit file                                         │
│                                                   │
│ /work/main.go                                     │
│   1  -  old line                                  │
│   1  +  new line                                  │
│                                                   │
│ Do you want to make this edit to main.go?         │
│ ❯ 1. Yes                                          │
│   2. Yes, allow all edits during this session     │
│   3. No, and tell Claude what to do differently   │
╰─────────────────────────────────────────────────╯`

// TestDetectInput_PerToolPermissionDialogNotDetected pins the RUNTIME-
// ENFORCEMENT gap on the claude side: DetectInput recognizes exactly three
// anchors (claudecode.go:88-93) and the per-tool permission dialog is not among
// them, so NO turns.InputRequest is ever emitted for it. Neither the unattended
// `trust_prompt` policy nor OnInputRequest can see a prompt that was never
// surfaced — on any path.
//
// Consequence being pinned: an unattended `structured-run --permission-mode
// manual claude` (or plan / acceptEdits) does not get denied or auto-answered —
// it STALLS on the dialog until the run deadline, yielding status "deadline"
// and process exit 124 (pkg/env/turn.go:97-99). The restrictive rungs are
// launch-time flags only; they bind nothing in this repo's input machinery.
//
// EXPECTED TO BREAK DELIBERATELY: the filed follow-up that teaches DetectInput
// the per-tool permission dialog will flip these assertions from `ok == false`
// to a real request. That is the fix, not a regression — when it goes red,
// delete this pin and assert the new Kind and options instead (and decide what
// the unattended policy should do with them).
func TestDetectInput_PerToolPermissionDialogNotDetected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
	}{
		{"bash command", toolPermissionScreen},
		{"file edit", editPermissionScreen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Guard the fixture itself: if it ever grows one of the known
			// anchors the pin would pass for the wrong reason.
			for _, anchor := range []string{trustAnchor, trustAnchorAlt, bypassAnchor} {
				if strings.Contains(tc.screen, anchor) {
					t.Fatalf("fixture contains the known anchor %q; it no longer models an UNKNOWN dialog", anchor)
				}
			}
			// The menu itself parses fine — the gap is anchor recognition, not
			// option parsing, so a future detector only needs a new anchor.
			if opts := parseNumberedMenu(tc.screen); len(opts) != 3 {
				t.Fatalf("parseNumberedMenu found %d options, want 3 — the fixture is not a well-formed menu", len(opts))
			}

			req, ok := DetectInput(tc.screen)
			if ok {
				t.Fatalf("DetectInput now classifies the per-tool permission dialog as %q with %d options — "+
					"restrictive --permission-mode rungs are no longer silently unenforced. Retire this pin.",
					req.Kind, len(req.Options))
			}
		})
	}
}

// TestOnScreen_PerToolPermissionDialogEmitsNoInputRequest pins the same gap one
// layer up: because DetectInput is silent, the adapter emits no InputRequested
// event either, so nothing downstream (policy, OnInputRequest, a live client)
// ever learns the turn is blocked. This is the mechanism behind the deadline /
// exit-124 outcome described above.
//
// EXPECTED TO BREAK DELIBERATELY by the same detector follow-up.
func TestOnScreen_PerToolPermissionDialogEmitsNoInputRequest(t *testing.T) {
	a := New()
	if req := findKind(a.OnScreen(screen.Snapshot{Text: toolPermissionScreen}), turns.InputRequested); req != nil {
		t.Fatalf("adapter surfaced an InputRequested (%+v) for the per-tool permission dialog; retire this pin", req.Input)
	}
}
