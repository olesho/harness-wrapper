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

// Interstitial request kinds. These are the only kinds the chat layer
// auto-dismisses; codex's real approval prompts (apply patch?, approve
// change?) are classified by the wrapper detector, never here, so they are
// never auto-confirmed.
const (
	KindUpdateNotice   = "codex_update_notice"
	KindModelMigration = "codex_model_migration"
	KindNotice         = "codex_notice"
)

// menuRE matches a Codex numbered menu row: an optional "›" highlight marker,
// then "N. Label", anchored to its own screen line. The label runs to the
// end of the line (trailing emulator padding is trimmed by cleanLabel).
var menuRE = regexp.MustCompile(`(?m)^[^\S\r\n]*(?:›[^\S\r\n]*)?(\d+)\.[^\S\r\n]+(.+?)[^\S\r\n]*$`)

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
		// A bare "Press enter to continue" screen. If it carries a numbered
		// menu we do not recognize, attach the parsed options and let
		// AutoDismissKeys refuse blind-Enter (a future menu could default to a
		// destructive choice like the update menu's "Update now"); a menu-less
		// notice is safely dismissed with Enter.
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
		// Only blind-Enter a menu-less notice. If a numbered menu is present we
		// do not know which row the highlight defaults to, so refuse and let
		// the request surface for a client/policy to answer.
		if len(req.Options) == 1 && req.Options[0].Alias == "continue" {
			return []byte("\r"), true
		}
		return nil, false
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
		num, label := m[1], cleanLabel(m[2])
		if seen[num] || label == "" {
			continue
		}
		seen[num] = true
		opts = append(opts, turns.InputOption{
			ID:    num,
			Alias: aliasForLabel(label),
			Label: label,
			Keys:  []byte(num + "\r"),
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
func aliasForLabel(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "skip"):
		return "skip"
	case strings.Contains(l, "update"):
		return "update"
	default:
		return ""
	}
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
