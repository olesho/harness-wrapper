// Command fakeharness is a scriptable stand-in for an interactive coding
// harness, used by the chat package's PTY-driven integration tests. It is not a
// product binary. It reads a JSON script (path in $FAKEHARNESS_SCRIPT), switches
// its PTY slave to raw mode like a real TUI, and replays the script's timeline:
// paint frames on a delay, block until the wrapper types an expected byte
// sequence, optionally echo the captured prompt back. See package
// internal/fakeharness for the script format and builder.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"golang.org/x/term"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeharness:", err)
		os.Exit(1)
	}
}

func run() error {
	// Dump argv BEFORE loading the script, so conformance tests can read back
	// the exact spawned args even when the script env is broken (mirrors the TS
	// fake's ordering in test/chat/fakeharness.mjs). Best-effort: any failure is
	// swallowed so it never perturbs the harness contract under test.
	dumpArgv()

	sc, err := loadScript()
	if err != nil {
		return err
	}

	// The PTY slave starts in canonical mode with echo. A real TUI switches to
	// raw so it can read control bytes — the CSI-13u submit carries no newline,
	// so canonical mode would block our read forever — and so the user's
	// keystrokes are not echoed onto the screen we paint. Mirror that.
	defer enterRawMode()()

	in := bufio.NewReader(os.Stdin)
	var captured string

	for _, step := range sc.Steps {
		done, err := runStep(in, step, &captured)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}

	// Stay alive after the timeline so the idle-completion watcher can confirm
	// the final turn while the process is still up — a real interactive harness
	// sits at its prompt waiting for the next message. Block until the PTY
	// closes (the parent kills us on Conversation.Close).
	_, _ = io.Copy(io.Discard, in)
	return nil
}

// dumpArgv writes os.Args[1:] as a single JSON array to the path named by
// $FAKEHARNESS_ARGV_OUT, when set, so tests can assert the exact argv the
// wrapper spawned the fake with (argv prepending). It is best-effort: an unset
// var, a bad path, or an unwritable file is silently ignored — this facility
// exists only for observation and must never change the fake's behavior.
func dumpArgv() {
	path := os.Getenv(fakeharness.ArgvOutVar)
	if path == "" {
		return
	}
	data, err := json.Marshal(os.Args[1:])
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// loadScript reads and parses the script named by $FAKEHARNESS_SCRIPT.
func loadScript() (fakeharness.Script, error) {
	var sc fakeharness.Script
	path := os.Getenv(fakeharness.EnvVar)
	if path == "" {
		return sc, fmt.Errorf("%s not set", fakeharness.EnvVar)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sc, fmt.Errorf("read script: %w", err)
	}
	if err := json.Unmarshal(data, &sc); err != nil {
		return sc, fmt.Errorf("parse script: %w", err)
	}
	return sc, nil
}

// enterRawMode switches the PTY slave to raw mode and returns a restore func
// (a no-op when stdin is not a terminal).
func enterRawMode() func() {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			return func() { _ = term.Restore(int(os.Stdin.Fd()), old) }
		}
	}
	return func() {
		// Not a terminal (or MakeRaw failed): nothing to restore.
	}
}

// runStep executes one timeline step. It returns done=true when the step ends
// replay (a Hold), updating captured through its pointer for a capturing
// WaitInput.
func runStep(in *bufio.Reader, step fakeharness.Step, captured *string) (bool, error) {
	switch {
	case step.Frame != nil:
		return false, paintFrame(step.Frame, *captured)

	case step.WaitInput != nil:
		return false, waitInput(in, step.WaitInput, captured)

	case step.Hold != nil:
		// Hold at the prompt until the wrapper closes the PTY (it kills us
		// on Conversation.Close) — the explicit form of the end-of-timeline
		// fall-through in run().
		_, _ = io.Copy(io.Discard, in)
		return true, nil

	case step.Exit != nil:
		os.Exit(step.Exit.Code)
	}
	return false, nil
}

// paintFrame renders one full-screen repaint step.
func paintFrame(f *fakeharness.Frame, captured string) error {
	if f.DelayMs > 0 {
		time.Sleep(time.Duration(f.DelayMs) * time.Millisecond)
	}
	body := f.Screen
	if f.Echo {
		body = strings.ReplaceAll(body, "{{prompt}}", captured)
	}
	// Raw mode disables the PTY's ONLCR (LF→CRLF) post-processing, so
	// emit CRLF explicitly. Without the CR, lines don't return to column
	// 0 — they staircase, and a long line can wrap and split a
	// detection-critical string (e.g. the resume UUID) across rows.
	body = strings.ReplaceAll(body, "\n", "\r\n")
	// Clear + home so each frame fully repaints; no stale footer can
	// bleed into a settled frame and fake Busy(). NoClear frames append
	// verbatim (classifier lines, see Builder.Raw).
	if !f.NoClear {
		body = "\x1b[2J\x1b[H" + body
	}
	if _, err := io.WriteString(os.Stdout, body); err != nil {
		return fmt.Errorf("paint frame: %w", err)
	}
	return nil
}

// waitInput blocks until the typed bytes match the step's regex, capturing the
// prefix into captured when requested.
func waitInput(in *bufio.Reader, w *fakeharness.WaitInput, captured *string) error {
	re, err := regexp.Compile(w.UntilRegex)
	if err != nil {
		return fmt.Errorf("bad wait_input regex %q: %w", w.UntilRegex, err)
	}
	buf, err := readUntil(in, re)
	if err != nil {
		return fmt.Errorf("wait_input %q: %w", w.Label, err)
	}
	if w.Capture {
		if loc := re.FindIndex(buf); loc != nil {
			*captured = stripPasteFraming(string(buf[:loc[0]]))
		}
	}
	return nil
}

// stripPasteFraming removes the bracketed-paste markers from a captured prompt.
//
// chat frames a large payload as a paste (CSI 200 ~ … CSI 201 ~) so the harness
// composer keeps it whole; a real TUI consumes those markers as FRAMING, not as
// content, and this fake must do the same or every {{prompt}} echo round-trip
// would see the escapes in the reply. Stripping rather than requiring them keeps
// the below-threshold path byte-identical.
func stripPasteFraming(s string) string {
	s = strings.TrimPrefix(s, fakeharness.PasteStart)
	return strings.TrimSuffix(s, fakeharness.PasteEnd)
}

// readUntil reads one byte at a time until the accumulated buffer matches re,
// returning the full buffer (including the match). Inputs are tiny, so the
// repeated whole-buffer match is not a concern.
func readUntil(r *bufio.Reader, re *regexp.Regexp) ([]byte, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return buf, err
		}
		buf = append(buf, b)
		if re.Match(buf) {
			return buf, nil
		}
	}
}
