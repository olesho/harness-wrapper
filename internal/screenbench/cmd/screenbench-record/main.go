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
//     --harness gemini \
//     --bin /usr/local/bin/gemini \
//     --out test/corpus/gemini/short-reply \
//     --auto-version \
//     --script test/scripts/gemini/short-reply.json
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
	fs.StringVar(&c.Harness, "harness", "", "harness name (e.g. codex, claude-code, gemini); recorded in meta.json")
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

	binaryVersion := c.BinaryVersion
	if c.AutoVersion {
		v, err := captureBinaryVersion(c.Bin)
		if err != nil {
			return fmt.Errorf("auto-version: %w", err)
		}
		binaryVersion = v
	}

	var scr *script
	if c.ScriptPath != "" {
		scr, err = loadScript(c.ScriptPath)
		if err != nil {
			return err
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if c.MaxDuration > 0 {
		var capCancel context.CancelFunc
		ctx, capCancel = context.WithTimeout(ctx, c.MaxDuration)
		defer capCancel()
	}

	env := append(os.Environ(),
		fmt.Sprintf("COLUMNS=%d", c.Cols),
		fmt.Sprintf("LINES=%d", c.Rows),
	)

	stdout := c.Stdout
	if stdout == nil {
		if scr == nil {
			stdout = os.Stdout
		} else {
			// Scripted mode: discard live PTY output so a CI run isn't
			// flooded with ANSI escape codes. bytes.raw still captures
			// everything via the AttachOutput fan-out below.
			stdout = io.Discard
		}
	}
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
	detachRaw := sess.AttachOutput(rawFile)
	defer detachRaw()

	var (
		driverErr  error
		driverDone chan struct{}
	)
	if scr != nil {
		driver := newScriptDriver(sess, c.IdleTimeout, 0)
		driver.submitKey = submitKeyForHarness(c.Harness)
		detachDriver := sess.AttachOutput(driver)
		defer detachDriver()

		driverDone = make(chan struct{})
		go func() {
			defer close(driverDone)
			driverErr = driver.Run(ctx, scr)
			// Script finished — give the harness a grace window to
			// emit any final output, then trigger shutdown.
			time.Sleep(c.IdleTimeout)
			_ = sess.Stop(context.Background())
		}()
	}

	res, _ := sess.Wait()
	if driverDone != nil {
		// Synchronize driverErr write with our read.
		<-driverDone
	}

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
	if scr == nil {
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
// "@google/gemini-cli 0.42.0", "Claude Code 2.1.141"); the first-line
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
