package claudecode

import (
	"strings"
	"testing"
)

// composerQuotingAnchor is the false positive this file exists for, reproduced
// on v0.8.2 rather than theorised: an agent DISCUSSING the trust dialog renders
// the anchor in its own reply, which arms the unnumbered fallback (the anchor
// test is a Contains over the whole screen), and the only ❯ left on the screen
// is the COMPOSER's. The block walk then climbed into the input box's rules and
// produced four bogus options — one of them aliased "proceed" with Keys "\r",
// i.e. "submit whatever is typed in the composer". Both consumers answer
// trust_prompt with proceed by default, so this silently sent the operator's
// half-written line.
const composerQuotingAnchor = "" +
	"⏺ The ticket says the anchor is \"Is this a project you created or one you trust?\" and menuRE\n" +
	"  needs numbered options.\n" +
	"\n" +
	"✻ Baked for 3s\n" +
	"\n" +
	"────────────────────────────────────────────\n" +
	"❯ yes please continue\n" +
	"────────────────────────────────────────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

// Same screen with an EMPTY composer. This one already passed before the fix
// (markerRE needs a non-space after the glyph); pin it so dropping ">" from the
// marker set cannot regress it.
const composerQuotingAnchorEmpty = "" +
	"⏺ The ticket says the anchor is \"Is this a project you created or one you trust?\".\n" +
	"\n" +
	"────────────────────────────────────────────\n" +
	"❯ \n" +
	"────────────────────────────────────────────\n"

// A real, well-formed dialog whose second option ABUTS a box rule. The rule
// must terminate the block instead of becoming a third "option" labelled with
// the border glyphs.
const trustDialogRuleBelow = "" +
	"Quick safety check: Is this a project you created or one you trust? (Like your own code).\n" +
	"\n" +
	"❯ No, exit\n" +
	"  Yes, I trust this folder\n" +
	"────────────────────────────────────────────\n" +
	"Enter to confirm · Esc to cancel\n"

// Prose that QUOTES the dialog's options with markdown "> " quoting. ">" is not
// a highlight glyph in any harness — ❯ is claude-code's, › is codex's — so this
// must not parse as a menu.
const quotedOptionsProse = "" +
	"⏺ The dialog reads:\n" +
	"\n" +
	"  Is this a project you created or one you trust?\n" +
	"\n" +
	"  > Yes, I trust this folder\n" +
	"  > No, exit\n"

// anchorFarAboveMenu puts a syntactically perfect menu far below the anchor —
// the shape of a long transcript that quoted the anchor near the top and has an
// unrelated selector near the bottom. The menu of a real dialog sits within a
// handful of rows of its question.
func anchorFarAboveMenu() string {
	var b strings.Builder
	b.WriteString("⏺ Quoting: Is this a project you created or one you trust?\n")
	for i := 0; i < maxAnchorToMenuRows+3; i++ {
		b.WriteString("  transcript line\n")
	}
	b.WriteString("\n❯ No, exit\n  Yes, I trust this folder\n")
	return b.String()
}

// The regression that would otherwise have shipped: a composer must never be
// read as a menu.
func TestDetectInput_ComposerQuotingAnchorNotDetected(t *testing.T) {
	req, ok := DetectInput(composerQuotingAnchor)
	if ok {
		t.Fatalf("composer parsed as a trust menu (%d options: %+v) — answering it would submit "+
			"whatever is typed in the input box", len(req.Options), req.Options)
	}
}

func TestDetectInput_EmptyComposerQuotingAnchorNotDetected(t *testing.T) {
	if req, ok := DetectInput(composerQuotingAnchorEmpty); ok {
		t.Fatalf("empty composer parsed as a trust menu: %+v", req.Options)
	}
}

func TestDetectInput_ChromeAdjacentToMenuIsNotAnOption(t *testing.T) {
	req, ok := DetectInput(trustDialogRuleBelow)
	if !ok {
		t.Fatal("a real dialog whose menu abuts a box rule stopped being detected")
	}
	if len(req.Options) != 2 {
		t.Fatalf("got %d options, want exactly 2: %+v", len(req.Options), req.Options)
	}
	for _, o := range req.Options {
		if boxOrRuleRE.MatchString(o.Label) {
			t.Errorf("border line became an option: %q", o.Label)
		}
	}
}

func TestDetectInput_QuotedOptionsAreNotAMenu(t *testing.T) {
	if req, ok := DetectInput(quotedOptionsProse); ok {
		t.Fatalf("markdown-quoted prose parsed as a menu: %+v", req.Options)
	}
}

func TestDetectInput_MenuFarBelowAnchorNotDetected(t *testing.T) {
	if req, ok := DetectInput(anchorFarAboveMenu()); ok {
		t.Fatalf("a menu %d+ rows below the anchor was bound to it: %+v", maxAnchorToMenuRows, req.Options)
	}
}
