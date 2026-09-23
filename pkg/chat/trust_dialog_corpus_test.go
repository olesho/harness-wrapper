package chat

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// trustCorpusAnchor is claudecode's trustAnchorAlt (claudecode.go:120) as
// 2.1.251 paints it. Restated here rather than imported because the constant is
// unexported; the pin in permission_dialog_pin_test.go keeps the two honest.
const trustCorpusAnchor = "Is this a project you created or one you trust?"

// trustDialogCorpusFrame returns the folder-trust dialog as claude 2.1.251
// actually painted it, from the live recording at
// test/corpus/claude-code/trust-dialog. That recording was made in a freshly
// `git init`-ed directory claude had never seen (see the scenario's meta.json)
// — the only way the dialog renders at all.
//
// Replayed INCREMENTALLY, for the same reason settledCorpusFrame does it (see
// its comment): 2.1.24x+ runs its TUI on the alternate screen, and both the
// answered dialog and the recorder's SIGTERM teardown blank this screen. A
// whole-file scr.Write(raw) yields a frame with no dialog on it at all and this
// test would assert on nothing. Production reads frames as they land.
//
// Unlike settledCorpusFrame this keeps the FIRST fully painted dialog frame,
// not the last. The recording deliberately answers the dialog, so the last
// anchor-carrying frame is the repaint AFTER the Down keypress, with the
// highlight already moved onto "Yes, I trust this folder". The screen under
// test is the one production actually meets — the untouched default, whose
// highlight sits on "No, exit". "Fully painted" means both rows are present:
// the anchor lands a few writes before the options do.
func trustDialogCorpusFrame(t *testing.T) string {
	t.Helper()
	path := filepath.Join(corpusRoot(t), "claude-code", "trust-dialog", "bytes.raw")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("trust-dialog corpus unavailable: %v", err)
	}
	scr := screen.New(120, 40)
	const chunk = 64
	for i := 0; i < len(raw); i += chunk {
		end := i + chunk
		if end > len(raw) {
			end = len(raw)
		}
		_, _ = scr.Write(raw[i:end])
		text := scr.Snapshot().Text
		if strings.Contains(text, trustCorpusAnchor) &&
			strings.Contains(text, "No, exit") &&
			strings.Contains(text, "Yes, I trust this folder") {
			return text
		}
	}
	t.Fatalf("trust-dialog recording never produced a frame carrying %q with both option rows", trustCorpusAnchor)
	return ""
}

// TestDetectInput_TrustDialogCorpusFrame is the regression pin for the bug this
// scenario was recorded for: on 2.1.251 the folder-trust dialog is an
// UNNUMBERED selector whose default highlight is "No, exit". Before the
// selector-menu parser landed the dialog parsed as no dialog at all, and
// answering it with a bare CR confirmed the highlighted row — quitting claude at
// startup instead of trusting the folder.
//
// Everything here is asserted against the live recording rather than a
// hand-written frame, so a future claude that repaints this screen breaks the
// test instead of silently reintroducing the quit-at-startup path.
func TestDetectInput_TrustDialogCorpusFrame(t *testing.T) {
	frame := trustDialogCorpusFrame(t)

	req, det := claudecode.DetectInputDetail(frame)
	if det != claudecode.DetectOK {
		t.Fatalf("DetectInputDetail(recorded 2.1.251 trust dialog) = %v, want DetectOK\n--- frame ---\n%s", det, frame)
	}
	if req == nil {
		t.Fatal("DetectInputDetail returned DetectOK with a nil request")
	}
	if len(req.Options) != 2 {
		t.Fatalf("got %d options, want 2 (trust / exit)\n--- frame ---\n%s", len(req.Options), frame)
	}

	byAlias := make(map[string]turns.InputOption, len(req.Options))
	for _, o := range req.Options {
		byAlias[o.Alias] = o
	}
	proceed, ok := byAlias["proceed"]
	if !ok {
		t.Fatalf("no option aliased \"proceed\"; options = %+v", req.Options)
	}
	deny, ok := byAlias["deny"]
	if !ok {
		t.Fatalf("no option aliased \"deny\"; options = %+v", req.Options)
	}

	// The assertion that pins the bug: claude highlights the DENY row by
	// default. If this ever flips, selectorKeys' relative arrow offsets are
	// derived from a different origin and every answer to this dialog moves the
	// wrong way — which is worse than a parse failure, because it is silent.
	if !deny.Highlighted {
		t.Errorf("deny option %q is not highlighted; claude 2.1.251 defaults the highlight to \"No, exit\"", deny.Label)
	}
	if proceed.Highlighted {
		t.Errorf("proceed option %q is highlighted; the recorded default is \"No, exit\"", proceed.Label)
	}

	// A bare CR would confirm whatever is highlighted — i.e. "No, exit". The
	// proceed keys must therefore move the highlight first.
	if bytes.Equal(proceed.Keys, []byte("\r")) || bytes.Equal(proceed.Keys, []byte("\n")) {
		t.Errorf("proceed option Keys = %q, a bare submit; on this frame that answers \"No, exit\" and quits claude", proceed.Keys)
	}
	if !bytes.Contains(proceed.Keys, []byte("\x1b[")) {
		t.Errorf("proceed option Keys = %q, want a cursor move before the submit", proceed.Keys)
	}
}

// TestReadyForInput_TrustDialogCorpusFrame pins the other half: this recorded
// frame must never read as ready. Send on a not-ready screen waits; Send on a
// screen wrongly called ready types the prompt into the menu and submits it
// onto the highlighted "No, exit".
func TestReadyForInput_TrustDialogCorpusFrame(t *testing.T) {
	frame := trustDialogCorpusFrame(t)
	if readyForInput(chatClaudeCode, frame) {
		t.Error("readyForInput(claude-code, recorded 2.1.251 trust dialog) = true; a blocking dialog must stay not-ready")
	}
}
