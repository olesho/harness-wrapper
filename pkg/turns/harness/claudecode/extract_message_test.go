package claudecode

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// A realistic rendered Claude Code screen: banner, the echoed prompt, the
// assistant "⏺" reply, the "✻ … for Ns" thinking footer, then the input box.
const oneShotScreen = `╭─── Claude Code v2.1.181 ───────────────────────────────────╮
│ Welcome back Oleh!                                         │
╰────────────────────────────────────────────────────────────╯

❯ Reply with exactly the word PONG and nothing else.

⏺ PONG

✻ Cooked for 4s

────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)
`

func TestExtractMessage_OneShotReply(t *testing.T) {
	got, ok := (&Adapter{}).ExtractMessage(screen.Snapshot{Text: oneShotScreen})
	if !ok {
		t.Fatal("ExtractMessage returned ok=false; want the assistant reply")
	}
	if got != "PONG" {
		t.Fatalf("ExtractMessage = %q, want %q", got, "PONG")
	}
}

// A turn that used a tool first, then gave a final message: the LAST "⏺" block
// before the footer is the reply; the tool call / its "⎿" result are skipped.
const toolThenReplyScreen = `❯ Create result.txt then say DONE.

⏺ Write(result.txt)
  ⎿  Wrote 1 line to result.txt

⏺ DONE

✻ Brewed for 6s

────────────────────────────────────────────────────────────
❯
`

func TestExtractMessage_SkipsToolCallTakesFinalMessage(t *testing.T) {
	got, ok := (&Adapter{}).ExtractMessage(screen.Snapshot{Text: toolThenReplyScreen})
	if !ok || got != "DONE" {
		t.Fatalf("ExtractMessage = %q (ok=%v), want %q", got, ok, "DONE")
	}
}

// A multi-line reply: continuation lines indented under the bullet are kept and
// dedented; the footer/box/input chrome is dropped.
const multiLineScreen = `❯ summarize

⏺ Here is the summary:
  - point one
  - point two

✻ Pondered for 2s

────────────────────────────────────────────────────────────
❯
`

func TestExtractMessage_MultiLineDedented(t *testing.T) {
	got, ok := (&Adapter{}).ExtractMessage(screen.Snapshot{Text: multiLineScreen})
	want := "Here is the summary:\n- point one\n- point two"
	if !ok || got != want {
		t.Fatalf("ExtractMessage = %q (ok=%v), want %q", got, ok, want)
	}
}

// Stale content from a prior turn must not leak: extraction is scoped to the
// LAST thinking footer, so only the most-recent turn's "⏺" block is returned.
const staleThenFreshScreen = `⏺ Old answer from a previous turn

✻ Cooked for 9s

❯ new question

⏺ Fresh answer

✻ Brewed for 1s

────────────────────────────────────────────────────────────
❯
`

func TestExtractMessage_IgnoresStalePriorTurn(t *testing.T) {
	got, ok := (&Adapter{}).ExtractMessage(screen.Snapshot{Text: staleThenFreshScreen})
	if !ok || got != "Fresh answer" {
		t.Fatalf("ExtractMessage = %q (ok=%v), want %q", got, ok, "Fresh answer")
	}
}

func TestExtractMessage_NoBulletReturnsFalse(t *testing.T) {
	if got, ok := (&Adapter{}).ExtractMessage(screen.Snapshot{Text: "just a banner\n❯\n"}); ok {
		t.Fatalf("ExtractMessage ok=true (%q); want false when no ⏺ block present", got)
	}
}

// QuitSequence is the graceful-exit keystrokes RunTurn sends before SIGTERM:
// the "/quit" slash command plus Claude's enhanced Enter (CSI 13 u), which is
// what actually submits the composer in Claude's kitty-protocol TUI.
func TestQuitSequence(t *testing.T) {
	q := (&Adapter{}).QuitSequence()
	if want := "/quit\x1b[13u"; string(q) != want {
		t.Fatalf("QuitSequence = %q, want %q", q, want)
	}
}

// ExtractSessionIDFromLine pulls the session UUID out of the "claude --resume
// <uuid>" exit hint as it appears in the raw PTY line stream — including when
// trailing ANSI styling and a carriage return ride along on the same line.
func TestExtractSessionIDFromLine(t *testing.T) {
	a := &Adapter{}
	const id = "74ca2184-c064-492c-88dc-c79c128de13e"
	cases := []struct {
		name   string
		line   string
		want   string
		wantOK bool
	}{
		{"plain", "claude --resume " + id, id, true},
		{"ansi-and-cr", "  claude --resume " + id + "\x1b[22m\r", id, true},
		{"with-prose", "Resume this conversation with: claude --resume " + id + " later", id, true},
		{"no-hint", "✻ Baked for 5s", "", false},
		{"empty", "", "", false},
		{"bad-uuid", "claude --resume not-a-uuid", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := a.ExtractSessionIDFromLine(tc.line)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("ExtractSessionIDFromLine(%q) = (%q, %v), want (%q, %v)", tc.line, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
