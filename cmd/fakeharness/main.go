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
	path := os.Getenv(fakeharness.EnvVar)
	if path == "" {
		return fmt.Errorf("%s not set", fakeharness.EnvVar)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read script: %w", err)
	}
	var sc fakeharness.Script
	if err := json.Unmarshal(data, &sc); err != nil {
		return fmt.Errorf("parse script: %w", err)
	}

	// The PTY slave starts in canonical mode with echo. A real TUI switches to
	// raw so it can read control bytes — the CSI-13u submit carries no newline,
	// so canonical mode would block our read forever — and so the user's
	// keystrokes are not echoed onto the screen we paint. Mirror that.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			defer func() { _ = term.Restore(int(os.Stdin.Fd()), old) }()
		}
	}

	in := bufio.NewReader(os.Stdin)
	var captured string

	for _, step := range sc.Steps {
		switch {
		case step.Frame != nil:
			if step.Frame.DelayMs > 0 {
				time.Sleep(time.Duration(step.Frame.DelayMs) * time.Millisecond)
			}
			body := step.Frame.Screen
			if step.Frame.Echo {
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
			if !step.Frame.NoClear {
				body = "\x1b[2J\x1b[H" + body
			}
			if _, err := io.WriteString(os.Stdout, body); err != nil {
				return fmt.Errorf("paint frame: %w", err)
			}

		case step.WaitInput != nil:
			re, err := regexp.Compile(step.WaitInput.UntilRegex)
			if err != nil {
				return fmt.Errorf("bad wait_input regex %q: %w", step.WaitInput.UntilRegex, err)
			}
			buf, err := readUntil(in, re)
			if err != nil {
				return fmt.Errorf("wait_input %q: %w", step.WaitInput.Label, err)
			}
			if step.WaitInput.Capture {
				if loc := re.FindIndex(buf); loc != nil {
					captured = string(buf[:loc[0]])
				}
			}

		case step.Hold != nil:
			// Hold at the prompt until the wrapper closes the PTY (it kills us
			// on Conversation.Close) — the explicit form of the end-of-timeline
			// fall-through below.
			_, _ = io.Copy(io.Discard, in)
			return nil

		case step.Exit != nil:
			os.Exit(step.Exit.Code)
		}
	}

	// Stay alive after the timeline so the idle-completion watcher can confirm
	// the final turn while the process is still up — a real interactive harness
	// sits at its prompt waiting for the next message. Block until the PTY
	// closes (the parent kills us on Conversation.Close).
	_, _ = io.Copy(io.Discard, in)
	return nil
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
