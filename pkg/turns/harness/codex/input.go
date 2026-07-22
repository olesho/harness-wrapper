package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// Codex paints a few blocking startup interstitials that wait for a keypress
// before the composer accepts input. Recorded on codex-cli 0.140.0
// (test/corpus/codex/update-notice):
//
//	  ✨  Update available! 0.140.0 -> 0.141.0
//	  Release notes: https://github.com/openai/codex/releases/latest
//	› 1. Update now (runs `npm install -g @openai/codex`)
//	  2. Skip
//	  3. Skip until next version
//	  Press enter to continue
//
// The notice is a menu whose default highlight (›) sits on "Update now", so a
// blind Enter would launch an npm install. Auto-dismiss must select "Skip"
// instead — see aliasForLabel and the chat layer's auto-dismiss.
//
// A second interstitial is the model-migration screen ("Choose how you'd like
// Codex to proceed … Press enter to continue"); it has no safe-default menu we
// must avoid, so it is dismissed with Enter.
const (
	updateAnchor    = "Update available!"
	migrationAnchor = "Choose how you'd like Codex to proceed"
	continueAnchor  = "Press enter to continue"
)

// approvalAnchors are the full-sentence questions codex renders at the top of a
// genuine command / apply-patch approval dialog. Captured live from codex-cli
// 0.144.4 (test/corpus/codex/approval-command, approval-patch). Full sentences,
// not prose fragments, so the ready-side gate (pkg/chat/ready.go) stays tight.
var approvalAnchors = []string{
	"Would you like to run the following command?",
	"Would you like to make the following edits?",
}

// Interstitial request kinds. These three are the only kinds the chat layer
// auto-dismisses; codex's real approval prompts are classified HERE as
// KindApproval (see detectApproval) and are excluded from auto-dismiss by kind
// — AutoDismissKeys' default arm returns (nil, false) for them, and
// tryAutoDismissCodex in pkg/chat/input.go matches only the interstitial kinds
// — so they are never auto-confirmed.
const (
	KindUpdateNotice   = "codex_update_notice"
	KindModelMigration = "codex_model_migration"
	KindNotice         = "codex_notice"
	// KindApproval marks a genuine command / apply-patch approval prompt. This
	// exact string is pinned by the chat contract fixture
	// (pkg/chat/codex_dismiss_test.go) and by orche's default handler contract
	// — do not rename.
	KindApproval = "approval_prompt"
)

// signinWallRE identifies Codex's logged-out onboarding / sign-in screens.
// They render "Press enter to continue" too, but they are an AUTH WALL — a
// first-run sign-in wizard, not a dismissable interstitial. The chat layer's
// auth-required path (onboardingWall / authRequired in ready.go) already holds
// them as not-ready and short-circuits Send with ReasonAuthRequired. If
// DetectInput also classified them as a codex_notice, the adapter would surface
// a spurious input_request AND (once notices are safely blind-Entered below) a
// bare Enter could pick the highlighted "Sign in with ChatGPT" row and kick off
// a real sign-in. So DetectInput excludes them entirely. Anchors mirror
// codexOnboardingRE in ready.go (kept in sync via test/corpus/auth).
var signinWallRE = regexp.MustCompile(`(?i)sign in with chatgpt|finish signing in via your browser`)

// menuRE matches a Codex numbered menu row: an optional "›" highlight marker,
// then "N. Label", anchored to its own screen line. The label runs to the
// end of the line (trailing emulator padding is trimmed by cleanLabel).
// Group 1 captures the "›" marker on the currently-selected row (empty when
// absent), group 2 the digit, group 3 the label.
var menuRE = regexp.MustCompile(`(?m)^[^\S\r\n]*(›)?[^\S\r\n]*(\d+)\.[^\S\r\n]+(.+?)[^\S\r\n]*$`)

// promptRE matches the idle composer prompt indicator on its own line — the
// "›" Codex prints at the start of the input box once it is ready for input.
// 0.140.0 rendered "› <placeholder>" (a space after the glyph); 0.141.0 dropped
// that space and butts the placeholder hint right against it ("›Find and fix a
// bug in @filename"). Requiring a trailing space made readiness silently miss the
// 0.141.0 composer — the prompt was never sent and codex produced no reply — so
// we match the glyph alone. Safe because readiness only consults this once the
// interstitial gate (DetectInput) has confirmed no blocking menu is present.
var promptRE = regexp.MustCompile(`(?m)^[^\S\r\n]*›`)

// DetectInput recognizes a blocking startup interstitial in the rendered
// screen text and returns the structured request, or (nil, false) when none
// is present. Pure function: the turn adapter and the chat readiness gate
// share it as the single source of truth for what counts as a blocking
// codex interstitial.
func DetectInput(text string) (*turns.InputRequest, bool) {
	// KindApproval is checked FIRST — before updateAnchor / migration / continue
	// — for two safety reasons (both would otherwise mis-handle an approval
	// dialog whose body incidentally quotes an interstitial anchor):
	//  1. continueAnchor→KindNotice and migrationAnchor→KindModelMigration both
	//     AUTO-DISMISS with a bare "\r", which on an approval dialog would press
	//     Enter on the highlighted "Yes" — i.e. auto-approve. The approval footer
	//     is "Press enter to confirm or esc to cancel" (not "…continue"), so no
	//     real dialog collides today, but the ordering makes that guarantee
	//     independent of codex's exact footer wording.
	//  2. The updateAnchor branch `return nil, false`s the WHOLE function when
	//     its skip gate fails — an approval body mentioning "Update available!"
	//     would be swallowed entirely, silently reviving the false-readiness
	//     failure this detection exists to kill.
	if req, ok := detectApproval(text); ok {
		return req, true
	}
	// The logged-out sign-in wall renders "Press enter to continue" but is an
	// auth wall handled by the auth-required path — never a dismissable
	// interstitial (see signinWallRE).
	if signinWallRE.MatchString(text) {
		return nil, false
	}
	switch {
	case strings.Contains(text, updateAnchor):
		opts := parseMenuOptions(text)
		req := &turns.InputRequest{Kind: KindUpdateNotice, Prompt: updateAnchor, Options: opts}
		// Require a parsed "Skip" row to confirm this is the live update menu,
		// not the boxed post-dismiss banner (no menu) nor a model reply that
		// merely mentions "Update available!" alongside an unrelated numbered
		// list. Requiring Skip also guarantees AutoDismissKeys has a safe
		// option to pick instead of the highlighted "Update now".
		if findByAlias(req, "skip") == nil {
			return nil, false
		}
		req.ID = inputID(req)
		return req, true

	case strings.Contains(text, migrationAnchor):
		req := &turns.InputRequest{Kind: KindModelMigration, Prompt: migrationAnchor, Options: continueOption()}
		req.ID = inputID(req)
		return req, true

	case strings.Contains(text, continueAnchor):
		// A bare "Press enter to continue" screen. The one actionable menu that
		// renders this anchor — the logged-out sign-in wall — is excluded above
		// (signinWallRE), so what reaches here is an informational notice, and
		// AutoDismissKeys blind-Enters it. Any numbered rows are attached as
		// options for a surfacing client, but they do not change the dismissal:
		// a menu-less notice and an informational multi-row notice both clear
		// with Enter.
		opts := parseMenuOptions(text)
		if len(opts) == 0 {
			opts = continueOption()
		}
		req := &turns.InputRequest{Kind: KindNotice, Prompt: continueAnchor, Options: opts}
		req.ID = inputID(req)
		return req, true

	default:
		return nil, false
	}
}

// detectApproval recognizes a genuine command / apply-patch approval dialog and
// returns the structured request, or (nil, false).
//
// The gate is MANDATORY-STRICT, not best-effort: once this surfaces, the chat
// readiness gate (pkg/chat/ready.go) blocks sends and idle-completion, so a
// false positive DEADLOCKS the turn — strictly worse than the false-readiness
// miss it replaces. Beyond the anchor it therefore requires ALL of:
//   - a proceed-aliased parsed row,
//   - a deny-aliased parsed row (mirrors the update dialog's skip-row gate), and
//   - the "›" highlight marker on at least one PARSED menu row.
//
// The highlight is a per-row property (parseMenuOptions records it from menuRE's
// marker group), NOT a screen-wide regex: scrollback prompt echoes render past
// prompts as "› <text>" rows, so a user prompt that began with "1. " echoes as
// "› 1. …" and a screen-wide scan would match it anywhere on screen — combined
// with a quoted anchor and a proceed/deny-shaped enumeration the whole gate
// would false-positive into a deadlocked turn.
//
// The per-row flag alone is NOT sufficient either, because parseMenuOptions
// reads the WHOLE screen: an echo row is itself a parsed row, so
// "› 4. Deploy the thing" above a prose spoof lends its highlight to the gate
// (digit dedup only saves the case where the echo's digit collides with a real
// menu digit). So the rows are parsed from the text AFTER the anchor — codex
// renders scrollback above the dialog, so a past-prompt echo can never sit
// inside that tail. Verified against the corpus: the live dialogs' menus follow
// their anchor, so this does not perturb their parsed options or their inputID.
//
// Residual (accepted, documented): a highlighted numbered row rendered BELOW a
// prose-quoted anchor — e.g. the user typing "4. something" into the composer
// while such a reply is on screen — is still counted. Codex replaces the
// composer with the dialog while a real approval is up, so this shape is
// contrived; the ready-side gate is independent of it.
func detectApproval(text string) (*turns.InputRequest, bool) {
	idx, anchor := -1, ""
	for _, a := range approvalAnchors {
		if i := strings.Index(text, a); i >= 0 {
			idx, anchor = i, a
			break
		}
	}
	if idx < 0 {
		return nil, false
	}
	opts := parseMenuOptions(text[idx+len(anchor):])
	req := &turns.InputRequest{Kind: KindApproval, Prompt: anchor, Options: opts}
	if findByAlias(req, "proceed") == nil {
		return nil, false
	}
	if findByAlias(req, "deny") == nil {
		return nil, false
	}
	highlighted := false
	for _, o := range opts {
		if o.Highlighted {
			highlighted = true
			break
		}
	}
	if !highlighted {
		return nil, false
	}
	req.ID = inputID(req)
	return req, true
}

// PromptReady reports whether the idle composer prompt is on screen. Callers
// must first confirm no interstitial is present (DetectInput) — the update
// menu also renders a "›" highlight that this would otherwise match.
func PromptReady(text string) bool {
	return promptRE.MatchString(text)
}

// AutoDismissKeys returns the keystrokes that safely dismiss an interstitial
// request without triggering a destructive action, and whether the request is
// an auto-dismissable interstitial at all. For the update menu it selects the
// "skip" option (never "update now"); for the other interstitials it presses
// Enter.
func AutoDismissKeys(req *turns.InputRequest) ([]byte, bool) {
	if req == nil {
		return nil, false
	}
	switch req.Kind {
	case KindUpdateNotice:
		if o := findByAlias(req, "skip"); o != nil {
			return o.Keys, true
		}
		// No "Skip" row parsed — refuse rather than risk pressing the
		// highlighted "Update now".
		return nil, false
	case KindNotice:
		// A KindNotice is a "Press enter to continue" screen with no recognized
		// action tokens. The one real Codex screen that IS an actionable menu —
		// the logged-out sign-in wall — is excluded upstream in DetectInput, so
		// what remains is an informational notice (e.g. the "What's new" /
		// changelog screen) whose advertised continuation is Enter. Clear it with
		// a bare CR regardless of how many informational rows parseMenuOptions
		// extracted, matching the TS port and keeping a multi-line notice from
		// wedging an unattended run.
		return []byte("\r"), true
	case KindModelMigration:
		return []byte("\r"), true
	default:
		return nil, false
	}
}

// continueOption is the single "press Enter to continue" choice for
// interstitials without a parseable menu.
func continueOption() []turns.InputOption {
	return []turns.InputOption{{ID: "continue", Alias: "continue", Label: "Continue", Keys: []byte("\r")}}
}

// parseMenuOptions extracts the numbered choices from the update menu,
// de-duplicating by choice number so a redraw painting the menu twice yields
// one option set. Each option carries "<digit>\r": the digit selects the row
// regardless of the initial highlight, Enter confirms.
func parseMenuOptions(text string) []turns.InputOption {
	var opts []turns.InputOption
	seen := make(map[string]bool)
	for _, m := range menuRE.FindAllStringSubmatch(text, -1) {
		highlighted, num, label := m[1] != "", m[2], cleanLabel(m[3])
		// Dedup keeps the FIRST occurrence of a digit, so a scrollback echo that
		// collides with an already-parsed menu digit is dropped. Note this is NOT
		// by itself a defense against echoes lending a spurious "›" highlight to
		// the approval gate (a non-colliding digit survives) — detectApproval
		// parses from the anchor tail for that; see its comment.
		if seen[num] || label == "" {
			continue
		}
		seen[num] = true
		opts = append(opts, turns.InputOption{
			ID:          num,
			Alias:       aliasForLabel(label),
			Label:       label,
			Keys:        []byte(num + "\r"),
			Highlighted: highlighted,
		})
	}
	return opts
}

// cleanLabel strips trailing column padding from a captured menu label.
func cleanLabel(s string) string {
	if i := strings.Index(s, "  "); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// aliasForLabel maps a menu label to a portable intent so the auto-dismiss and
// policies can target "skip" without knowing the concrete wording. "Skip until
// next version" and "Skip" both map to "skip"; "Update now" maps to "update"
// so it is never selected by the safe auto-dismiss.
//
// ORDER MATTERS. The interstitial tokens are tested first so classification of
// update / notice menus is byte-identical to before the approval vocabulary was
// added ("Skip" must not become deny-adjacent). Notice/menu option aliases may
// shift (a "Continue" row now aliases "proceed" where it had ""), but nothing
// downstream acts on notice option aliases.
func aliasForLabel(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "skip"):
		return "skip"
	case strings.Contains(l, "update"):
		return "update"
	// Yes/No approval vocabulary, mirroring the claude-code adapter.
	case containsAny(l, "proceed", "accept", "trust", "yes", "continue"):
		return "proceed"
	// The deny tokens below are comma/space-suffixed ("no,", "no ") on purpose
	// so they never match "now"/"notice"; that leaves a bare "No" (lowercasing
	// to exactly "no") matching neither, so this exact-match case is required —
	// the approval gate DEMANDS a deny row, and real dialogs render a bare
	// "2. No".
	case l == "no":
		return "deny"
	case containsAny(l, "exit", "deny", "reject", "cancel", "no,", "no ", "don't", "do not"):
		return "deny"
	default:
		return ""
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func findByAlias(req *turns.InputRequest, alias string) *turns.InputOption {
	for i := range req.Options {
		if req.Options[i].Alias == alias {
			return &req.Options[i]
		}
	}
	return nil
}

// inputID derives a stable id from the request's identity (kind + prompt +
// option labels) so consecutive redraws of one interstitial collapse to a
// single request while a genuinely different one gets a fresh id.
func inputID(req *turns.InputRequest) string {
	var b strings.Builder
	b.WriteString(req.Kind)
	b.WriteByte(0)
	b.WriteString(req.Prompt)
	for _, o := range req.Options {
		b.WriteByte(0)
		b.WriteString(o.Label)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
