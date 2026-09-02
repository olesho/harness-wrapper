//go:build screenbench

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestEchoNeedle(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"short line kept whole", "hi there", "hi there"},
		{"first line only", "use the Bash tool\nsecond line", "use the Bash tool"},
		{"capped at echoNeedleLen", strings.Repeat("a", 60), strings.Repeat("a", echoNeedleLen)},
		{"trimmed", "  padded  ", "padded"},
		{"empty is unmatchable", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := echoNeedle(tt.text); got != tt.want {
				t.Errorf("echoNeedle(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// TestSend_WaitsForEchoBeforeSubmitting is the whole point of the echo wait:
// the body goes out, and the submit key is withheld until the composer has
// echoed it back. A submit key riding in the same burst as the text is
// consumed as pasted content and the turn never starts — see echo.go.
func TestSend_WaitsForEchoBeforeSubmitting(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, time.Second, 0)
	d.submitKey = []byte("\x1b[13u")
	d.echoGap = 2 * time.Second

	const body = "use the Bash tool to list entries"
	done := make(chan error, 1)
	go func() { done <- d.send(context.Background(), body+"\n") }()

	// Until the echo lands, only the body may have been written.
	deadline := time.After(time.Second)
	for {
		if strings.Contains(stdin.String(), "\x1b[13u") {
			t.Fatal("submit key written before the composer echoed the body")
		}
		if strings.Contains(stdin.String(), body) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("body was never written")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Echo it back the way the harness would: styled, with the text inside.
	_, _ = d.Write([]byte("\x1b[2m❯ \x1b[0m" + body))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return after the echo arrived")
	}

	if got, want := stdin.String(), body+"\x1b[13u"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

// TestSend_DegradesWhenEchoNeverArrives pins the failure mode: a composer that
// never echoes must still get the submit key, once, after roughly the bound.
// Delaying a send is acceptable; dropping it (no turn) or duplicating it (two
// turns) is not.
func TestSend_DegradesWhenEchoNeverArrives(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, time.Second, 0)
	d.submitKey = []byte("\x1b[13u")
	d.echoGap = 150 * time.Millisecond

	start := time.Now()
	if err := d.send(context.Background(), "silent composer\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < d.echoGap {
		t.Errorf("send returned after %s, want at least the echo bound %s", elapsed, d.echoGap)
	}
	if got, want := stdin.String(), "silent composer\x1b[13u"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
	if n := bytes.Count([]byte(stdin.String()), []byte("\x1b[13u")); n != 1 {
		t.Errorf("submit key written %d times, want exactly 1", n)
	}
}

// TestSend_SubmitKeyWrittenExactlyOnce covers both paths at once: whether or
// not the echo is seen, exactly one submit key reaches the PTY.
func TestSend_SubmitKeyWrittenExactlyOnce(t *testing.T) {
	t.Run("echo seen", func(t *testing.T) {
		stdin := &recordingStdin{}
		d := newScriptDriver(stdin, time.Second, 0)
		d.submitKey = []byte("\x1b[13u")
		d.echoGap = time.Second
		// Pre-load the buffer so the echo is already there when send looks.
		_, _ = d.Write([]byte("echo me back"))
		if err := d.send(context.Background(), "echo me back\n"); err != nil {
			t.Fatal(err)
		}
		if n := bytes.Count([]byte(stdin.String()), []byte("\x1b[13u")); n != 1 {
			t.Errorf("submit key written %d times, want exactly 1", n)
		}
	})
	t.Run("echo missed", func(t *testing.T) {
		stdin := &recordingStdin{}
		d := newScriptDriver(stdin, time.Second, 0)
		d.submitKey = []byte("\x1b[13u")
		d.echoGap = 50 * time.Millisecond
		if err := d.send(context.Background(), "never echoed\n"); err != nil {
			t.Fatal(err)
		}
		if n := bytes.Count([]byte(stdin.String()), []byte("\x1b[13u")); n != 1 {
			t.Errorf("submit key written %d times, want exactly 1", n)
		}
	})
}

// TestSend_BareNewlineSkipsTheWait: a bare "\n" step is the submit key alone
// (settled-after-turn uses it to answer the trust dialog). There is no body to
// echo, so it must not pay the bound.
func TestSend_BareNewlineSkipsTheWait(t *testing.T) {
	stdin := &recordingStdin{}
	d := newScriptDriver(stdin, time.Second, 0)
	d.submitKey = []byte("\x1b[13u")
	d.echoGap = 5 * time.Second

	start := time.Now()
	if err := d.send(context.Background(), "\n"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("bare newline waited %s; it has no body to echo", elapsed)
	}
	if got := stdin.String(); got != "\x1b[13u" {
		t.Errorf("sent %q, want the submit key alone", got)
	}
}

// TestAwaitComposerEcho_CancelledContext: a cancelled run must abandon the wait
// rather than sit out the bound.
func TestAwaitComposerEcho_CancelledContext(t *testing.T) {
	d := newScriptDriver(&recordingStdin{}, time.Second, 0)
	d.echoGap = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if d.awaitComposerEcho(ctx, "anything") {
		t.Error("reported an echo that never arrived")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s after ctx cancellation", elapsed)
	}
}
