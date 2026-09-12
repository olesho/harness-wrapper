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
//	  "idle_timeout": "20s",
//	  "max_duration": "4m",
//	  "steps": [
//	    {"wait_for": "❯ "},
//	    {"send": "say hi in one word\n"},
//	    {"wait_for": "✻ [A-Z][a-zA-Z]+ for \\d+[hms].*done"},
//	    {"sleep": "8s"}
//	  ]
//	}
//
// Each step must populate exactly one of the WaitFor / Send / Sleep /
// Interrupt fields (interrupt_key is a modifier on an interrupt step, not a
// kind of its own). Empty steps and steps with more than one field error at
// load time so misshapen scripts fail loudly rather than silently no-op.
//
// THE SINGLE MOST EXPENSIVE FACT ABOUT THIS FILE: wait_for matches the RAW PTY
// BYTE STREAM, not the rendered screen. The buffer it searches holds the
// harness's output verbatim — SGR colour changes, cursor moves and line-wrap
// escapes interleaved with the text. Two consequences for every anchor written
// against it:
//
//   - Anchors must be short and contiguous WITHIN ONE STYLED RUN. A pattern
//     spanning a colour change (very likely across "✻ <verb> for <dur>" ->
//     "· done <clock>", which Claude renders in different styles) can fail to
//     match a screen that visibly shows it.
//   - Text the emulator composes but the harness never emitted as one run —
//     anything reflowed or overwritten — is not there to match at all.
//
// And an anchor that never matches does NOT fail the bake: waitFor advances on
// the idle timeout instead (see its doc comment). A green recording is
// therefore not proof the anchor matched; grep the captured bytes for the
// marker before trusting a fresh corpus.
type script struct {
	// IdleTimeout and MaxDuration are optional per-scenario overrides for the
	// recorder's --idle-timeout and --max-duration flags, as Go duration
	// strings. The budget belongs with the scenario and versioned in the same
	// file: a tool-call turn needs a far longer idle tolerance than a
	// "what is 2+2?" turn, and rebake-corpus-all cannot pass per-scenario
	// flags at all. Empty leaves the flag value in force.
	IdleTimeout string `json:"idle_timeout,omitempty"`
	MaxDuration string `json:"max_duration,omitempty"`

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

	// Interrupt, when true, writes the harness's interrupt key to its PTY.
	// Exists as its own step kind because JSON cannot reasonably embed a raw
	// control byte in a `send` string.
	Interrupt bool `json:"interrupt,omitempty"`

	// InterruptKey selects WHICH key an Interrupt step writes: "ctrl-c"
	// (0x03, the default and what every recording before 2026-08-29 used) or
	// "esc" (0x1b).
	//
	// Which one actually interrupts is harness- and version-specific, and it
	// moved. Measured on Claude Code 2.1.251, 2026-08-29, by recording the
	// same scripted turn twice and reading the captured bytes:
	//
	//   esc    → the turn stops and the harness paints
	//            "⎿  Interrupted · What should Claude do instead?"
	//   ctrl-c → the turn stops, NOTHING is painted, and the prompt is
	//            restored into the composer — a CLEAR, not an interrupt.
	//
	// Ctrl-C is Claude Code's exit/clear key; Esc is its documented in-TUI
	// interrupt. The default stays "ctrl-c" so the codex interrupted-mid-reply
	// corpus, baked with 0x03, keeps recording identically; the claude script
	// asks for "esc" explicitly.
	//
	// WHEN the key lands matters as much as which key it is. An Esc that
	// arrives before the first token cancels the request outright and is
	// painted the same as a clear — nothing on screen, prompt back in the
	// composer. The busy footer ("esc to interrupt") appears while the model
	// is still thinking, so it is not on its own enough to dwell on: wait for
	// the reply to start streaming (the U+23FA bullet) as well, then sleep,
	// then interrupt. That is what test/scripts/claude/interrupted-mid-reply
	// does, and a bake without it captures an empty screen.
	InterruptKey string `json:"interrupt_key,omitempty"`
}

// interruptBytes returns the byte sequence an Interrupt step writes.
func (s scriptStep) interruptBytes() []byte {
	if s.InterruptKey == "esc" {
		return []byte{0x1b}
	}
	return []byte{0x03}
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
	if s.IdleTimeout != "" {
		if _, err := time.ParseDuration(s.IdleTimeout); err != nil {
			return nil, fmt.Errorf("script %s: invalid idle_timeout: %w", path, err)
		}
	}
	if s.MaxDuration != "" {
		if _, err := time.ParseDuration(s.MaxDuration); err != nil {
			return nil, fmt.Errorf("script %s: invalid max_duration: %w", path, err)
		}
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
	if step.InterruptKey != "" {
		if !step.Interrupt {
			return errors.New("interrupt_key set on a step that is not an interrupt step")
		}
		if step.InterruptKey != "ctrl-c" && step.InterruptKey != "esc" {
			return fmt.Errorf("invalid interrupt_key %q: want ctrl-c or esc", step.InterruptKey)
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

	// echoGap bounds the composer-echo wait in send. Zero means submitEchoGap;
	// tests shrink it so they do not pay the real bound on the degrade path.
	echoGap time.Duration

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
		return d.send(ctx, step.Send)
	case step.Interrupt:
		_, err := d.stdin.WriteStdin(step.interruptBytes())
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
//
// The body and the submit key are two separate writes with a bounded wait for
// the composer echo between them (echo.go), mirroring pkg/chat's
// writeMessageAndSubmit: a submit key riding in the same burst as the text is
// consumed as pasted content and the prompt is never sent. The echo wait is
// best-effort — on timeout the submit key is written anyway, degrading to the
// old timing — so this can delay a send but never drop or duplicate one.
//
// Steps with no trailing "\n", and every step when submitKey is nil, keep the
// verbatim single raw write. That is what lets the explicit split-step scripts
// (send body, sleep, send "\x1b[13u") and the codex bakes record unchanged.
func (d *scriptDriver) send(ctx context.Context, s string) error {
	if d.submitKey != nil && strings.HasSuffix(s, "\n") {
		if body := strings.TrimSuffix(s, "\n"); body != "" {
			if _, err := d.stdin.WriteStdin([]byte(body)); err != nil {
				return err
			}
			d.awaitComposerEcho(ctx, echoNeedle(body))
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

// Bracketed-paste framing (pkg/chat.pasteWrapForHarness — CSI 200 ~ … CSI 201 ~
// around a payload of pasteThreshold bytes or more) deliberately has NO mirror
// here, for the same reason as Shift+Tab below. Production frames a payload only
// at >=1KB; every recorded scenario's Send is a short line, so a mirror would be
// dead code that no corpus exercises and that could silently drift. If a
// scenario is ever recorded with a large prompt — which is exactly what a
// re-baked corpus for the truncation defect would need — mirror the framing here
// FIRST, or the recorded bytes stop matching what the wrapper actually writes.

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
