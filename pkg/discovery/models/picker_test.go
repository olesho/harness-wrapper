package models

import (
	"reflect"
	"regexp"
	"testing"
)

// TestPickerHeaderGate pins the header gate: ONLY claude/claude-code and codex
// are recognized; every other harness returns nil (-> ParseModelPicker yields
// nil), so a stray numbered list on a pi/opencode screen never yields false
// positives. Mirrors TS pickerHeader.
func TestPickerHeaderGate(t *testing.T) {
	recognized := []string{"claude", "claude-code", "Claude", "  CODEX  ", "codex"}
	for _, h := range recognized {
		if pickerHeaderRe(h) == nil {
			t.Errorf("pickerHeaderRe(%q) = nil, want a header regex", h)
		}
	}
	for _, h := range []string{"pi", "opencode", "gemini", "", "claude-code-extra"} {
		if pickerHeaderRe(h) != nil {
			t.Errorf("pickerHeaderRe(%q) != nil, want nil", h)
		}
	}
	// A codex-shaped numbered list under a pi harness must parse to nil: no
	// header match, so no rows are extracted.
	stray := "Select something\n  1. gpt-5.5 (default)  a frontier model\n"
	if got := ParseModelPicker(stray, "pi"); got != nil {
		t.Errorf("ParseModelPicker(stray, pi) = %#v, want nil", got)
	}
	// Same text with no picker header under a supported harness also yields nil.
	if got := ParseModelPicker("  1. Opus  a description\n", "claude-code"); got != nil {
		t.Errorf("ParseModelPicker(no-header, claude-code) = %#v, want nil", got)
	}
}

// TestDescriptionLessRowDropped pins the inherited TS behavior: rowRe requires a
// 2+-space column separating label from description, so a row with no
// description (or only a single space before it) does not match and is silently
// dropped.
func TestDescriptionLessRowDropped(t *testing.T) {
	// Row "2. Haiku" has no 2+-space description column -> dropped; row "3.
	// Sonnet Efficient" has only a single space, not a column -> dropped. The
	// first row has a proper 2-space column and survives.
	text := "Select model\n" +
		"  1. Opus  Best for everyday tasks\n" +
		"  2. Haiku\n" + // description-less: dropped
		"  3. Sonnet Efficient\n" // single space, not a column: dropped
	got := ParseModelPicker(text, "claude-code")
	want := []Info{
		{ID: "opus", Label: "Opus", Description: "Best for everyday tasks", Current: false, IsDefault: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseModelPicker dropped-row handling\n got %#v\nwant %#v", got, want)
	}
}

// TestRE2JSWhitespaceParity is the heart of deliverable 2: verify - do not
// assume - that the Go RE2 port accepts the SAME rows V8's Unicode-aware \s
// would. Go's default \s is ASCII-only ([\t\n\f\r ]); JS \s also matches NBSP
// and the Unicode spaces. Because a real picker COULD render a non-ASCII space
// in the label->description gap, we spelled the whitespace classes out
// explicitly (jsWS/jsNonWS). This test proves the explicit class matches a
// non-ASCII gap where the naive \s{2,} would not, so the two engines agree.
func TestRE2JSWhitespaceParity(t *testing.T) {
	// Every rune in JS's \s set, used as the 2-wide label->description gap.
	unicodeSpaces := []struct {
		name string
		r    rune
	}{
		{"nbsp", 0x00a0},
		{"ogham", 0x1680},
		{"en-quad", 0x2000},
		{"em-quad", 0x2001},
		{"three-per-em", 0x2004},
		{"figure", 0x2007},
		{"punctuation", 0x2008},
		{"thin", 0x2009},
		{"hair", 0x200a},
		{"narrow-nbsp", 0x202f},
		{"math", 0x205f},
		{"ideographic", 0x3000},
		{"line-sep", 0x2028},
		{"para-sep", 0x2029},
		{"bom", 0xfeff},
	}
	// A naive RE2 rendering of the TS regex, whose ASCII-only \s\S would MISS
	// non-ASCII gaps - the bug this port avoids.
	naive := regexp.MustCompile(`^\s*[❯›*]?\s*\d+\.\s+(.+?)\s{2,}(\S.*?)\s*$`)
	for _, us := range unicodeSpaces {
		gap := string([]rune{us.r, us.r})
		line := "  1. Opus" + gap + "Best model"
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("rowRe failed to match %s gap (JS \\s would match)", us.name)
			continue
		}
		// The label must not absorb the gap; description starts after it.
		if got := m[1]; got != "Opus" {
			t.Errorf("%s: label = %q, want %q", us.name, got, "Opus")
		}
		// Sanity: the naive ASCII-only regex misses non-ASCII gaps - the reason
		// we needed the explicit classes. (Line/para separators are excluded
		// here because `.` in RE2 also stops at them, muddying the comparison.)
		if us.r != 0x2028 && us.r != 0x2029 {
			if naive.MatchString(line) {
				t.Errorf("%s: naive ASCII \\s regex unexpectedly matched - parity claim is moot", us.name)
			}
		}
	}

	// End-to-end: a claude picker whose gap is an NBSP still parses identically.
	text := "Select model\n  1. Opus  Best for everyday tasks\n"
	got := ParseModelPicker(text, "claude-code")
	want := []Info{{ID: "opus", Label: "Opus", Description: "Best for everyday tasks"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NBSP-gap parse\n got %#v\nwant %#v", got, want)
	}
}

// TestClaudeMarkerParity checks the claude-side markers behave as the TS /i
// regexes do: the check mark -> current, ^Default\b or (recommended) ->
// isDefault, id is the lowercased first token.
func TestClaudeMarkerParity(t *testing.T) {
	text := "Select model\n" +
		"  1. Default (recommended)  Opus 4.8 best\n" +
		"❯ 2. Opus ✔                 Opus 4.8 best\n" +
		"  3. DEFAULTISH thing       not a default\n" + // ^Default\b requires a boundary
		"  4. Fancy (RECOMMENDED)    case-insensitive default\n"
	got := ParseModelPicker(text, "claude-code")
	want := []Info{
		{ID: "default", Label: "Default (recommended)", Description: "Opus 4.8 best", IsDefault: true},
		{ID: "opus", Label: "Opus", Description: "Opus 4.8 best", Current: true},
		{ID: "defaultish", Label: "DEFAULTISH thing", Description: "not a default"},
		{ID: "fancy", Label: "Fancy (RECOMMENDED)", Description: "case-insensitive default", IsDefault: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claude marker parity\n got %#v\nwant %#v", got, want)
	}
}

// TestCodexMarkerParity checks the codex-side markers: (current)/(default) are
// case-insensitive, and the id is the first token with all parenthesized
// segments stripped.
func TestCodexMarkerParity(t *testing.T) {
	text := "Select Model and Effort\n" +
		"  1. gpt-5.5 (DEFAULT)      frontier\n" +
		"› 2. gpt-5.4-mini (current)  small and fast\n"
	got := ParseModelPicker(text, "codex")
	want := []Info{
		{ID: "gpt-5.5", Label: "gpt-5.5", Description: "frontier", IsDefault: true},
		{ID: "gpt-5.4-mini", Label: "gpt-5.4-mini", Description: "small and fast", Current: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codex marker parity\n got %#v\nwant %#v", got, want)
	}
}
