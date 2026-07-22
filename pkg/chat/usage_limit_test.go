package chat

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// The usage/session-limit wall is the sibling of the logged-out banner handled in
// auth_paths_test.go, and it fails DIFFERENTLY: the wall is painted as an assistant
// bubble, so it IS the extracted "reply". authRelabel's empty-clean-extraction gate
// therefore declines, and without usageLimitRelabel the turn is persisted as a
// SUCCESS carrying the wall as its text — which a downstream validator rejects as a
// bogus answer, retrying until a transient quota outage blocks the task.

// TestUsageLimitMessage pins the matcher: the wall's exact phrasings match (with or
// without the tool-result decoration glyph, "session" or "usage"), the reset tail
// rides along, and ordinary prose that merely mentions a limit does not match.
func TestUsageLimitMessage(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		in      string
		want    string // "" ⇒ expect no match
	}{
		{
			name:    "decorated wall with reset time",
			harness: chatClaudeCode,
			in:      "  ⎿  You've hit your session limit · resets 10:20pm (Europe/Warsaw)\n     /usage-credits to finish what you're working on.\n",
			want:    "You've hit your session limit · resets 10:20pm (Europe/Warsaw)",
		},
		{
			name:    "bare wall, no decoration",
			harness: chatClaudeCode,
			in:      "You've hit your session limit · resets 8pm (UTC)",
			want:    "You've hit your session limit · resets 8pm (UTC)",
		},
		{
			name:    "usage phrasing with a bullet bubble",
			harness: chatClaudeCode,
			in:      "⏺ You've hit your usage limit · resets 9:15am (UTC)\n\n✻ Brewed for 0s\n",
			want:    "You've hit your usage limit · resets 9:15am (UTC)",
		},
		{
			name:    "spelled-out 'have hit'",
			harness: chatClaudeCode,
			in:      "You have hit your session limit · resets midnight",
			want:    "You have hit your session limit · resets midnight",
		},
		{
			name:    "wall embedded in a fuller screen",
			harness: chatClaudeCode,
			in:      "⏺ Working on it…\n\n  ⎿  You've hit your session limit · resets 10:20pm (Europe/Warsaw)\n\n❯ \n",
			want:    "You've hit your session limit · resets 10:20pm (Europe/Warsaw)",
		},
		{
			// The false-positive guard: a genuine reply DISCUSSING quota must not be
			// relabeled — the CLI only ever emits the anchored "You('ve| have) hit
			// your … limit" sentence.
			name:    "prose about usage limits does not match",
			harness: chatClaudeCode,
			in:      "⏺ Your plan's usage limit resets nightly; the session limit is separate.\n",
			want:    "",
		},
		{
			name:    "a reply quoting the phrase mid-sentence does not match",
			harness: chatClaudeCode,
			in:      "⏺ The docs say that when you've hit your session limit the CLI prints a banner.\n",
			want:    "",
		},
		{
			// Only claude-code has a known wall today; other harnesses must not be
			// scanned with claude's phrasing.
			name:    "codex is not scanned",
			harness: "codex",
			in:      "You've hit your session limit · resets 8pm (UTC)",
			want:    "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := usageLimitMessage(tc.harness, tc.in)
			if tc.want == "" {
				if ok {
					t.Fatalf("usageLimitMessage matched %q, want no match", got)
				}
				return
			}
			if !ok {
				t.Fatalf("usageLimitMessage found no wall, want %q", tc.want)
			}
			if got != tc.want {
				t.Errorf("usageLimitMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUsageLimitRelabel_WallAsReply is the false-success killer: the wall arrives as
// the extracted reply (a "⏺" bubble), so the turn "completes" carrying it. The
// relabel must convert it to a terminal ReasonUsageLimited, keep the reset time in
// the reason, and clear the text so the wall is never persisted as an answer.
func TestUsageLimitRelabel_WallAsReply(t *testing.T) {
	const wall = "You've hit your session limit · resets 10:20pm (Europe/Warsaw)"
	snap := screen.Snapshot{Text: "⏺ " + wall + "\n\n✻ Brewed for 0s\n\n❯ \n"}
	c := &Conversation{opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New()}

	// Precondition: this is a NON-empty extraction, which is exactly why
	// authRelabel cannot catch it.
	if c.cleanAssistantText(snap) == "" {
		t.Fatalf("precondition: the wall should be extracted as a reply")
	}
	turn := &Turn{
		State:  TurnStateComplete,
		Reason: "claude-code: end-of-turn marker confirmed at a settled prompt",
		Text:   wall, // the false-success text the bug would persist
	}
	if !c.usageLimitRelabel(turn, snap) {
		t.Fatalf("usageLimitRelabel did not fire on a usage-limit wall")
	}
	if turn.State != TurnStateErrored {
		t.Errorf("State = %q, want %q", turn.State, TurnStateErrored)
	}
	if !strings.HasPrefix(turn.Reason, ReasonUsageLimited) {
		t.Errorf("Reason = %q, want the ReasonUsageLimited prefix", turn.Reason)
	}
	if !strings.Contains(turn.Reason, "resets 10:20pm") {
		t.Errorf("Reason = %q, want the reset time carried along", turn.Reason)
	}
	if turn.Text != "" {
		t.Errorf("Text = %q, want empty (the wall must not be persisted as a reply)", turn.Text)
	}
}

// The other rendering: the wall appears only under a "⎿" tool-result decoration, so
// nothing is extracted as a reply. authRelabel would decline too (a quota wall is
// not a logout banner), and the turn would complete with the whole screen as its
// text. The whole-screen probe fallback must still catch it.
func TestUsageLimitRelabel_WallOnScreenOnly(t *testing.T) {
	snap := screen.Snapshot{Text: "  ⎿  You've hit your usage limit · resets 9:15am (UTC)\n\n❯ \n"}
	c := &Conversation{opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New()}
	if c.cleanAssistantText(snap) != "" {
		t.Fatalf("precondition: a ⎿-decorated wall should not extract as a reply")
	}
	turn := &Turn{State: TurnStateComplete, Text: snap.Text}
	if !c.usageLimitRelabel(turn, snap) {
		t.Fatalf("usageLimitRelabel did not fire via the whole-screen probe")
	}
	if turn.State != TurnStateErrored || turn.Text != "" {
		t.Errorf("turn = {State:%q Text:%q}, want errored with empty text", turn.State, turn.Text)
	}
}

// Negative: a genuine reply is never touched, and — the ordering invariant — a
// logged-out screen still relabels as ReasonAuthRequired rather than being swallowed
// by the usage-limit check that now runs first at both completion sites.
func TestUsageLimitRelabel_LeavesOtherOutcomesAlone(t *testing.T) {
	c := &Conversation{opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New()}

	t.Run("genuine reply untouched", func(t *testing.T) {
		snap := screen.Snapshot{Text: "⏺ Done — the session limit handling is in place.\n\n✻ Brewed for 2s\n\n❯ \n"}
		turn := &Turn{State: TurnStateComplete, Text: "Done — the session limit handling is in place."}
		if c.usageLimitRelabel(turn, snap) {
			t.Errorf("usageLimitRelabel wrongly fired on a genuine reply")
		}
		if turn.State != TurnStateComplete {
			t.Errorf("State = %q, want unchanged %q", turn.State, TurnStateComplete)
		}
	})

	t.Run("logged-out screen still routes to authRelabel", func(t *testing.T) {
		snap := screen.Snapshot{Text: loadCorpusScreen(t, "claude-code/not-logged-in-brewed")}
		turn := &Turn{State: TurnStateComplete, Text: snap.Text}
		if c.usageLimitRelabel(turn, snap) {
			t.Fatalf("usageLimitRelabel wrongly claimed a logged-out screen")
		}
		if !c.authRelabel(turn, snap) {
			t.Fatalf("authRelabel should still fire for the logged-out screen")
		}
		if turn.Reason != ReasonAuthRequired {
			t.Errorf("Reason = %q, want ReasonAuthRequired", turn.Reason)
		}
	})
}
