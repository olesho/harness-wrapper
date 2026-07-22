//go:build screenbench

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// script is the parsed form of a JSON recording script.
//
// Schema:
//
//	{
//	  "steps": [
//	    {"wait_for": "> "},
//	    {"send": "say hi in one word\n"},
//	    {"wait_for": "Token usage:"},
//	    {"send": "/exit\n"}
//	  ]
//	}
//
// Each step must populate exactly one of the WaitFor / Send / Sleep
// fields. Empty steps and steps with more than one field error at load
// time so misshapen scripts fail loudly rather than silently no-op.
type script struct {
	Steps []scriptStep `json:"steps"`
}

type scriptStep struct {
	// WaitFor is a Go-flavoured regexp matched against the rolling
	// recent-output buffer. Returns when the regex matches OR
	// idle-timeout elapses with no new output. Best-effort by design —
	// scripts run unattended and a slightly-off pattern shouldn't
	// hang the run.
	WaitFor string `json:"wait_for,omitempty"`

	// Send is bytes written to the harness's PTY stdin verbatim. Use
	// "\n" to submit a line.
	Send string `json:"send,omitempty"`

	// Sleep is a Go duration string (e.g. "500ms", "2s") for the
	// driver to pause before advancing. Useful for screens that paint
	// in pieces with no specific marker.
	Sleep string `json:"sleep,omitempty"`

	// Interrupt, when true, writes a single Ctrl-C byte (0x03) to the
	// harness's PTY. Exists as its own step kind because JSON cannot
	// reasonably embed a raw 0x03 in a `send` string.
	Interrupt bool `json:"interrupt,omitempty"`
}

func (s scriptStep) kind() (string, error) {
	count := 0
	if s.WaitFor != "" {
		count++
	}
	if s.Send != "" {
		count++
	}
	if s.Sleep != "" {
		count++
	}
	if s.Interrupt {
		count++
	}
	switch count {
	case 1:
		switch {
		case s.WaitFor != "":
			return "wait_for", nil
		case s.Send != "":
			return "send", nil
		case s.Interrupt:
			return "interrupt", nil
		default:
			return "sleep", nil
		}
	case 0:
		return "", errors.New("step has no wait_for / send / sleep / interrupt field")
	default:
		return "", errors.New("step has more than one of wait_for / send / sleep / interrupt")
	}
}

// loadScript reads and parses a JSON script from disk, validating that
// every step populates exactly one field.
func loadScript(path string) (*script, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read script %s: %w", path, err)
	}
	var s script
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse script %s: %w", path, err)
	}
	if len(s.Steps) == 0 {
		return nil, fmt.Errorf("script %s has no steps", path)
	}
	for i, step := range s.Steps {
		if err := validateStep(step); err != nil {
			return nil, fmt.Errorf("script %s step %d: %w", path, i, err)
		}
	}
	return &s, nil
}

// validateStep checks that a step populates exactly one field and that any
// wait_for regex and sleep duration parse.
func validateStep(step scriptStep) error {
	if _, err := step.kind(); err != nil {
		return err
	}
	if step.WaitFor != "" {
		if _, err := regexp.Compile(step.WaitFor); err != nil {
			return fmt.Errorf("invalid wait_for regex: %w", err)
		}
	}
	if step.Sleep != "" {
		if _, err := time.ParseDuration(step.Sleep); err != nil {
			return fmt.Errorf("invalid sleep duration: %w", err)
		}
	}
	return nil
}

// scriptDriver pumps a parsed script against a wrapper.Session. It
// implements io.Writer so the recorder can hand it to
// Session.AttachOutput; every byte the harness emits flows through
// Write, getting matched against the pending wait_for regex (if any)
// and updating the "last output" timestamp used by the idle-timeout
// fallback.
//
// One driver handles one script run. Concurrent Run calls are not
// supported.
type scriptDriver struct {
	stdin       stdinWriter
	idleTimeout time.Duration
	bufCap      int

	// submitKey, when non-nil, replaces the trailing "\n" of a Send step.
	// Enhanced-keyboard harnesses (claude-code, codex ≥ their enhanced
	// versions) do not treat a raw CR/LF from a PTY writer as submit — only
	// CSI 13 u (the unmodified Enter). Nil preserves the legacy raw-byte
	// behavior. See submitKeyForHarness.
	submitKey []byte

	mu        sync.Mutex
	buf       []byte
	lastOut   time.Time
	pendingRe *regexp.Regexp
	matched   chan struct{}
}

// stdinWriter is the minimum surface scriptDriver needs from
// wrapper.Session. Defined as an interface so tests can drive the
// driver without a real PTY.
type stdinWriter interface {
	WriteStdin(p []byte) (int, error)
}

// newScriptDriver returns a driver bound to stdin. idleTimeout sets
// the wait_for fallback; bufCap caps the rolling buffer (64 KiB by
// default if zero).
func newScriptDriver(stdin stdinWriter, idleTimeout time.Duration, bufCap int) *scriptDriver {
	if bufCap <= 0 {
		bufCap = 64 * 1024
	}
	return &scriptDriver{
		stdin:       stdin,
		idleTimeout: idleTimeout,
		bufCap:      bufCap,
	}
}

// Write captures bytes into the rolling buffer and signals any pending
// wait_for that matches. Always returns (len(p), nil) — never blocks
// the harness output stream.
//
// On a successful match the buffer is truncated to the bytes that
// follow the match. This is essential for multi-turn scripts: without
// it, a wait_for for the same pattern in a later step would re-fire
// instantly against the still-buffered match from the previous turn.
func (d *scriptDriver) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf = append(d.buf, p...)
	if len(d.buf) > d.bufCap {
		d.buf = d.buf[len(d.buf)-d.bufCap:]
	}
	d.lastOut = time.Now()
	if d.pendingRe != nil {
		if loc := d.pendingRe.FindIndex(d.buf); loc != nil {
			d.buf = d.buf[loc[1]:]
			d.pendingRe = nil
			select {
			case d.matched <- struct{}{}:
			default:
			}
		}
	}
	return len(p), nil
}

// Run executes every step in script in order. Returns when the last
// step completes, ctx is cancelled, or a Send fails.
//
// wait_for steps return when the regex matches OR the harness has been
// idle for idleTimeout (the "screen settled" fallback). Send steps
// write immediately. Sleep steps pause the driver but not the harness.
func (d *scriptDriver) Run(ctx context.Context, s *script) error {
	for i, step := range s.Steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.runStep(ctx, step); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
	}
	return nil
}

func (d *scriptDriver) runStep(ctx context.Context, step scriptStep) error {
	switch {
	case step.WaitFor != "":
		return d.waitFor(ctx, step.WaitFor)
	case step.Send != "":
		return d.send(step.Send)
	case step.Interrupt:
		_, err := d.stdin.WriteStdin([]byte{0x03})
		return err
	case step.Sleep != "":
		dur, _ := time.ParseDuration(step.Sleep)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dur):
			return nil
		}
	}
	return errors.New("empty step")
}

// send writes a Send step's bytes. When submitKey is set and the step ends
// in "\n" (the script convention for "type this, then submit"), the trailing
// newline is replaced by the harness's enhanced Enter so the turn actually
// runs — a raw "\n" only inserts a newline in enhanced-keyboard composers.
func (d *scriptDriver) send(s string) error {
	if d.submitKey != nil && strings.HasSuffix(s, "\n") {
		if body := strings.TrimSuffix(s, "\n"); body != "" {
			if _, err := d.stdin.WriteStdin([]byte(body)); err != nil {
				return err
			}
		}
		_, err := d.stdin.WriteStdin(d.submitKey)
		return err
	}
	_, err := d.stdin.WriteStdin([]byte(s))
	return err
}

// submitKeyForHarness returns the byte sequence that submits the composer for
// a harness, or nil to use the raw Send bytes. This mirrors
// pkg/chat.submitKeyForHarness — the inward submit-key contract — kept in
// sync so re-baked corpus is recorded the same way the wrapper drives turns.
func submitKeyForHarness(harness string) []byte {
	switch harness {
	case "claude-code", "codex":
		return []byte("\x1b[13u") // CSI 13 u — unmodified Enter in enhanced keyboard mode
	default:
		return nil
	}
}

// Shift+Tab (the permission-mode cycle key) deliberately has NO mirror here.
// Its encoding lives in pkg/chat.shiftTabForHarness — CSI 9;2u, the enhanced-
// keyboard form rather than the legacy "\x1b[Z" — and internal/fakeharness exports it
// as ShiftTabCSI9_2u for hermetic scenarios. No recorded scenario switches
// permission mode yet, so mirroring it here would be a third copy with nothing
// exercising it. If a mode-switch scenario is ever recorded, add a
// shiftTabForHarness twin of the function above with the same
// "mirrors pkg/chat.<fn> — the inward contract" note, rather than inlining the
// bytes at the call site.

// waitFor blocks until pattern matches the rolling buffer OR the
// harness has been idle for idleTimeout. Idle-timeout return is
// considered a non-error best-effort advance — the script keeps going.
func (d *scriptDriver) waitFor(ctx context.Context, pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("compile wait_for: %w", err)
	}

	d.mu.Lock()
	d.pendingRe = re
	d.matched = make(chan struct{}, 1)
	// Check the existing buffer in case the marker already arrived
	// before the script reached this step. On a hit, truncate to
	// post-match so the next wait_for doesn't see the same bytes.
	if loc := re.FindIndex(d.buf); loc != nil {
		d.buf = d.buf[loc[1]:]
		d.pendingRe = nil
		select {
		case d.matched <- struct{}{}:
		default:
		}
	}
	ch := d.matched
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.pendingRe = nil
		d.mu.Unlock()
	}()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			return nil
		case <-tick.C:
			d.mu.Lock()
			last := d.lastOut
			d.mu.Unlock()
			if !last.IsZero() && time.Since(last) >= d.idleTimeout {
				return nil
			}
		}
	}
}
