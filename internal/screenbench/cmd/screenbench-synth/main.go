//go:build screenbench

// screenbench-synth generates a synthetic bake-off corpus exercising
// common TUI byte-stream patterns that real chat CLIs emit: SGR
// styling, cursor moves, partial-line rewrites, alternate-screen
// toggles, code blocks, scrollback overflow, box-drawing panels.
//
// Output goes under <out>/synth/<scenario>/ with bytes.raw, meta.json,
// and expected.txt populated. The expected text is what a faithful
// emulator should render after the full byte stream replays, after
// metrics.Normalize (trailing whitespace trimmed per line; blank lines
// trimmed top/bottom).
//
// These recordings do NOT represent real Codex/Claude Code output —
// they validate the bench plumbing and surface emulator behavior gaps
// on isolated TUI primitives. Real-harness recordings are still
// required to answer the actual fidelity question.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper-screenbench/scenario"
)

const (
	cols = 80
	rows = 24
)

type synth struct {
	name     string
	notes    string
	bytes    []byte
	expected string
}

func main() {
	out := flag.String("out", "test/corpus", "corpus root directory")
	flag.Parse()

	scenarios := []synth{
		shortReply(),
		longMarkdown(),
		codeBlock(),
		interruptMidStream(),
		altScreenToggle(),
		scrollbackOverflow(),
	}

	for _, s := range scenarios {
		dir := filepath.Join(*out, "synth", s.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bytes.raw"), s.bytes, 0o644); err != nil {
			fail("write bytes.raw: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "expected.txt"), []byte(s.expected), 0o644); err != nil {
			fail("write expected.txt: %v", err)
		}
		if err := scenario.WriteMeta(dir, scenario.Meta{
			Harness:       "synth",
			BinaryVersion: "screenbench-synth",
			RecordedAt:    time.Now().UTC(),
			Cols:          cols,
			Rows:          rows,
			Notes:         s.notes,
		}); err != nil {
			fail("write meta: %v", err)
		}
		fmt.Printf("wrote %s (%d bytes)\n", dir, len(s.bytes))
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "screenbench-synth: "+format+"\n", args...)
	os.Exit(1)
}

// ---- scenarios ----

// shortReply: a minimal two-turn exchange with simple SGR styling.
func shortReply() synth {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")              // clear screen, cursor home
	b.WriteString("\x1b[1m> \x1b[22mhello\r\n") // bold prompt, user text
	b.WriteString("\x1b[32mHi there!\x1b[0m ")  // green assistant
	b.WriteString("How can I help today?\r\n")
	b.WriteString("\x1b[1m> \x1b[22m\x1b[?25h") // new prompt, show cursor
	return synth{
		name:     "short-reply",
		notes:    "single-turn short reply with SGR styling",
		bytes:    []byte(b.String()),
		expected: "> hello\nHi there! How can I help today?\n>",
	}
}

// longMarkdown: a multi-paragraph reply with headings, bold, and bullets.
func longMarkdown() synth {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\x1b[1m> \x1b[22msummarize the plan\r\n")
	b.WriteString("\x1b[1;4mOverview\x1b[0m\r\n") // bold+underline heading
	b.WriteString("The plan has \x1b[1mthree phases\x1b[22m. Each phase is\r\n")
	b.WriteString("independently shippable.\r\n")
	b.WriteString("\r\n")
	b.WriteString("\x1b[1;4mPhases\x1b[0m\r\n")
	b.WriteString("  \x1b[33m•\x1b[0m \x1b[1mDiscovery\x1b[22m — corpus + bench\r\n")
	b.WriteString("  \x1b[33m•\x1b[0m \x1b[1mAdapters\x1b[22m — codex, claude-code\r\n")
	b.WriteString("  \x1b[33m•\x1b[0m \x1b[1mLibrary\x1b[22m — pkg/chat\r\n")
	b.WriteString("\r\n")
	b.WriteString("\x1b[2mEnd of summary.\x1b[0m\r\n")
	b.WriteString("\x1b[1m> \x1b[22m")
	exp := strings.Join([]string{
		"> summarize the plan",
		"Overview",
		"The plan has three phases. Each phase is",
		"independently shippable.",
		"",
		"Phases",
		"  • Discovery — corpus + bench",
		"  • Adapters — codex, claude-code",
		"  • Library — pkg/chat",
		"",
		"End of summary.",
		">",
	}, "\n")
	return synth{
		name:     "long-markdown",
		notes:    "multi-paragraph reply: headings, bold runs, bullet list",
		bytes:    []byte(b.String()),
		expected: exp,
	}
}

// codeBlock: a fenced code block rendered with line gutters and SGR
// syntax highlighting. The expected output strips the colors but
// preserves layout.
func codeBlock() synth {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\x1b[1m> \x1b[22mshow a hello world\r\n")
	b.WriteString("\x1b[36m```go\x1b[0m\r\n")
	lines := []string{
		"package main",
		"",
		"import \"fmt\"",
		"",
		"func main() {",
		"    fmt.Println(\"hello, world\")",
		"}",
	}
	for i, ln := range lines {
		// gutter: dim gray "  1 │ "
		b.WriteString(fmt.Sprintf("\x1b[2m%3d │\x1b[0m ", i+1))
		// naive "highlight" certain keywords just to exercise SGR
		col := strings.ReplaceAll(ln, "package", "\x1b[35mpackage\x1b[0m")
		col = strings.ReplaceAll(col, "import", "\x1b[35mimport\x1b[0m")
		col = strings.ReplaceAll(col, "func", "\x1b[35mfunc\x1b[0m")
		b.WriteString(col)
		b.WriteString("\r\n")
	}
	b.WriteString("\x1b[36m```\x1b[0m\r\n")
	b.WriteString("\x1b[1m> \x1b[22m")
	var exp strings.Builder
	exp.WriteString("> show a hello world\n```go\n")
	for i, ln := range lines {
		exp.WriteString(fmt.Sprintf("%3d │ %s\n", i+1, ln))
	}
	exp.WriteString("```\n>")
	return synth{
		name:     "code-block",
		notes:    "fenced code block with line-number gutter and SGR keyword highlighting",
		bytes:    []byte(b.String()),
		expected: exp.String(),
	}
}

// interruptMidStream: assistant starts streaming, then the stream is
// erased mid-line (\r + EL) and replaced with an "interrupted" message.
// This pattern is common in chat TUIs when the user hits Ctrl-C.
func interruptMidStream() synth {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\x1b[1m> \x1b[22mwrite a long story\r\n")
	b.WriteString("Once upon a time, in a land far away, there lived a curious")
	// User interrupts: \r + EL (erase to end of line) replaces the line.
	b.WriteString("\r\x1b[2K")
	b.WriteString("\x1b[31m⚠ interrupted by user\x1b[0m\r\n")
	b.WriteString("\x1b[1m> \x1b[22m")
	exp := strings.Join([]string{
		"> write a long story",
		"⚠ interrupted by user",
		">",
	}, "\n")
	return synth{
		name:     "interrupt-mid-stream",
		notes:    "partial assistant line erased via \\r+CSI2K and replaced with interrupt notice",
		bytes:    []byte(b.String()),
		expected: exp,
	}
}

// altScreenToggle: enter alt screen, display a "loading" frame, leave
// alt screen, then show real content on the main screen. The alt-screen
// content should NOT appear in the final main-screen extraction.
func altScreenToggle() synth {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\x1b[1m> \x1b[22mlist files\r\n")
	// Enter alt screen, draw spinner frame.
	b.WriteString("\x1b[?1049h\x1b[2J\x1b[H")
	b.WriteString("⠋ loading files...\r\n")
	// Leave alt screen — main-screen content should be restored.
	b.WriteString("\x1b[?1049l")
	b.WriteString("README.md  go.mod  pkg/\r\n")
	b.WriteString("\x1b[1m> \x1b[22m")
	exp := strings.Join([]string{
		"> list files",
		"README.md  go.mod  pkg/",
		">",
	}, "\n")
	return synth{
		name:     "alt-screen-toggle",
		notes:    "CSI ?1049h/l alt-screen entry/exit; alt-screen content must not bleed into main extraction",
		bytes:    []byte(b.String()),
		expected: exp,
	}
}

// scrollbackOverflow: emit more lines than the terminal height. A
// faithful emulator's visible screen should retain only the last `rows`
// lines after the stream completes. We exercise this with a 30-line
// numbered list on a 24-row terminal.
func scrollbackOverflow() synth {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\x1b[1m> \x1b[22mcount to 30\r\n")
	for i := 1; i <= 30; i++ {
		b.WriteString(fmt.Sprintf("line %d\r\n", i))
	}
	b.WriteString("\x1b[1m> \x1b[22m")

	// rows = 24. The screen holds 24 lines. After all writes the screen's
	// visible window is the *last 24 lines*. Compute what they are.
	// We emitted: "> count to 30", "line 1" .. "line 30", "> "
	// That's 32 logical lines; the last line is the prompt with no newline.
	// Visible 24 = last 24 rows. The cursor sits on the prompt row.
	// Expected (after Normalize trims trailing whitespace and blank-line edges):
	all := []string{"> count to 30"}
	for i := 1; i <= 30; i++ {
		all = append(all, fmt.Sprintf("line %d", i))
	}
	all = append(all, ">")
	visible := all[len(all)-rows:]
	return synth{
		name:     "scrollback-overflow",
		notes:    "32 logical lines on a 24-row screen; visible region is the trailing 24",
		bytes:    []byte(b.String()),
		expected: strings.Join(visible, "\n"),
	}
}
