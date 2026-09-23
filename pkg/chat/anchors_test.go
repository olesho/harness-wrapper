package chat

import (
	"strings"
	"testing"
)

// The anchors are exported so a consumer stops copying them. loom had mirrored
// these patterns into internal/agenterr against v0.7.7 and, by v0.10, held 4 of
// the 6 onboarding anchors — missing both of claude's OAuth sign-in walls —
// with its copies matching anywhere on screen where these are line-anchored.
// Every test here is about keeping that from happening again.

// TestAuthAnchors_AgreeWithAuthRequired is the property that makes the export
// safe to rely on: an anchor set that disagreed with the predicate it came from
// would be a second source of truth, which is the thing being removed.
func TestAuthAnchors_AgreeWithAuthRequired(t *testing.T) {
	screens := []string{
		"Let's get started.\n  Choose the text style that looks best\n",
		"Select login method\n  1. Claude account with subscription\n",
		"  Browser didn't open? Use the url below to sign in\n  https://claude.ai/oauth\n",
		"  Paste code here if prompted > \n",
		"› Sign in with ChatGPT\n  Provide your own API key\n",
		"  Finish signing in via your browser\n",
		"Claude Code\n\nNot logged in · Run /login\n\n❯ ",
		"Invalid API key · Please run /login\n",
		"Error: 401 Unauthorized\n",
		"missing bearer or basic authentication\n",
		"run: codex login\n",
		// negatives
		"⏺ Done — the migration is applied.\n\n❯ ",
		"⏺ The docs explain how to run /login when a session expires.\n",
		"",
	}
	for _, harness := range []string{chatClaudeCode, "codex"} {
		for _, screen := range screens {
			want := authRequired(harness, screen)
			got := false
			for _, a := range AuthAnchors(harness) {
				if a.RE.MatchString(screen) {
					got = true
					break
				}
			}
			if got != want {
				t.Errorf("harness %q: AuthAnchors say %v, authRequired says %v for %q",
					harness, got, want, screen)
			}
		}
	}
}

// TestAuthAnchors_OnboardingFirst pins the ORDER. A caller records the first
// hit as the arm this package would have taken, and authRequired evaluates the
// onboarding wizard before the logged-out banner — a screen showing both is a
// wizard, because that one will never become a usable composer on its own.
func TestAuthAnchors_OnboardingFirst(t *testing.T) {
	const both = "Claude Code\n  Select login method\n  1. Claude account\n  Not logged in\n❯ "
	anchors := AuthAnchors(chatClaudeCode)
	first := ""
	for _, a := range anchors {
		if a.RE.MatchString(both) {
			first = a.ID
			break
		}
	}
	if first != "claude.onboarding.select_login_method" {
		t.Errorf("first hit = %q, want the onboarding arm", first)
	}
}

// TestAuthAnchors_IDsAreAContract spells the ids out. loom's
// docs/adr/0002-authfailure-stays-terminal.md names them in its revisit
// triggers, so a rename has to be a conscious edit here rather than a silent
// break downstream.
func TestAuthAnchors_IDsAreAContract(t *testing.T) {
	want := map[string]bool{
		"claude.onboarding.theme_picker":        true,
		"claude.onboarding.select_login_method": true,
		"claude.onboarding.oauth_browser_open":  true,
		"claude.onboarding.oauth_paste_code":    true,
		"claude.loggedout.run_login":            true,
		"claude.loggedout.not_logged_in":        true,
		"claude.loggedout.invalid_api_key":      true,
		"codex.onboarding.sign_in_with_chatgpt": true,
		"codex.onboarding.browser_signin":       true,
		"codex.loggedout.401_unauthorized":      true,
		"codex.loggedout.missing_bearer":        true,
		"codex.loggedout.not_logged_in":         true,
		"codex.loggedout.codex_login":           true,
	}
	got := map[string]bool{}
	for _, a := range AuthAnchors("") {
		if got[a.ID] {
			t.Errorf("duplicate anchor id %q", a.ID)
		}
		got[a.ID] = true
		if a.RE == nil {
			t.Errorf("anchor %q has no regex", a.ID)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("anchor id %q is gone — it is named in loom's ADR-0002", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Logf("new anchor id %q — add it to the list once it is meant to be a contract", id)
		}
	}
}

// TestAuthAnchors_EmptyHarnessIsEveryHarness covers the caller that describes a
// screen without knowing which harness drew it, and TestAuthAnchors_Unknown the
// one that asks about a harness with no banner set.
func TestAuthAnchors_HarnessSelection(t *testing.T) {
	all := AuthAnchors("")
	if len(all) != len(AuthAnchors(chatClaudeCode))+len(AuthAnchors("codex")) {
		t.Errorf("AuthAnchors(\"\") = %d anchors, want claude + codex", len(all))
	}
	for _, h := range []string{"opencode", "pi", "nonesuch"} {
		if got := AuthAnchors(h); got != nil {
			t.Errorf("AuthAnchors(%q) = %v, want nil", h, got)
		}
		if got := DialogAnchors(h); got != nil {
			t.Errorf("DialogAnchors(%q) = %v, want nil", h, got)
		}
	}
}

// TestDialogAnchors covers the other half of the evidence question: a verdict
// taken over a folder-trust or bypass-acceptance screen is about a DIALOG, not
// a credential.
func TestDialogAnchors(t *testing.T) {
	got := DialogAnchors(chatClaudeCode)
	if len(got) != 3 {
		t.Fatalf("got %d dialog anchors, want 3: %v", len(got), got)
	}
	screen := "Claude Code\n\nDo you trust the files in this folder?\n\n❯ 1. Yes\n"
	hit := false
	for _, a := range got {
		if strings.Contains(screen, a) {
			hit = true
		}
	}
	if !hit {
		t.Errorf("no dialog anchor matched a folder-trust screen: %v", got)
	}
}
