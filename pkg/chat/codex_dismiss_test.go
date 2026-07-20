package chat

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
	codexharness "github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
)

func codexUpdateRequest() *turns.InputRequest {
	return &turns.InputRequest{
		ID:     "upd-1",
		Kind:   codexharness.KindUpdateNotice,
		Prompt: "Update available!",
		Options: []turns.InputOption{
			{ID: "1", Alias: "update", Label: "Update now", Keys: []byte("1\r")},
			{ID: "2", Alias: "skip", Label: "Skip", Keys: []byte("2\r")},
			{ID: "3", Alias: "skip", Label: "Skip until next version", Keys: []byte("3\r")},
		},
	}
}

// Default (no AutoSkipCodexUpdateNotice): the update menu is SURFACED to the
// client so it can choose Update / Skip — the chat layer writes nothing and
// keeps the request pending.
func TestCodexAutoDismiss_Default(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{Harness: "codex"}, rec)

	c.handleInputRequested(codexUpdateRequest())

	if len(rec.data) != 0 {
		t.Errorf("wrote %q, want nothing (update menu surfaces by default)", rec.data)
	}
	if !c.inputSurfaced {
		t.Error("inputSurfaced = false, want true (update menu surfaces to client)")
	}
	select {
	case ev := <-c.eventCh:
		if ev.Type != EventInputRequest || ev.Input == nil || ev.Input.Kind != codexharness.KindUpdateNotice {
			t.Errorf("surfaced event = %+v, want EventInputRequest(codex_update_notice)", ev)
		}
	default:
		t.Error("expected the update menu to surface as an EventInputRequest")
	}
}

// With AutoSkipCodexUpdateNotice set (the headless default), the update menu is
// cleared by selecting Skip and nothing is surfaced — the pre-existing safe
// behavior, now opt-in.
func TestCodexAutoSkipUpdateNotice(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{Harness: "codex", AutoSkipCodexUpdateNotice: true}, rec)

	c.handleInputRequested(codexUpdateRequest())

	if got := string(rec.data); got != "2\r" {
		t.Errorf("wrote %q, want %q (Skip)", got, "2\r")
	}
	if c.inputSurfaced {
		t.Error("inputSurfaced = true, want false for auto-skipped update menu")
	}
	select {
	case ev := <-c.eventCh:
		t.Errorf("unexpected surfaced event for auto-skipped update menu: %+v", ev)
	default:
	}
}

// With auto-dismiss disabled, the interstitial surfaces to the client instead
// of being answered.
func TestCodexAutoDismiss_Disabled(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{Harness: "codex", DisableCodexAutoDismiss: true}, rec)

	c.handleInputRequested(codexUpdateRequest())

	if len(rec.data) != 0 {
		t.Errorf("wrote %q, want nothing when auto-dismiss disabled", rec.data)
	}
	if !c.inputSurfaced {
		t.Error("inputSurfaced = false, want true when surfacing to client")
	}
	if c.currentInput == nil || c.currentInput.Kind != codexharness.KindUpdateNotice {
		t.Error("expected the update notice to remain the pending input")
	}
}

// A real codex approval prompt (not an interstitial kind) is never
// auto-dismissed, in either mode — it surfaces for the client to decide.
func TestCodexAutoDismiss_NeverTouchesApprovals(t *testing.T) {
	approval := func() *turns.InputRequest {
		return &turns.InputRequest{
			ID:     "ap-1",
			Kind:   "approval_prompt",
			Prompt: "apply patch?",
			Options: []turns.InputOption{
				{ID: "1", Alias: "proceed", Label: "Yes", Keys: []byte("1\r")},
				{ID: "2", Alias: "deny", Label: "No", Keys: []byte("2\r")},
			},
		}
	}

	for _, disable := range []bool{false, true} {
		rec := &keyRecorder{}
		c := newTestConv(Options{Harness: "codex", DisableCodexAutoDismiss: disable}, rec)
		c.handleInputRequested(approval())
		if len(rec.data) != 0 {
			t.Errorf("disable=%v: auto-wrote %q to an approval prompt; must never happen", disable, rec.data)
		}
		if !c.inputSurfaced {
			t.Errorf("disable=%v: approval prompt was not surfaced to the client", disable)
		}
	}
}
