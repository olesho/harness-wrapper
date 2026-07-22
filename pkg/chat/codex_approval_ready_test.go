package chat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// codexCorpusScreen renders a recorded codex byte stream through the emulator at
// the recorded geometry (120x40) and returns the snapshot text. Follows the
// auth_corpus_test.go pattern of reading the shared corpus straight off disk; a
// missing recording is FATAL, not a skip — these fixtures are the only evidence
// the readiness gate sees a real approval dialog.
func codexCorpusScreen(t *testing.T, scenario string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../test/corpus/codex", scenario, "bytes.raw"))
	if err != nil {
		t.Fatalf("test/corpus/codex/%s/bytes.raw is REQUIRED: %v", scenario, err)
	}
	scr := screen.New(120, 40)
	_, _ = scr.Write(data)
	return scr.Snapshot().Text
}

// TestReadyForInput_CodexApprovalDialogIsNotReady is the production consequence
// of the KindApproval port. A session parked on a genuine command / apply-patch
// approval dialog used to report READY: DetectInput was silent on it, so
// readyForInput fell through to codex.PromptReady, whose promptRE matches the
// dialog's own "› 1. Yes, proceed (y)" highlight row. Send then typed the
// prompt text straight into the approval menu, and maybeIdleComplete could
// idle-complete the turn with the dialog still up.
//
// readyForInput inherits the fix through its existing codex.DetectInput
// delegation — ready.go itself needed no change, so this test is what pins the
// behavior at the chat layer.
func TestReadyForInput_CodexApprovalDialogIsNotReady(t *testing.T) {
	for _, scenario := range []string{"approval-command", "approval-patch"} {
		t.Run(scenario, func(t *testing.T) {
			text := codexCorpusScreen(t, scenario)
			if readyForInput("codex", text) {
				t.Errorf("readyForInput(codex, %s) = true; a live approval dialog must "+
					"hold the session NOT ready (otherwise Send types the prompt into the menu)", scenario)
			}
		})
	}
}

// TestReadyForInput_CodexIdleComposerStaysReady is the other half: the approval
// gate must not have made readiness stricter for the ordinary idle composer.
func TestReadyForInput_CodexIdleComposerStaysReady(t *testing.T) {
	if !readyForInput("codex", codexCorpusScreen(t, "prompt-ready")) {
		t.Error("readyForInput(codex, prompt-ready) = false; the idle composer must stay ready")
	}
}
