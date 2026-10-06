package claudecode

import (
	"strings"
	"testing"
)

// A reply that quotes a dialog question is conversation text, not a dialog:
// the composer is still painted below it. Reading it as a dialog marks the
// session not-ready and blocks every later send until the text scrolls off.
func TestDetectInputDetail_QuotedAnchorAboveComposer(t *testing.T) {
	rule := strings.Repeat("─", 80)
	for _, anchor := range []string{trustAnchor, trustAnchorAlt, bypassAnchor} {
		frame := strings.Join([]string{
			"❯ what does the startup dialog say?",
			"⏺ It asks: \"" + anchor + "\" and offers:",
			"  1. Yes, proceed",
			"  2. No, exit",
			"✻ Baked for 3s",
			rule,
			"❯ ",
			rule,
			"  ⏵⏵ auto mode on (shift+tab to cycle)",
		}, "\n")
		if req, det := DetectInputDetail(frame); det != DetectNone {
			t.Errorf("anchor %q quoted in a reply: det = %v (req %+v), want DetectNone", anchor, det, req)
		}
		// "Has my answer cleared the dialog?" must agree: a quoted anchor
		// above the composer is not a dialog still painted.
		if AnchorPresent(frame) {
			t.Errorf("anchor %q quoted in a reply: AnchorPresent = true, want false", anchor)
		}
	}
}

// The live folder-trust dialog has a rule ABOVE its question but no composer
// below it, so it must still be detected.
func TestDetectInputDetail_DialogWithRuleAbove(t *testing.T) {
	frame := strings.Join([]string{
		strings.Repeat("─", 80),
		" Accessing workspace:",
		" /tmp/proj",
		" Quick safety check: " + trustAnchorAlt,
		" ❯ No, exit",
		"   Yes, I trust this folder",
		" Enter to confirm · Esc to cancel",
	}, "\n")
	if _, det := DetectInputDetail(frame); det != DetectOK {
		t.Fatalf("det = %v, want DetectOK", det)
	}
}
