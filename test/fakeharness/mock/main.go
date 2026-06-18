// Mock CLI harness for harness-wrapper tests. It behaves like an
// interactive agent CLI: prints a banner, performs a configurable
// behavior selected by --mode, and exits with a predictable code.
//
// Modes:
//
//	completed     prints progress lines and DONE, exits 0
//	failed        prints an error to stderr, exits with --exit-code
//	stuck         prints one line then blocks forever (until SIGTERM)
//	needs-input   prints a prompt, reads a line from stdin, exits 0 if it matches --expected-input
//	cost-limited  prints a quota-exhausted message, exits with --exit-code
//	api-error     prints --api-error-msg (optionally --api-error-repeat times) then either heartbeats until signal or, if --api-error-recover, continues to completed-style progress and exits 0
//
// This binary has no external dependencies on a particular consumer.
// It's a standalone fake harness invoked as a subprocess by tests
// under pkg/wrapper.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	mode := flag.String("mode", "completed", "completed|failed|stuck|needs-input|trust|emit|cost-limited|api-error")
	delay := flag.Duration("delay", 50*time.Millisecond, "delay between progress lines")
	exitCode := flag.Int("exit-code", 1, "exit code for failed and cost-limited modes")
	steps := flag.Int("steps", 3, "progress steps for completed mode")
	prompt := flag.String("prompt", "Continue? [y/N] ", "prompt text for needs-input mode")
	expected := flag.String("expected-input", "y", "accepted input for needs-input mode")
	apiErrorMsg := flag.String("api-error-msg", "API Error: 529 Overloaded.", "message to print for api-error mode")
	apiErrorRepeat := flag.Int("api-error-repeat", 1, "print the api-error message this many times")
	apiErrorRepeatGap := flag.Duration("api-error-repeat-gap", 100*time.Millisecond, "delay between repeated api-error prints")
	apiErrorRecover := flag.Bool("api-error-recover", false, "after printing, resume normal completed-style progress and exit 0 (else heartbeat until signal)")
	apiErrorHeartbeat := flag.Duration("api-error-heartbeat", 200*time.Millisecond, "heartbeat interval for api-error mode")
	readyPrompt := flag.Bool("ready-prompt", false, "emit a Claude Code-style ready prompt and consume one input line before running the mode (so chat-layer readiness passes); composes with modes that don't otherwise read stdin")
	failedMsg := flag.String("failed-msg", "Fatal: workspace is not writable.", "stderr message for failed mode")
	emitFile := flag.String("emit-file", "", "for --mode emit: path to a file whose bytes are written verbatim to stdout")
	flag.Parse()

	installSignalCleanup()

	fmt.Println("Mock Agent CLI")

	if *readyPrompt {
		// Emulate Claude Code reaching its interactive prompt and the user
		// submitting one message. This lets the chat layer's readiness gate
		// (which looks for "Claude Code" + the "❯" prompt) pass before the
		// selected mode produces its mid-turn behavior.
		fmt.Println("Claude Code")
		fmt.Println("❯")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}

	switch *mode {
	case "completed":
		runCompleted(*steps, *delay)
	case "failed":
		fmt.Fprintln(os.Stderr, *failedMsg)
		os.Exit(*exitCode)
	case "stuck":
		fmt.Println("Thinking...")
		select {}
	case "needs-input":
		runNeedsInput(*prompt, *expected)
	case "trust":
		runTrust()
	case "emit":
		runEmit(*emitFile)
	case "cost-limited":
		fmt.Fprintln(os.Stderr, "ERROR: quota exceeded. Please try again after your usage limit resets.")
		os.Exit(*exitCode)
	case "api-error":
		runAPIError(*apiErrorMsg, *apiErrorRepeat, *apiErrorRepeatGap, *apiErrorRecover, *apiErrorHeartbeat, *steps, *delay)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
}

func runCompleted(steps int, delay time.Duration) {
	for i := 1; i <= steps; i++ {
		fmt.Printf("Step %d/%d\n", i, steps)
		time.Sleep(delay)
	}
	fmt.Println("DONE")
}

func runNeedsInput(prompt, expected string) {
	fmt.Println("Need approval to continue.")
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if line == expected {
		fmt.Println("Approved. DONE")
		return
	}
	fmt.Fprintln(os.Stderr, "Rejected.")
	os.Exit(2)
}

// runTrust simulates Claude Code's startup folder-trust dialog: it renders the
// trust prompt as a numbered menu, waits for the choice digit, clears the
// screen, then behaves like a normal interactive turn (read prompt, reply,
// print a thinking-summary so the turn completes). Choosing anything but "1"
// exits non-zero, like declining trust.
func runTrust() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Do you trust the files in this folder?")
	fmt.Println()
	fmt.Println("❯ 1. Yes, proceed")
	fmt.Println("  2. No, exit")

	choice, _ := reader.ReadString('\n')
	if strings.TrimRight(choice, "\r\n") != "1" {
		fmt.Fprintln(os.Stderr, "Trust declined.")
		os.Exit(2)
	}

	// Clear the screen so the dialog text no longer lingers in the emulated
	// snapshot — this is what lets the watcher observe the prompt resolving.
	fmt.Print("\x1b[2J\x1b[H")
	fmt.Println("Claude Code")
	fmt.Println("❯")

	line, _ := reader.ReadString('\n')
	fmt.Printf("assistant reply: %s\n", strings.TrimRight(line, "\r\n"))
	fmt.Println("✻ Baked for 1s")
	time.Sleep(200 * time.Millisecond)
}

func runAPIError(msg string, repeat int, repeatGap time.Duration, recover bool, heartbeat time.Duration, steps int, delay time.Duration) {
	if repeat < 1 {
		repeat = 1
	}
	for i := 0; i < repeat; i++ {
		if i > 0 {
			time.Sleep(repeatGap)
		}
		fmt.Println(msg)
	}
	if recover {
		// Brief pause, then resume normal output so callers can verify
		// the wrapper's StatusAPIError did not contaminate the
		// terminal Result when output continues.
		time.Sleep(500 * time.Millisecond)
		runCompleted(steps, delay)
		return
	}
	// Heartbeat: keep PTY active without producing recognizable
	// content. A bare dot per tick is enough to refresh lastOutput.
	for {
		time.Sleep(heartbeat)
		fmt.Print(".")
	}
}

func runEmit(path string) {
	if path == "" {
		fmt.Fprintln(os.Stderr, "emit mode requires --emit-file")
		os.Exit(2)
	}
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path from the test itself
	if err != nil {
		fmt.Fprintf(os.Stderr, "emit: %v\n", err)
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(data)
}

func installSignalCleanup() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-ch
		fmt.Println("Mock interrupted.")
		os.Exit(130)
	}()
}
