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

// promptReady141Screen is the idle composer on codex-cli 0.141.0. It dropped the
// space after the "›" glyph and butts the placeholder hint right against it
// ("›Find…"), unlike 0.140.0's "› Run /review…". Requiring a trailing space made
// PromptReady miss this, so the chat layer never sent a prompt and codex produced
// no reply (session-mode codex was wedged until the glyph-only match below).
const promptReady141Screen = `
╭───────────────────────────────────────╮
│ >_ OpenAI Codex (v0.141.0)            │
│                                       │
│ model:     gpt-5.5   /model to change │
│ directory: /private/tmp               │
╰───────────────────────────────────────╯

  Tip: Use /fast to enable our fastest inference with increased plan usage.

›Find and fix a bug in @filename

  gpt-5.5 default · /private/tmp
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

func TestAutoDismiss_InformationalNoticeBlindEnters(t *testing.T) {
	// A "Press enter to continue" notice carrying informational rows (e.g. a
	// "What's new" / changelog screen) is dismissed with a bare Enter — the
	// continuation Codex advertises. The one actionable "Press enter to continue"
	// menu, the sign-in wall, is excluded upstream (see
	// TestDetectInput_SigninWallExcluded), so a remaining KindNotice is safe to
	// clear. Mirrors the TS port; keeps a multi-line notice from wedging a run.
	const noticeMenu = `
  What's new in Codex

› 1. View the changelog
  2. Learn about /fast

  Press enter to continue
`
	req, ok := DetectInput(noticeMenu)
	if !ok {
		t.Fatal("DetectInput did not recognize the notice")
	}
	if req.Kind != KindNotice {
		t.Errorf("Kind = %q, want %q", req.Kind, KindNotice)
	}
	keys, ok := AutoDismissKeys(req)
	if !ok {
		t.Fatal("AutoDismissKeys refused an informational notice; want a bare Enter")
	}
	if string(keys) != "\r" {
		t.Errorf("AutoDismissKeys = %q, want Enter", keys)
	}
}

// TestDetectInput_SigninWallExcluded locks in that Codex's logged-out sign-in
// wall is NOT classified as a dismissable interstitial. It renders "Press enter
// to continue" but is an auth wall handled by the auth-required path; treating
// it as a notice would surface a spurious codex_notice and, with blind-Enter,
// could kick off a real sign-in. Screen text mirrors test/corpus/auth/codex.
func TestDetectInput_SigninWallExcluded(t *testing.T) {
	const signinMenu = `
  Welcome to Codex, OpenAI's command-line coding agent
  Sign in with ChatGPT to use Codex as part of your paid plan
  or connect an API key for usage-based billing
> 1. Sign in with ChatGPT
  2. Sign in with Device Code
  3. Provide your own API key
  Press enter to continue
`
	if req, ok := DetectInput(signinMenu); ok {
		t.Errorf("DetectInput classified the sign-in wall as %q; want (nil, false)", req.Kind)
	}

	const browserMenu = `
  Finish signing in via your browser
  Press enter to continue
`
	if req, ok := DetectInput(browserMenu); ok {
		t.Errorf("DetectInput classified the browser sign-in screen as %q; want (nil, false)", req.Kind)
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

func TestPromptReady_Codex0141Composer(t *testing.T) {
	// Regression: codex 0.141.0 renders the composer prompt as "›Find…" with no
	// space after the glyph. Requiring a trailing space made PromptReady return
	// false, so the chat layer never sent the prompt and the turn never ran.
	if !PromptReady(promptReady141Screen) {
		t.Error("PromptReady did not recognize the codex 0.141.0 idle composer")
	}
	if _, ok := DetectInput(promptReady141Screen); ok {
		t.Error("DetectInput false-positived on the 0.141.0 idle composer")
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
