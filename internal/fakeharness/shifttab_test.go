package fakeharness_test

import (
	"regexp"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// matchesStream replays typed bytes one at a time through the step regex the
// way cmd/fakeharness.readUntil does (append a byte, re-match the whole buffer)
// and reports whether the step would ever unblock.
func matchesStream(t *testing.T, pattern, typed string) bool {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("compile %q: %v", pattern, err)
	}
	var buf []byte
	for i := range len(typed) {
		buf = append(buf, typed[i])
		if re.Match(buf) {
			return true
		}
	}
	return false
}

// shiftTabPattern pulls the regex AwaitShiftTab put in the script, so the test
// asserts against what the fake binary actually compiles — not a re-typed copy.
func shiftTabPattern(t *testing.T) string {
	t.Helper()
	s := fakeharness.New("claude-code").AwaitShiftTab().Build()
	if len(s.Steps) != 1 || s.Steps[0].WaitInput == nil {
		t.Fatalf("AwaitShiftTab produced %#v, want exactly one WaitInput step", s.Steps)
	}
	w := s.Steps[0].WaitInput
	if w.Capture {
		t.Error("AwaitShiftTab must not capture: a Shift+Tab keypress carries no prompt text")
	}
	if w.Label != "shift-tab" {
		t.Errorf("AwaitShiftTab label = %q, want %q", w.Label, "shift-tab")
	}
	return w.UntilRegex
}

// TestAwaitShiftTabMatchesConstant is the positive half of the contract: a
// script that writes ShiftTabCSI9_2u unblocks the step, including when the
// keypress arrives after unrelated leading bytes (the fake matches on a rolling
// buffer, not an exact-equality compare).
func TestAwaitShiftTabMatchesConstant(t *testing.T) {
	pattern := shiftTabPattern(t)
	cases := []struct {
		name  string
		typed string
	}{
		{"exact", fakeharness.ShiftTabCSI9_2u},
		{"after leading text", "hello" + fakeharness.ShiftTabCSI9_2u},
		{"twice", fakeharness.ShiftTabCSI9_2u + fakeharness.ShiftTabCSI9_2u},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !matchesStream(t, pattern, tc.typed) {
				t.Errorf("AwaitShiftTab regex %q did not match %q", pattern, tc.typed)
			}
		})
	}
}

// TestAwaitShiftTabRejectsOtherEncodings is the negative half, and the reason
// this test exists at all: if the regex escaping of ESC / "[" / ";" were wrong
// the pattern could go loose and match the legacy CSI Z form or a bare tab,
// letting a scenario pass against bytes the wrapper never sends.
func TestAwaitShiftTabRejectsOtherEncodings(t *testing.T) {
	pattern := shiftTabPattern(t)
	cases := []struct {
		name  string
		typed string
	}{
		{"legacy CSI Z", "\x1b[Z"},
		{"bare tab", "\t"},
		{"tab then shift", "\t\x1b[2u"},
		{"submit key", fakeharness.SubmitCSI13u},
		{"unmodified tab CSI u", "\x1b[9u"},
		{"different modifier", "\x1b[9;5u"},
		{"literal pattern text", "\x1b[9;2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if matchesStream(t, pattern, tc.typed) {
				t.Errorf("AwaitShiftTab regex %q matched %q, want no match", pattern, tc.typed)
			}
		})
	}
}

// TestShiftTabConstantBytes pins the exported constant itself: CSI 9 ; 2 u —
// Tab codepoint 9, Shift modifier 2, in the kitty keyboard protocol. The twin
// assertion on the production side is pkg/chat.TestShiftTabMatchesFakeharness.
func TestShiftTabConstantBytes(t *testing.T) {
	if want := "\x1b[9;2u"; fakeharness.ShiftTabCSI9_2u != want {
		t.Fatalf("ShiftTabCSI9_2u = %q, want %q", fakeharness.ShiftTabCSI9_2u, want)
	}
}
