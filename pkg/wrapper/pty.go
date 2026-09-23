package wrapper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// waitResult bundles the outcome of cmd.Wait so the supervisor can
// receive it on a single channel.
type waitResult struct {
	err     error
	endedAt time.Time
}

// copyPTYOutput reads bytes from the PTY master and writes them to the
// caller's stdout, recording the timestamp of the most recent byte for
// idle detection.
func copyPTYOutput(src io.Reader, dst io.Writer, lastOutput *atomic.Int64, recentOutput *recentOutputBuffer, tap *lineSplitter) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			lastOutput.Store(time.Now().UnixNano())
			recentOutput.Write(buf[:n])
			// Durable taps (recent-output ring + the line tap) consume the raw
			// bytes BEFORE the display fanout, so the line tap is never
			// downstream of the lossy attach path. tap is nil-safe.
			tap.write(buf[:n])
			_, _ = dst.Write(buf[:n])
		}
		if err != nil {
			// EOF: deliver any final unterminated line before the goroutine ends.
			tap.flush()
			return
		}
	}
}

// classifyExit maps a finished os/exec process state into the wrapper's
// normalized status. When ctx was cancelled the wrapper considers the
// run interrupted regardless of how the child happened to exit.
func classifyExit(state *os.ProcessState, _ error, ctxErr error) (Status, int, string, string) {
	if state == nil {
		return StatusUnknown, -1, "", "process state unavailable"
	}
	if ctxErr != nil {
		return StatusInterrupted, state.ExitCode(), signalFromState(state), "context cancelled"
	}
	if state.Success() {
		return StatusIdle, 0, "", ""
	}
	if state.Exited() {
		return StatusFailed, state.ExitCode(), "", fmt.Sprintf("exit code %d", state.ExitCode())
	}
	signal := signalFromState(state)
	return StatusInterrupted, state.ExitCode(), signal, fmt.Sprintf("terminated by %s", signal)
}

// isCostOrQuotaLimited reports whether output contains a cost/quota/rate
// fingerprint, returning the matched phrase so callers can use it as a
// classification reason. The empty string with false means no match.
func isCostOrQuotaLimited(output string) (string, bool) {
	normalized := strings.ToLower(stripANSIEscapes(output))
	patterns := []string{
		"blocked by cost",
		"cost limit",
		"quota exceeded",
		"rate limit",
		"rate-limit",
		"usage limit",
		"session limit",
		"you've hit your limit",
		"you have hit your limit",
		"you've hit your session limit",
		"you have hit your session limit",
		"limit resets",
		"resets at",
		"extra usage",
	}
	for _, pattern := range patterns {
		if strings.Contains(normalized, pattern) {
			return pattern, true
		}
	}
	return "", false
}

// Bytes the escape-sequence parser in stripANSIEscapes treats specially.
const (
	ctlBEL = 0x07 // ends an OSC string (xterm's alternative to ST)
	ctlCAN = 0x18 // cancels the sequence in progress
	ctlESC = 0x1b // introduces every escape sequence
	ctlSUB = 0x1a // cancels the sequence in progress, like CAN
	ctlDEL = 0x7f // ignored inside a sequence
)

// Parser states for stripANSIEscapes.
const (
	escGround  = iota // text
	escStart          // after ESC
	escInter          // ESC + intermediate bytes, awaiting the final byte
	escCSI            // ESC [ + parameters, awaiting the final byte
	escString         // an OSC / DCS / SOS / PM / APC payload, awaiting its terminator
	escStringE        // ESC inside a string: ST when the next byte is '\'
)

// stripANSIEscapes returns the text of s with every terminal control sequence
// removed, so a classifier matches what the harness printed rather than how it
// was drawn. It follows ECMA-48 as xterm parses it:
//
//   - CSI, ESC [ <parameters 0x30-0x3F> <intermediates 0x20-0x2F> <final 0x40-0x7E>:
//     colours, cursor moves, private modes (ESC [ ? 2004 h).
//   - Control strings, ended by ST (ESC \): OSC (ESC ]), which BEL may also end,
//     and DCS (ESC P), SOS (ESC X), PM (ESC ^), APC (ESC _). Their payload is
//     never text on screen: Claude Code writes the conversation's topic into
//     the window title (OSC 0, "◐ Fix the login rate limit") and its links as
//     OSC 8 hyperlinks whose URL rides in the payload.
//   - Two-byte escapes, ESC + an optional run of intermediates + a final byte
//     0x30-0x7E: ESC 7 (save cursor), ESC ( B (designate ASCII), ESC =.
//
// The parser this replaces ended every sequence at the first byte in '@'..'~'.
// '[' and ']' are in that range, so it stripped only "ESC [" and "ESC ]": the
// parameters of every CSI (38;5;402m, ?2004h) and the whole payload of every
// OSC (the title, the hyperlink URL) were left in the text the classifier
// reads. The title quoting a cost phrase could then fire a wall on a reply
// that never mentioned one, and a line that began with a sequence (ESC [ 2 K,
// erase line) no longer began with its text, so the line-anchored API-error
// and session-limit matchers missed a real one.
//
// Cursor motion is removed, not rendered. Claude Code 2.1.270 places the words
// of a freshly painted line with CSI <n> G (cursor to column) instead of
// writing the blanks between them, so such a line strips to "TheWizard'sLibrary";
// only the vt100 screen (pkg/screen) has its spaces. Turning motion into blanks
// here would widen what the unanchored phrase arms match in a live run, which
// is the opposite of what this function is for.
//
// Inside a sequence, CAN and SUB cancel it, ESC starts a new one, DEL is
// ignored and the other C0 controls are kept (a terminal executes them without
// ending the sequence). A byte at or above 0x80 inside CSI or an escape ends
// the sequence and is kept as text; inside a control string it is payload
// (titles are UTF-8). C1 controls are never recognised in their 8-bit form:
// in UTF-8 output 0x9B and 0x9C are continuation bytes ("✻" is E2 9C BB). A
// sequence still open at the end of s is dropped; the next poll sees it whole.
func stripANSIEscapes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	state := escGround
	belEnds := false // the control string is an OSC, which BEL also ends
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case escGround:
			if c == ctlESC {
				state = escStart
				continue
			}
			b.WriteByte(c)
		case escString:
			switch {
			case c == ctlESC:
				state = escStringE
			case c == ctlCAN || c == ctlSUB || (c == ctlBEL && belEnds):
				state = escGround
			}
		case escStringE:
			if c == '\\' {
				state = escGround
				continue
			}
			// ESC ended the string and began another sequence; c is its first
			// byte after the ESC.
			state = escStart
			i--
		default: // escStart, escInter, escCSI
			next := stepEscape(state, c, &b)
			if next == escString {
				belEnds = c == ']'
			}
			state = next
		}
	}
	return b.String()
}

// stepEscape advances the escape, intermediate and CSI states of
// stripANSIEscapes by one byte, writing to b the bytes that are text.
func stepEscape(state int, c byte, b *strings.Builder) int {
	switch {
	case c == ctlESC:
		return escStart
	case c == ctlCAN || c == ctlSUB:
		return escGround
	case c == ctlDEL:
		return state
	case c < 0x20:
		b.WriteByte(c)
		return state
	case c >= 0x80:
		b.WriteByte(c)
		return escGround
	}
	switch state {
	case escStart:
		switch {
		case c == '[':
			return escCSI
		case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
			return escString
		case c <= 0x2f:
			return escInter
		default:
			return escGround
		}
	case escInter:
		if c <= 0x2f {
			return escInter
		}
		return escGround
	default: // escCSI
		if c <= 0x3f {
			return escCSI
		}
		return escGround
	}
}

func signalFromState(state *os.ProcessState) string {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return status.Signal().String()
	}
	return ""
}

// isBinaryNotFound reports whether err from pty.Start is a result of the
// underlying binary not existing.
func isBinaryNotFound(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errors.Is(execErr.Err, exec.ErrNotFound) || errors.Is(execErr.Err, os.ErrNotExist)
	}
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}
