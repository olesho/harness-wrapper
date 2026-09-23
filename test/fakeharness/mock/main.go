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
//	login         claude-style sign-in: a URL, then a code prompt; the right code stores a login in $MOCK_CONFIG_DIR
//	login-device  codex-style device sign-in: a URL and a one-time code, then a login stored after a moment
//	login-status  prints {"loggedIn": ...}: whether $MOCK_CONFIG_DIR holds a login or MOCK_AUTH_TOKEN is set
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
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	mode := flag.String("mode", "completed", "completed|failed|stuck|needs-input|trust|emit|cost-limited|api-error|login|login-device|login-status")
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
	// chat's claude-code and pi adapters start every fresh session under an
	// assigned id (--session-id <uuid>). Accepted, as those harnesses accept it,
	// so the mock can stand in for one; it names no transcript here.
	_ = flag.String("session-id", "", "the session id a claude-code or pi launch assigns (accepted, unused)")
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
	case "login":
		runLogin()
	case "login-device":
		runLoginDevice()
	case "login-status":
		runLoginStatus()
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

	// Paint the whole dialog in ONE write. A real TUI emits its dialog as a
	// single screen update, and the watcher surfaces an input request from the
	// first frame that matches — with no settle window (see
	// Conversation.handleInputRequested). Printing the menu line-by-line left a
	// window in which the emulated screen held the question and option 1 but not
	// yet option 2, so the request surfaced carrying ONE option. Locally the
	// writes coalesce and it passes; on a loaded CI runner the reader lands in
	// that window and `TestSSE_TrustDialogSurfacedAndAnswered` fails with
	// "options = [{ID:1 … Yes, proceed}], want 2". That is an artifact of the
	// fixture dribbling its paint, not of the code under test.
	fmt.Print("Do you trust the files in this folder?\n" +
		"\n" +
		"❯ 1. Yes, proceed\n" +
		"  2. No, exit\n")

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

// mockLoginCode is the code the login mode's sign-in page "shows".
const mockLoginCode = "mock-code#mock-state"

// mockLoginURL is the login mode's sign-in page.
const mockLoginURL = "https://login.example.test/oauth/authorize?code=true&state=mock"

// runLogin simulates claude's `auth login`: the sign-in URL as an OSC 8 link
// and as text, written in two pieces, then a code prompt. The right code
// stores a login and pauses for Enter; a wrong one is asked for again, up to
// three tries.
func runLogin() {
	fmt.Print("Opening browser to sign in\r\nIf the browser didn't open, visit: \x1b]8;;" + mockLoginURL[:30])
	time.Sleep(50 * time.Millisecond)
	fmt.Print(mockLoginURL[30:] + "\x07\x1b[94m" + mockLoginURL + "\x1b[39m\x1b]8;;\x07\r\n")
	reader := bufio.NewReader(os.Stdin)
	for range 3 {
		fmt.Print("Paste code here if prompted > ")
		line, err := reader.ReadString('\n')
		if err != nil {
			os.Exit(1)
		}
		if strings.TrimSpace(line) == mockLoginCode {
			storeMockLogin()
			fmt.Print("Login successful. Press Enter to continue\r\n")
			_, _ = reader.ReadString('\n')
			return
		}
		fmt.Print("Invalid code. Please make sure the full code was copied\r\n")
	}
	os.Exit(1)
}

// runLoginDevice simulates codex's `login --device-auth`: a URL and a
// one-time code, then a login the "server" completes after a moment.
func runLoginDevice() {
	fmt.Print("Follow these steps to sign in with ChatGPT using device code authorization:\r\n\r\n" +
		"1. Open this link in your browser and sign in to your account\r\n" +
		"   \x1b[94mhttps://login.example.test/codex/device\x1b[0m\r\n\r\n" +
		"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\r\n" +
		"   \x1b[94mMOCK-C0DE1\x1b[0m\r\n")
	time.Sleep(300 * time.Millisecond)
	storeMockLogin()
	fmt.Print("Successfully logged in\r\n")
}

func runLoginStatus() {
	_, err := os.Stat(filepath.Join(os.Getenv("MOCK_CONFIG_DIR"), ".credentials.json"))
	fmt.Printf("{\"loggedIn\": %v}\n", err == nil || os.Getenv("MOCK_AUTH_TOKEN") != "")
}

func storeMockLogin() {
	path := filepath.Join(os.Getenv("MOCK_CONFIG_DIR"), ".credentials.json")
	if err := os.WriteFile(path, []byte(`{"mock": true}`+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "store login:", err)
		os.Exit(1)
	}
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
