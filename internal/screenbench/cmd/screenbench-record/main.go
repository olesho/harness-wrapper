//go:build screenbench

// screenbench-record runs an interactive harness under wrapper
// supervision while capturing the raw PTY byte stream to a corpus
// scenario directory.
//
// Two operating modes:
//
//  1. Interactive (default). User interacts naturally with the TUI;
//     bytes are captured to bytes.raw until the harness exits.
//
//     screenbench-record \
//     --harness codex \
//     --bin /usr/local/bin/codex \
//     --out test/corpus/codex/short-reply \
//     --cols 120 --rows 40 \
//     --notes "single-turn short reply"
//
//  2. Scripted (--script PATH). Unattended: a JSON script of
//     wait_for / send / sleep steps drives the session. Required for
//     CI-friendly corpus refresh. See script.go for the schema.
//
//     screenbench-record \
//     --harness codex \
//     --bin /usr/local/bin/codex \
//     --out test/corpus/codex/short-reply \
//     --auto-version \
//     --script test/scripts/codex/short-reply.json
//
// After the harness exits, populate expected.txt by hand or by copying
// from the harness's session JSONL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper-screenbench/scenario"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "screenbench-record:", err)
		os.Exit(1)
	}
}

// recorderConfig is the parsed flag set.
type recorderConfig struct {
	Harness       string
	Bin           string
	Out           string
	Cols, Rows    int
	BinaryVersion string
	AutoVersion   bool
	Notes         string
	ScriptPath    string
	IdleTimeout   time.Duration
	MaxDuration   time.Duration
	HarnessArgs   []string

	// Stdout overrides the destination for the live PTY output. Set
	// by tests; production main() leaves it nil and run() defaults to
	// os.Stdout in interactive mode or io.Discard in scripted mode.
	Stdout io.Writer
}

func parseFlags(args []string) (recorderConfig, error) {
	fs := flag.NewFlagSet("screenbench-record", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := recorderConfig{}
	fs.StringVar(&c.Harness, "harness", "", "harness name (e.g. codex, claude-code, opencode, pi); recorded in meta.json")
	fs.StringVar(&c.Bin, "bin", "", "path to harness binary (required)")
	fs.StringVar(&c.Out, "out", "", "output scenario directory (required)")
	fs.IntVar(&c.Cols, "cols", 120, "terminal columns (also passed via COLUMNS env)")
	fs.IntVar(&c.Rows, "rows", 40, "terminal rows (also passed via LINES env)")
	fs.StringVar(&c.BinaryVersion, "binary-version", "", "harness version string for meta.json (ignored when --auto-version is set)")
	fs.BoolVar(&c.AutoVersion, "auto-version", false, "shell-out `<bin> --version` and populate meta.binary_version")
	fs.StringVar(&c.Notes, "notes", "", "free-text notes for meta.json")
	fs.StringVar(&c.ScriptPath, "script", "", "path to a JSON script of {wait_for, send, sleep} steps; enables unattended recording")
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", 3*time.Second, "treat N of no PTY output as 'screen settled' during wait_for steps")
	fs.DurationVar(&c.MaxDuration, "max-duration", 5*time.Minute, "hard cap on session length; 0 disables")
	if err := fs.Parse(args); err != nil {
		return recorderConfig{}, err
	}
	if c.Bin == "" || c.Out == "" || c.Harness == "" {
		return recorderConfig{}, errors.New("usage: screenbench-record --harness NAME --bin PATH --out DIR [flags] [-- harness args]")
	}
	c.HarnessArgs = fs.Args()
	return c, nil
}

func run(c recorderConfig) error {
	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return fmt.Errorf("mkdir out: %w", err)
	}
	rawPath := filepath.Join(c.Out, "bytes.raw")
	rawFile, err := os.Create(rawPath)
	if err != nil {
		return fmt.Errorf("create bytes.raw: %w", err)
	}
	defer rawFile.Close()

	binaryVersion, err := resolveBinaryVersion(c)
	if err != nil {
		return err
	}

	var scr *script
	if c.ScriptPath != "" {
		scr, err = loadScript(c.ScriptPath)
		if err != nil {
			return err
		}
		// The scenario's own timing budget wins over the flags, and must be
		// applied BEFORE recordingContext (which bakes in MaxDuration) and
		// launchDriver (which uses IdleTimeout for both the wait_for fallback
		// and its post-script grace sleep — a longer scenario rightly gets a
		// longer grace window).
		applyScriptTiming(&c, scr)
	}

	ctx, cancel := recordingContext(c)
	defer cancel()

	env := append(
		os.Environ(),
		fmt.Sprintf("COLUMNS=%d", c.Cols),
		fmt.Sprintf("LINES=%d", c.Rows),
	)

	stdout := resolveStdout(c, scr != nil)
	wcfg := wrapper.Config{
		BinaryPath: c.Bin,
		Args:       c.HarnessArgs,
		Env:        env,
		Stdout:     stdout,
		Harness:    c.Harness,
	}
	if scr == nil {
		// Interactive mode: forward the developer's keyboard.
		wcfg.Stdin = os.Stdin
	}
	// In scripted mode wcfg.Stdin stays nil; the driver writes via WriteStdin.

	sess, err := wrapper.Start(ctx, wcfg)
	if err != nil {
		return fmt.Errorf("wrapper start: %w", err)
	}
	// Size the PTY to the target geometry. The wrapper only auto-sizes from the
	// controlling terminal when stdin AND stdout are TTYs; in scripted mode both
	// are non-TTYs (stdin nil, stdout io.Discard), so without this the PTY keeps
	// pty.Start's ~0x0 default. A full-screen TUI (codex 0.142's ratatui, claude)
	// renders for the actual winsize, so an unsized PTY yields a degenerate stream
	// that replays blank at --cols×--rows. Mirrors pkg/chat.Open's Resize.
	_ = sess.Resize(uint16(c.Cols), uint16(c.Rows))
	detachRaw := sess.AttachOutput(rawFile)
	defer detachRaw()

	var driver *driverHandle
	if scr != nil {
		var detachDriver func()
		driver, detachDriver = launchDriver(ctx, sess, c, scr)
		defer detachDriver()
	}

	res, _ := sess.Wait()
	var driverErr error
	if driver != nil {
		// Synchronize driverErr write with our read.
		<-driver.done
		driverErr = driver.err
	}

	return finalize(c, rawPath, binaryVersion, res, scr != nil, driverErr)
}

// resolveBinaryVersion returns the meta.binary_version string: the flag value,
// or the shelled-out `<bin> --version` when --auto-version is set.
func resolveBinaryVersion(c recorderConfig) (string, error) {
	if !c.AutoVersion {
		return c.BinaryVersion, nil
	}
	v, err := captureBinaryVersion(c.Bin)
	if err != nil {
		return "", fmt.Errorf("auto-version: %w", err)
	}
	return v, nil
}

// applyScriptTiming overrides the recorder's idle-timeout and max-duration with
// the script's own values when it declares them. Both were validated at load,
// so a parse failure here is impossible and is ignored rather than re-reported.
// The effective values go to stderr so a bake's provenance is visible in its log.
func applyScriptTiming(c *recorderConfig, scr *script) {
	if d, err := time.ParseDuration(scr.IdleTimeout); err == nil && scr.IdleTimeout != "" {
		c.IdleTimeout = d
	}
	if d, err := time.ParseDuration(scr.MaxDuration); err == nil && scr.MaxDuration != "" {
		c.MaxDuration = d
	}
	fmt.Fprintf(os.Stderr, "[screenbench-record] timing: idle-timeout=%s max-duration=%s\n",
		c.IdleTimeout, c.MaxDuration)
}

// recordingContext builds the session context: interrupt/SIGTERM cancellation
// plus an optional hard max-duration cap. The returned func unwinds both.
func recordingContext(c recorderConfig) (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if c.MaxDuration <= 0 {
		return ctx, cancel
	}
	ctx, capCancel := context.WithTimeout(ctx, c.MaxDuration)
	return ctx, func() {
		capCancel()
		cancel()
	}
}

// resolveStdout picks the live PTY output destination: the test override if set,
// os.Stdout in interactive mode, or io.Discard in scripted mode (so a CI run
// isn't flooded with ANSI escape codes; bytes.raw still captures everything).
func resolveStdout(c recorderConfig, scripted bool) io.Writer {
	if c.Stdout != nil {
		return c.Stdout
	}
	if !scripted {
		return os.Stdout
	}
	return io.Discard
}

// driverHandle tracks a running script driver goroutine: done closes when the
// goroutine exits, at which point err holds its Run result.
type driverHandle struct {
	done chan struct{}
	err  error
}

// launchDriver attaches a script driver to the session's output and runs the
// script in a goroutine. The returned detach func must be called to stop the
// output fan-out (mirrors the previous deferred detach in run).
func launchDriver(ctx context.Context, sess *wrapper.Session, c recorderConfig, scr *script) (*driverHandle, func()) {
	driver := newScriptDriver(sess, c.IdleTimeout, 0)
	driver.submitKey = submitKeyForHarness(c.Harness)
	detach := sess.AttachOutput(driver)

	h := &driverHandle{done: make(chan struct{})}
	go func() {
		defer close(h.done)
		h.err = driver.Run(ctx, scr)
		// Script finished — give the harness a grace window to
		// emit any final output, then trigger shutdown.
		time.Sleep(c.IdleTimeout)
		_ = sess.Stop(context.Background())
	}()
	return h, detach
}

// finalize writes meta.json, prints the capture summary, and surfaces a
// non-cancellation script-driver error.
func finalize(c recorderConfig, rawPath, binaryVersion string, res wrapper.Result, scripted bool, driverErr error) error {
	if err := scenario.WriteMeta(c.Out, scenario.Meta{
		Harness:       c.Harness,
		BinaryVersion: binaryVersion,
		RecordedAt:    time.Now().UTC(),
		Cols:          c.Cols,
		Rows:          c.Rows,
		Notes:         c.Notes,
	}); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n[screenbench-record] captured %d bytes to %s (status=%s, exit=%d)\n",
		fileSize(rawPath), rawPath, res.Status, res.ExitCode)
	if !scripted {
		fmt.Fprintf(os.Stderr, "[screenbench-record] populate %s with the ground-truth final assistant text\n",
			filepath.Join(c.Out, "expected.txt"))
	}
	if driverErr != nil && !errors.Is(driverErr, context.Canceled) && !errors.Is(driverErr, context.DeadlineExceeded) {
		return fmt.Errorf("script driver: %w", driverErr)
	}
	return nil
}

// captureBinaryVersion shells out `<bin> --version` and returns the
// first non-empty line of stdout, with trailing whitespace trimmed.
// Most CLIs print a single-line version banner ("codex 0.130.0",
// "opencode 0.4.2", "Claude Code 2.1.141"); the first-line
// rule is robust enough for them.
// versionSemverRE extracts a clean semver from noisy `--version` output like
// "2.1.185 (Claude Code)" or "codex-cli 0.141.0", so meta.binary_version is a
// bare version that lines up with the versions.json pin.
var versionSemverRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-.][0-9A-Za-z.-]+)?`)

func captureBinaryVersion(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", err
	}
	if v := versionSemverRE.FindString(string(out)); v != "" {
		return v, nil
	}
	// No semver matched — fall back to the first non-empty line verbatim.
	for line := range strings.SplitSeq(string(out), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t, nil
		}
	}
	return "", errors.New("empty --version output")
}

func fileSize(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.Size()
}
