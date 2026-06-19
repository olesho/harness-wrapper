package codex

import "testing"

// updateNoticeScreen is the rendered "Update available!" menu interstitial,
// from test/corpus/codex/update-notice (codex-cli 0.140.0). The "›" highlight
// sits on "Update now", so a blind Enter would launch an npm install.
const updateNoticeScreen = `
  ✨  Update available! 0.140.0 -> 0.141.0

  Release notes: https://github.com/openai/codex/releases/latest

› 1. Update now (runs ` + "`npm install -g @openai/codex`" + `)
  2. Skip
  3. Skip until next version

  Press enter to continue
`

// promptReadyScreen is the idle composer after the notice was dismissed, from
// test/corpus/codex/prompt-ready. Note the post-dismiss "Update available!"
// banner here is boxed and non-blocking (no menu rows).
const promptReadyScreen = `
╭─────────────────────────────────────────────────╮
│ ✨ Update available! 0.140.0 -> 0.141.0         │
│ Run npm install -g @openai/codex to update.     │
│                                                 │
│ See full release notes:                         │
│ https://github.com/openai/codex/releases/latest │
╰─────────────────────────────────────────────────╯

╭──────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.140.0)               │
│                                          │
│ model:     gpt-5.5   /model to change    │
│ directory: ~/Work/aether/harness-wrapper │
╰──────────────────────────────────────────╯

  Tip: Start a fresh idea with /new; the previous session stays in history.

› Run /review on my current changes

  gpt-5.5 default · ~/Work/aether/harness-wrapper
`

// migrationScreen is the model-migration interstitial (synthesized from the
// 0.140.0 binary strings; could not be triggered on demand).
const migrationScreen = `
  Choose how you'd like Codex to proceed.

  Try new model      gpt-5.5 -> gpt-6
  Use existing model

  Press enter to continue
`

func TestDetectInput_UpdateNotice(t *testing.T) {
	req, ok := DetectInput(updateNoticeScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the update notice")
	}
	if req.Kind != KindUpdateNotice {
		t.Errorf("Kind = %q, want %q", req.Kind, KindUpdateNotice)
	}
	want := []struct {
		id, alias, label string
	}{
		{"1", "update", "Update now (runs `npm install -g @openai/codex`)"},
		{"2", "skip", "Skip"},
		{"3", "skip", "Skip until next version"},
	}
	if len(req.Options) != len(want) {
		t.Fatalf("len(Options) = %d, want %d (%+v)", len(req.Options), len(want), req.Options)
	}
	for i, w := range want {
		o := req.Options[i]
		if o.ID != w.id || o.Alias != w.alias || o.Label != w.label {
			t.Errorf("Options[%d] = {id:%q alias:%q label:%q}, want {id:%q alias:%q label:%q}",
				i, o.ID, o.Alias, o.Label, w.id, w.alias, w.label)
		}
	}
	if req.ID == "" {
		t.Error("request ID is empty")
	}

	// Auto-dismiss must select Skip (digit 2), never the highlighted "Update
	// now" that would run an npm install.
	keys, ok := AutoDismissKeys(req)
	if !ok {
		t.Fatal("AutoDismissKeys refused the update notice")
	}
	if string(keys) != "2\r" {
		t.Errorf("AutoDismissKeys = %q, want %q (Skip)", keys, "2\r")
	}
}

func TestDetectInput_ModelMigration(t *testing.T) {
	req, ok := DetectInput(migrationScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the model-migration screen")
	}
	if req.Kind != KindModelMigration {
		t.Errorf("Kind = %q, want %q", req.Kind, KindModelMigration)
	}
	keys, ok := AutoDismissKeys(req)
	if !ok {
		t.Fatal("AutoDismissKeys refused the migration screen")
	}
	if string(keys) != "\r" {
		t.Errorf("AutoDismissKeys = %q, want Enter", keys)
	}
}

func TestAutoDismiss_RefusesUnknownMenu(t *testing.T) {
	// A "Press enter to continue" screen carrying an unrecognized numbered menu
	// must NOT be blind-Entered — the highlight could default to a destructive
	// row. AutoDismissKeys refuses so it surfaces for a client to answer.
	const unknownMenu = `
  Something new happened.

› 1. Delete everything
  2. Keep it

  Press enter to continue
`
	req, ok := DetectInput(unknownMenu)
	if !ok {
		t.Fatal("DetectInput did not recognize the notice")
	}
	if req.Kind != KindNotice {
		t.Errorf("Kind = %q, want %q", req.Kind, KindNotice)
	}
	if _, ok := AutoDismissKeys(req); ok {
		t.Error("AutoDismissKeys must refuse an unknown numbered menu, not blind-Enter it")
	}
}

func TestDetectInput_PromptReadyIsNotInterstitial(t *testing.T) {
	// The idle composer mentions "Update available!" in a non-blocking banner
	// but has no menu — it must not be detected as an interstitial.
	if _, ok := DetectInput(promptReadyScreen); ok {
		t.Error("DetectInput false-positived on the idle composer banner")
	}
	if !PromptReady(promptReadyScreen) {
		t.Error("PromptReady did not recognize the idle composer prompt")
	}
}

func TestDetectInput_AdversarialReplyMentioningUpdate(t *testing.T) {
	// A model reply that discusses updates and happens to contain a numbered
	// list must not be mistaken for the update menu: there is no "Skip" row.
	const reply = `
Here is what I found. There is an "Update available!" message you can ignore.
Steps to upgrade later:
  1. Run the installer
  2. Restart the app
  3. Verify the version

› Tell me what to do next
`
	if _, ok := DetectInput(reply); ok {
		t.Error("DetectInput false-positived on a reply mentioning 'Update available!'")
	}
}

func TestPromptReady_DuringInterstitialStillMatchesGlyph(t *testing.T) {
	// PromptReady alone matches the menu's "›" highlight; readiness in the
	// chat layer gates it behind DetectInput, which this asserts is needed.
	if !PromptReady(updateNoticeScreen) {
		t.Skip("update menu has no leading '›' line in this build; gate still relies on DetectInput")
	}
	if _, ok := DetectInput(updateNoticeScreen); !ok {
		t.Error("DetectInput must flag the update menu so readiness overrides the '›' match")
	}
}
