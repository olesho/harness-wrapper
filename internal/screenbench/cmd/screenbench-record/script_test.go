//go:build screenbench

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- script loader unit tests ---

func writeScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "script.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadScriptValid(t *testing.T) {
	path := writeScript(t, `{
		"steps": [
			{"wait_for": "> "},
			{"send": "hi\n"},
			{"sleep": "100ms"}
		]
	}`)
	s, err := loadScript(path)
	if err != nil {
		t.Fatalf("loadScript: %v", err)
	}
	if len(s.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(s.Steps))
	}
}

func TestLoadScriptEmptySteps(t *testing.T) {
	path := writeScript(t, `{"steps": []}`)
	if _, err := loadScript(path); err == nil {
		t.Error("expected error for empty steps")
	}
}

func TestLoadScriptStepWithoutField(t *testing.T) {
	path := writeScript(t, `{"steps": [{}]}`)
	if _, err := loadScript(path); err == nil {
		t.Error("expected error for empty step")
	}
}

func TestLoadScriptStepWithMultipleFields(t *testing.T) {
	path := writeScript(t, `{"steps": [{"wait_for": "x", "send": "y"}]}`)
	if _, err := loadScript(path); err == nil {
		t.Error("expected error for ambiguous step")
	}
}

func TestLoadScriptInvalidRegex(t *testing.T) {
	path := writeScript(t, `{"steps": [{"wait_for": "["}]}`)
	if _, err := loadScript(path); err == nil {
		t.Error("expected error for malformed regex")
	}
}

func TestLoadScriptInvalidSleep(t *testing.T) {
	path := writeScript(t, `{"steps": [{"sleep": "ten"}]}`)
	if _, err := loadScript(path); err == nil {
		t.Error("expected error for unparseable sleep")
	}
}

// --- driver unit tests (no real PTY) ---

// recordingStdin implements stdinWriter and remembers every byte it received.
type recordingStdin struct {
	mu   sync.Mutex
	sent []byte
}

func (r *recordingStdin) WriteStdin(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, p...)
	return len(p), nil
}

func (r *recordingStdin) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.sent)
}

// TestDriver_SubmitKeyTranslation pins the inward submit-key contract for the
// recorder: with a submitKey set, a Send step's trailing "\n" is replaced by
// the enhanced Enter (so the turn submits on enhanced-keyboard harnesses),
// while the typed body is preserved and a Send with no trailing newline is
// passed through untouched. Nil submitKey keeps the legacy raw behavior.
func TestDriver_SubmitKeyTranslation(t *testing.T) {
	if got := submitKeyForHarness("claude-code"); string(got) != "\x1b[13u" {
		t.Errorf("claude-code submit key = %q, want CSI 13u", got)
	}
	if got := submitKeyForHarness("codex"); string(got) != "\x1b[13u" {
		t.Errorf("codex submit key = %q, want CSI 13u", got)
	}
	if got := submitKeyForHarness("opencode"); got != nil {
		t.Errorf("opencode submit key = %q, want nil (raw)", got)
	}

	t.Run("translates trailing newline", func(t *testing.T) {
		stdin := &recordingStdin{}
		d := newScriptDriver(stdin, time.Second, 0)
		d.submitKey = []byte("\x1b[13u")
		if err := d.send("what is 2 plus 2?\n"); err != nil {
			t.Fatal(err)
		}
		if got, want := stdin.String(), "what is 2 plus 2?\x1b[13u"; got != want {
			t.Errorf("sent %q, want %q", got, want)
		}
	})

	t.Run("nil submitKey stays raw", func(t *testing.T) {
		stdin := &recordingStdin{}
		d := newScriptDriver(stdin, time.Second, 0)
		if err := d.send("hi\n"); err != nil {
			t.Fatal(err)
		}
		if got := stdin.String(); got != "hi\n" {
			t.Errorf("sent %q, want raw %q", got, "hi\n")
		}
	})

	t.Run("no trailing newline passes through", func(t *testing.T) {
		stdin := &recordingStdin{}
		d := newScriptDriver(stdin, time.Second, 0)
		d.submitKey = []byte("\x1b[13u")
		if err := d.send("partial"); err != nil {
			t.Fatal(err)
		}
		if got := stdin.String(); got != "partial" {
			t.Errorf("sent %q, want %q", got, "partial")
		}
	})
}

// TestDriver_WaitForThenSendThenWaitFor exercises the canonical
// wait-then-send-then-wait sequence by hand-feeding bytes through the
// driver's Write method. No PTY required.
func TestDriver_WaitForThenSendThenWaitFor(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, 250*time.Millisecond, 0)

	s := &script{Steps: []scriptStep{
		{WaitFor: `prompt> `},
		{Send: "hi\n"},
		{WaitFor: `done`},
	}}

	driverDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { driverDone <- d.Run(ctx, s) }()

	// Feed the first prompt.
	if _, err := d.Write([]byte("welcome\nprompt> ")); err != nil {
		t.Fatal(err)
	}

	// Give the driver a beat to process the wait_for match and send.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(stdin.String(), "hi\n") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := stdin.String(); !strings.Contains(got, "hi\n") {
		t.Fatalf("expected send to fire after prompt; stdin so far: %q", got)
	}

	// Feed the completion marker.
	if _, err := d.Write([]byte("doing work...done\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-driverDone:
		if err != nil {
			t.Fatalf("driver.Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("driver did not finish")
	}
}

// TestDriver_WaitForFallsBackToIdleTimeout proves that wait_for
// advances on idle-timeout when the regex never matches. This is the
// safety valve when a scripted scenario's pattern is slightly off.
func TestDriver_WaitForFallsBackToIdleTimeout(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, 150*time.Millisecond, 0)

	s := &script{Steps: []scriptStep{
		{WaitFor: `regex-that-never-matches`},
		{Send: "after-idle\n"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	driverDone := make(chan error, 1)
	go func() { driverDone <- d.Run(ctx, s) }()

	// Emit some bytes so lastOut is non-zero, then go silent.
	_, _ = d.Write([]byte("hello\n"))

	select {
	case err := <-driverDone:
		if err != nil {
			t.Fatalf("driver.Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("driver did not advance after idle-timeout")
	}
	if got := stdin.String(); !strings.Contains(got, "after-idle") {
		t.Errorf("expected post-idle send to fire, got stdin=%q", got)
	}
}

// TestDriver_WaitForMatchesExistingBuffer covers the case where the
// regex target arrived before the wait_for step started — the driver
// must immediately return without waiting for new bytes.
func TestDriver_WaitForMatchesExistingBuffer(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, 5*time.Second, 0)

	// Pre-populate the buffer.
	_, _ = d.Write([]byte("already here: prompt> "))

	s := &script{Steps: []scriptStep{
		{WaitFor: `prompt> `},
		{Send: "ok\n"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx, s); err != nil {
		t.Fatalf("driver.Run: %v", err)
	}
	if got := stdin.String(); !strings.Contains(got, "ok\n") {
		t.Errorf("expected immediate send, got stdin=%q", got)
	}
}

// TestDriver_MultiTurnDoesNotRefireOnStaleBuffer proves that a
// wait_for in a later step only matches new bytes, not the still-
// buffered match from a previous turn. Critical for multi-turn
// scripts where the same end-of-turn marker fires multiple times.
func TestDriver_MultiTurnDoesNotRefireOnStaleBuffer(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, 200*time.Millisecond, 0)

	s := &script{Steps: []scriptStep{
		{WaitFor: `Token usage:`},
		{Send: "next-turn\n"},
		{WaitFor: `Token usage:`},
		{Send: "final\n"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	driverDone := make(chan error, 1)
	go func() { driverDone <- d.Run(ctx, s) }()

	// First turn footer.
	_, _ = d.Write([]byte("...assistant reply...\nToken usage: total=10 input=5 output=5\n"))

	// Wait for the first send.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(stdin.String(), "next-turn") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdin.String(), "next-turn") {
		t.Fatalf("first send did not fire; stdin=%q", stdin.String())
	}

	// At this point the script is waiting for the SECOND Token usage.
	// If the stale-buffer bug regressed, "final\n" would have already
	// been sent (because the first footer is still in the buffer).
	if strings.Contains(stdin.String(), "final") {
		t.Fatalf("second wait_for fired on stale buffer; stdin=%q", stdin.String())
	}

	// Now emit the second turn footer.
	_, _ = d.Write([]byte("...more reply...\nToken usage: total=20 input=10 output=10\n"))

	select {
	case err := <-driverDone:
		if err != nil {
			t.Fatalf("driver.Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("driver did not finish; stdin=%q", stdin.String())
	}
	if !strings.Contains(stdin.String(), "final") {
		t.Errorf("final send did not fire; stdin=%q", stdin.String())
	}
}

// TestDriver_InterruptStep verifies the interrupt step writes a single
// 0x03 byte (Ctrl-C) to stdin. JSON can't reasonably embed a raw 0x03
// in a `send` string, so interrupt is its own step kind.
func TestDriver_InterruptStep(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, time.Second, 0)
	s := &script{Steps: []scriptStep{{Interrupt: true}}}

	if err := d.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stdin.mu.Lock()
	got := stdin.sent
	stdin.mu.Unlock()
	if len(got) != 1 || got[0] != 0x03 {
		t.Errorf("expected single 0x03 byte sent, got % x", got)
	}
}

func TestDriver_SleepStep(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, time.Second, 0)
	s := &script{Steps: []scriptStep{{Sleep: "100ms"}}}

	start := time.Now()
	if err := d.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("expected ~100ms sleep, took %v", elapsed)
	}
}

func TestDriver_ContextCancelStopsRun(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, 10*time.Second, 0)
	s := &script{Steps: []scriptStep{{WaitFor: `never`}}}

	ctx, cancel := context.WithCancel(context.Background())
	driverDone := make(chan error, 1)
	go func() { driverDone <- d.Run(ctx, s) }()

	// Make lastOut non-zero so idle-timeout doesn't short-circuit.
	_, _ = d.Write([]byte("x"))
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-driverDone:
		if err == nil {
			t.Error("expected context error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("driver did not honour ctx cancel")
	}
}

// TestCanonicalScriptsLoad walks test/scripts/{codex,claude}
// and parses every .json with loadScript. Catches typos and bad
// regexes at commit time so a CI cron doesn't trip on them.
func TestCanonicalScriptsLoad(t *testing.T) {
	root := repoScriptsRoot(t)
	for _, harness := range []string{"codex", "claude"} {
		harness := harness
		t.Run(harness, func(t *testing.T) {
			dir := filepath.Join(root, harness)
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read %s: %v", dir, err)
			}
			seen := 0
			for _, e := range entries {
				if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
					continue
				}
				path := filepath.Join(dir, e.Name())
				if _, err := loadScript(path); err != nil {
					t.Errorf("loadScript(%s): %v", path, err)
				}
				seen++
			}
			if seen < 6 {
				t.Errorf("%s: expected ≥6 scenario scripts, found %d", harness, seen)
			}
		})
	}
}

func repoScriptsRoot(t *testing.T) string {
	t.Helper()
	// Walk up looking for a directory that actually contains test/scripts.
	// Stopping at the first go.mod is wrong: screenbench is its own nested
	// module, and the canonical scripts live at the OUTER repo root, above
	// internal/screenbench/go.mod.
	cwd, _ := os.Getwd()
	for range 8 {
		scripts := filepath.Join(cwd, "test", "scripts")
		if fi, err := os.Stat(scripts); err == nil && fi.IsDir() {
			return scripts
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			break
		}
		cwd = parent
	}
	t.Fatal("could not find test/scripts above the working directory")
	return ""
}

// --- parseFlags tests ---

func TestParseFlagsRequiresEssentials(t *testing.T) {
	if _, err := parseFlags(nil); err == nil {
		t.Error("expected error for missing flags")
	}
}

func TestParseFlagsSetsScriptDefaults(t *testing.T) {
	c, err := parseFlags([]string{
		"--harness", "codex",
		"--bin", "/x/codex",
		"--out", "/tmp/out",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleTimeout <= 0 || c.MaxDuration <= 0 {
		t.Errorf("expected defaults for IdleTimeout/MaxDuration, got %+v", c)
	}
	if c.AutoVersion {
		t.Errorf("AutoVersion should default false, got %v", c.AutoVersion)
	}
}

func TestParseFlagsScriptPath(t *testing.T) {
	c, err := parseFlags([]string{
		"--harness", "codex",
		"--bin", "/x/codex",
		"--out", "/tmp/out",
		"--script", "/some/script.json",
		"--auto-version",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ScriptPath != "/some/script.json" {
		t.Errorf("ScriptPath = %q", c.ScriptPath)
	}
	if !c.AutoVersion {
		t.Errorf("AutoVersion should be true")
	}
}
