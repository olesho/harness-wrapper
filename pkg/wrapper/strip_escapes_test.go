package wrapper

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	claudeharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/claude"
)

// TestStripANSIEscapes pins the parser against each sequence family it must
// remove and each byte it must keep.
func TestStripANSIEscapes(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text", "hello world", "hello world"},
		{"SGR colour with parameters", "\x1b[38;5;402mhello\x1b[0m", "hello"},
		{"truecolour SGR", "\x1b[38;2;215;119;87m✻\x1b[39m done", "✻ done"},
		{"private mode", "\x1b[?2004h\x1b[?25lready", "ready"},
		{"CSI with an intermediate", "\x1b[2 qcursor", "cursor"},
		{"erase line before text", "line1\r\n\x1b[2KAPI Error: 529", "line1\r\nAPI Error: 529"},
		{"cursor position", "\x1b[40;1H\x1b[38;3Hat", "at"},
		{"OSC title ended by BEL", "\x1b]0;◐ Fix the rate limit\x07text", "text"},
		{"OSC title ended by ST", "\x1b]0;✳ Claude Code\x1b\\text", "text"},
		{"OSC 8 hyperlink", "\x1b]8;id=zaxmda;https://example.com/rate-limit\x1b\\docs\x1b]8;;\x1b\\", "docs"},
		{"DCS payload", "\x1bPq#0;2;0;0;0\x1b\\after", "after"},
		{"APC payload", "\x1b_quota exceeded\x1b\\after", "after"},
		{"PM and SOS payloads", "\x1b^pm\x1b\\\x1bXsos\x1b\\after", "after"},
		{"BEL does not end a DCS", "\x1bPa\x07b\x1b\\after", "after"},
		{"save and restore cursor", "\x1b7saved\x1b8", "saved"},
		{"designate a charset", "\x1b(Bplain", "plain"},
		{"keypad mode", "\x1b=\x1b>keys", "keys"},
		{"ESC restarts a sequence", "\x1b\x1b[1mbold", "bold"},
		{"ESC inside a string starts the next sequence", "\x1b]0;title\x1b[1mbold", "bold"},
		{"CAN cancels a CSI", "\x1b[31\x18x", "x"},
		{"SUB cancels a string", "\x1b]0;tit\x1ax", "x"},
		{"C0 control inside a CSI is kept", "\x1b[1\nm", "\n"},
		{"DEL inside a CSI is ignored", "\x1b[3\x7f1mx", "x"},
		{"UTF-8 after ESC ends the escape", "\x1b✻ kept", "✻ kept"},
		{"UTF-8 text is kept byte for byte", "✻ Baked for 5s · ⎿  ❯", "✻ Baked for 5s · ⎿  ❯"},
		{"UTF-8 inside a title is payload", "\x1b]0;◐ ✳ ✻\x07x", "x"},
		{"open CSI at the end is dropped", "text\x1b[38;5", "text"},
		{"open OSC at the end is dropped", "text\x1b]0;tit", "text"},
		{"lone ESC at the end is dropped", "text\x1b", "text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripANSIEscapes(tc.in); got != tc.want {
				t.Fatalf("stripANSIEscapes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// escapeRemnant matches what the old parser left behind: it ended every
// sequence at the first byte in '@'..'~', so "ESC [" and "ESC ]" were removed
// and the parameters and payloads after them were kept.
var escapeRemnant = regexp.MustCompile(`\?\d+[hl]|\d+(?:;\d+)+[mHf]|\x07|(?:^|[^\d])[08];`)

// TestStripANSIEscapes_RecordedClaude replays every recorded claude-code session
// through the stripper. Claude 2.1.270 paints its screen with truecolour SGR,
// private modes, cursor positioning, an OSC 0 window title carrying the
// conversation's topic, and OSC 8 hyperlinks; none of it may reach the text the
// classifier reads, while the plain text it prints after the TUI tears down —
// the resume hint — must arrive intact.
func TestStripANSIEscapes_RecordedClaude(t *testing.T) {
	dirs, err := filepath.Glob("../../test/corpus/claude-code/*/bytes.raw")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no claude-code recordings found: %v", err)
	}
	for _, path := range dirs {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got := stripANSIEscapes(string(raw))
			if strings.ContainsRune(got, 0x1b) {
				t.Errorf("stripped text still holds an ESC byte")
			}
			if m := escapeRemnant.FindString(got); m != "" {
				t.Errorf("stripped text holds escape-sequence remnant %q", m)
			}
			for _, title := range oscTitles(string(raw)) {
				if title != "" && strings.Contains(got, title) {
					t.Errorf("window title %q leaked into the stripped text", title)
				}
			}
			if hint := resumeHint.FindString(string(raw)); hint != "" && !strings.Contains(got, hint) {
				t.Errorf("resume hint %q did not survive stripping", hint)
			}
		})
	}
}

// resumeHint is the line claude prints to the normal screen as it exits.
var resumeHint = regexp.MustCompile(`claude --resume [0-9a-f-]{36}`)

// oscTitles returns the payloads of the OSC 0 title sequences in raw.
func oscTitles(raw string) []string {
	var out []string
	for _, m := range regexp.MustCompile("\x1b\\]0;([^\x07\x1b]*)").FindAllStringSubmatch(raw, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestHarnessAdapter_EscapeSequenceAdversarial are the classifier rows the old
// parser got wrong. Each would have classified differently had the sequence's
// parameters or payload been read as text.
func TestHarnessAdapter_EscapeSequenceAdversarial(t *testing.T) {
	claude := harnessAdapter{patterns: claudeharness.Patterns}
	now := time.Now()
	cases := []struct {
		name       string
		c          Classifier
		output     string
		wantStatus Status
		wantCode   int
		wantResume bool
	}{
		{
			// Claude Code titles the window with the conversation's topic. A
			// conversation ABOUT a rate limit must not read as one: the reply
			// here says nothing of the kind.
			name:   "a window title naming a cost phrase is not a wall",
			c:      claude,
			output: "\x1b]0;◐ Add rate limiting to the login endpoint\x07⏺ Done. All tests pass.\r\n",
		},
		{
			name:   "a hyperlink URL naming a cost phrase is not a wall",
			c:      claude,
			output: "\x1b]8;;https://docs.example.com/api/rate-limit\x1b\\the docs\x1b]8;;\x1b\\\r\n",
		},
		{
			name:   "a DCS payload naming a quota is not a wall (default classifier)",
			c:      defaultClassifier{},
			output: "\x1bPquota exceeded\x1b\\build ok\r\n",
		},
		{
			// An erase-line or colour sequence ahead of the text used to leave
			// its parameters at the start of the line, where the matcher's
			// line anchor could no longer see "API Error:".
			name:       "an API error line that starts with a sequence still classifies",
			c:          claude,
			output:     "\x1b[2K\x1b[38;2;255;107;128mAPI Error: 529 Overloaded\x1b[39m\r\n",
			wantStatus: StatusAPIError,
			wantCode:   529,
		},
		{
			name:       "a styled session-limit banner still classifies with its reset time",
			c:          claude,
			output:     "  ⎿  \x1b[1mYou've hit your session limit\x1b[22m · resets 6:40pm (UTC)\r\n",
			wantStatus: StatusBlockedByCost,
			wantResume: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.c.Classify(ClassifierInput{RecentOutput: tc.output, Quiet: true, Idle: true, SinceLastOutput: time.Hour})
			if got.Status != tc.wantStatus {
				t.Fatalf("Status = %q (reason %q), want %q", got.Status, got.Reason, tc.wantStatus)
			}
			if got.HTTPCode != tc.wantCode {
				t.Errorf("HTTPCode = %d, want %d", got.HTTPCode, tc.wantCode)
			}
			if tc.wantResume && !got.ResumeAt.After(now.Add(-24*time.Hour)) {
				t.Errorf("ResumeAt = %v, want the banner's reset time", got.ResumeAt)
			}
		})
	}
}
