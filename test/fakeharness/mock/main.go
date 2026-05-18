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
	mode := flag.String("mode", "completed", "completed|failed|stuck|needs-input|cost-limited")
	delay := flag.Duration("delay", 50*time.Millisecond, "delay between progress lines")
	exitCode := flag.Int("exit-code", 1, "exit code for failed and cost-limited modes")
	steps := flag.Int("steps", 3, "progress steps for completed mode")
	prompt := flag.String("prompt", "Continue? [y/N] ", "prompt text for needs-input mode")
	expected := flag.String("expected-input", "y", "accepted input for needs-input mode")
	flag.Parse()

	installSignalCleanup()

	fmt.Println("Mock Agent CLI")

	switch *mode {
	case "completed":
		runCompleted(*steps, *delay)
	case "failed":
		fmt.Fprintln(os.Stderr, "Fatal: workspace is not writable.")
		os.Exit(*exitCode)
	case "stuck":
		fmt.Println("Thinking...")
		select {}
	case "needs-input":
		runNeedsInput(*prompt, *expected)
	case "cost-limited":
		fmt.Fprintln(os.Stderr, "ERROR: quota exceeded. Please try again after your usage limit resets.")
		os.Exit(*exitCode)
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

func installSignalCleanup() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-ch
		fmt.Println("Mock interrupted.")
		os.Exit(130)
	}()
}
