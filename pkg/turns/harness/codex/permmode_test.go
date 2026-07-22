package codex

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// rightAligned renders one 80-column screen row with s pushed to the right
// edge, then right-pads to the terminal width the way pkg/screen does
// (screen.go:24-26 keeps per-row trailing whitespace). Padding on BOTH sides is
// the point: the marker sits in a right-alignment gutter, and no `$` anchor may
// be relied on to find the row's end.
func rightAligned(s string) string {
	const cols = 80
	if pad := cols - len([]rune(s)); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// planScreen is the idle composer with codex's collaboration marker painted.
// Codex renders it ONLY while the mode is Plan, right-aligned on the composer's
// hint row; the default mode paints nothing there at all.
func planScreen(marker string) string {
	return "\n" +
		"  Tip: Use /fast to enable our fastest inference.        \n" +
		"                                                        \n" +
		"› Find and fix a bug in @filename                        \n" +
		"\n" +
		rightAligned(marker) + "\n" +
		"  gpt-5.5 default · /private/tmp                         \n"
}

func TestCollaborationMode_PlanMarker(t *testing.T) {
	cases := []struct {
		name   string
		marker string
	}{
		{"with hint", "Plan mode (shift+tab to cycle)"},
		{"without hint", "Plan mode"},
		{"emulator-widened gaps", "Plan   mode   (shift+tab   to   cycle)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, ok := collaborationMode(planScreen(tc.marker))
			if !ok || mode != "plan" {
				t.Errorf("collaborationMode = (%q, %v), want (\"plan\", true)", mode, ok)
			}
		})
	}
}

// The marker survives a trailing padding run after it: pkg/screen right-pads
// every row to the terminal width, so the parser must not depend on the marker
// ending the row's runes.
func TestCollaborationMode_PlanMarkerRightPadded(t *testing.T) {
	screenText := planScreen("Plan mode (shift+tab to cycle)") + strings.Repeat(" ", 40) + "\n"
	if mode, ok := collaborationMode(screenText); !ok || mode != "plan" {
		t.Errorf("collaborationMode = (%q, %v), want (\"plan\", true)", mode, ok)
	}

	padded := "  chrome\n" + rightAligned("Plan mode") + strings.Repeat(" ", 12) + "\n› \n"
	if mode, ok := collaborationMode(padded); !ok || mode != "plan" {
		t.Errorf("padded marker: collaborationMode = (%q, %v), want (\"plan\", true)", mode, ok)
	}
}

// A healthy codex screen that paints no marker is the DEFAULT mode, not an
// unreadable screen. These are the package's own idle-composer frames.
func TestCollaborationMode_NoMarkerIsDefault(t *testing.T) {
	for name, screenText := range map[string]string{
		"0.140.0 composer": promptReadyScreen,
		"0.141.0 composer": promptReady141Screen,
	} {
		t.Run(name, func(t *testing.T) {
			mode, ok := collaborationMode(screenText)
			if !ok || mode != "default" {
				t.Errorf("collaborationMode = (%q, %v), want (\"default\", true)", mode, ok)
			}
		})
	}
}

// The real 0.142.2 recordings settle at the composer with no marker painted, so
// they must read as default — the same frames codex_test.go feeds to OnScreen.
func TestCollaborationMode_CorpusScenariosAreDefault(t *testing.T) {
	for _, scenario := range []string{"short-reply", "long-markdown", "code-block", "tool-call", "multi-turn"} {
		t.Run(scenario, func(t *testing.T) {
			scr := screen.New(120, 40)
			_, _ = scr.Write(corpusBytes(t, scenario))
			mode, ok := New().PermissionMode(scr.Snapshot())
			if !ok || mode != "default" {
				t.Errorf("PermissionMode(%s) = (%q, %v), want (\"default\", true)", scenario, mode, ok)
			}
		})
	}
}

// ("", false) is reserved for a screen with NO readable signal: no composer at
// all. Absence of the Plan marker must never land here.
func TestCollaborationMode_UnreadableScreens(t *testing.T) {
	const signinWall = `
  Sign in with ChatGPT

› 1. Sign in with ChatGPT
  2. Provide your own API key

  Press enter to continue
`
	for name, screenText := range map[string]string{
		"blank":            "",
		"whitespace only":  strings.Repeat(" ", 80) + "\n" + strings.Repeat(" ", 80) + "\n",
		"sign-in wall":     signinWall,
		"update notice":    updateNoticeScreen,
		"model migration":  migrationScreen,
		"banner, no input": "  ✨ Update available! 0.140.0 -> 0.141.0\n  Run npm install -g @openai/codex to update.\n",
	} {
		t.Run(name, func(t *testing.T) {
			mode, ok := collaborationMode(screenText)
			if ok || mode != "" {
				t.Errorf("collaborationMode = (%q, %v), want (\"\", false)", mode, ok)
			}
		})
	}
}

// Reply prose that merely talks about plan mode must not be read as the footer
// marker — the marker is a right-aligned, row-terminal token, prose is not.
func TestCollaborationMode_ProseMentionIsNotPlan(t *testing.T) {
	prose := "\n" +
		"• I'll stay in Plan mode until you tell me otherwise, then switch.\n" +
		"• Running codex in plan mode means no edits are applied.\n" +
		"• Use \"Plan mode (shift+tab to cycle)\" is how the footer reads it.\n" +
		"\n" +
		"› Find and fix a bug in @filename\n" +
		"\n" +
		"  gpt-5.5 default · /private/tmp\n"
	mode, ok := collaborationMode(prose)
	if !ok || mode != "default" {
		t.Errorf("prose mention: collaborationMode = (%q, %v), want (\"default\", true)", mode, ok)
	}
}

// The refusal banner codex paints when /plan is typed mid-task must NOT be
// classified as a blocking dialog: readyForInput calls DetectInput's blocking
// arm (pkg/chat/ready.go:176-180), so classifying it would make every screen
// carrying the banner read as not-ready and deadlock the send path. Recognizing
// the banner is the live driver's job, not the readiness gate's.
func TestDetectInput_PlanDisabledBannerIsNotBlocking(t *testing.T) {
	const banner = `
■ '/plan' is disabled while a task is in progress.

› Find and fix a bug in @filename

  gpt-5.5 default · /private/tmp
`
	if req, blocking := DetectInput(banner); blocking {
		t.Fatalf("DetectInput classified the /plan refusal banner as blocking: %+v", req)
	}
	if !PromptReady(banner) {
		t.Error("PromptReady = false on the /plan refusal banner; the composer is still up")
	}
	if mode, ok := collaborationMode(banner); !ok || mode != "default" {
		t.Errorf("collaborationMode = (%q, %v), want (\"default\", true)", mode, ok)
	}
}

// The adapter method does the snapshot unwrap and delegates, mirroring
// claudecode.(*Adapter).Busy.
func TestAdapterPermissionMode(t *testing.T) {
	var _ turns.PermissionModeDetector = New()

	scr := screen.New(80, 24)
	_, _ = scr.Write([]byte(strings.ReplaceAll(planScreen("Plan mode (shift+tab to cycle)"), "\n", "\r\n")))
	if mode, ok := New().PermissionMode(scr.Snapshot()); !ok || mode != "plan" {
		t.Errorf("PermissionMode = (%q, %v), want (\"plan\", true)", mode, ok)
	}

	blank := screen.New(80, 24)
	if mode, ok := New().PermissionMode(blank.Snapshot()); ok || mode != "" {
		t.Errorf("PermissionMode(blank) = (%q, %v), want (\"\", false)", mode, ok)
	}
}
