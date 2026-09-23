package wrapper_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Keep-alive supervision (Config.KeepAliveOnClassification, ADR-006), driven
// by scripted harnesses: /bin/sh prints what a harness would and then idles in
// sleep, which is the reproduction of the idle-kill — a quiet harness whose
// last output merely MENTIONS a rate limit was SIGTERMed as blocked_by_cost.

const (
	kaQuiet    = 50 * time.Millisecond
	kaClassify = 200 * time.Millisecond
	// kaOutlive is how long a keep-alive harness must stay up: three
	// classify windows, well past the moment the default mode kills.
	kaOutlive = 3 * kaClassify
)

// banner is Claude Code's session-limit wall, as the wrapper's anchored
// matcher reads it.
const banner = "  ⎿  You've hit your session limit · resets 6:40pm (UTC)"

// scriptSession starts /bin/sh running script, as the claude harness, with
// the idle thresholds shrunk.
func scriptSession(t *testing.T, script string, keepAlive bool) *wrapper.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	sess, err := wrapper.Start(ctx, wrapper.Config{
		BinaryPath:                "/bin/sh",
		Args:                      []string{"-c", script},
		Stdout:                    io.Discard,
		Harness:                   "claude",
		IdleQuiet:                 kaQuiet,
		IdleClassify:              kaClassify,
		KeepAliveOnClassification: keepAlive,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = sess.Stop(stopCtx)
	})
	return sess
}

// printLines returns a shell fragment that prints each line from a file, so no
// quoting in the text can reach the shell.
func printLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "out.txt")
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "cat '" + p + "'"
}

// events collects a session's events as they arrive.
type events struct {
	mu  sync.Mutex
	evs []wrapper.SessionEvent
}

func collect(sess *wrapper.Session) *events {
	e := &events{}
	go func() {
		for ev := range sess.Events() {
			e.mu.Lock()
			e.evs = append(e.evs, ev)
			e.mu.Unlock()
		}
	}()
	return e
}

// statuses returns the Status of every non-terminal event so far.
func (e *events) statuses() []wrapper.Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []wrapper.Status
	for _, ev := range e.evs {
		if !ev.Terminated {
			out = append(out, ev.Status)
		}
	}
	return out
}

func (e *events) first(status wrapper.Status) (wrapper.SessionEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.evs {
		if ev.Status == status && !ev.Terminated {
			return ev, true
		}
	}
	return wrapper.SessionEvent{}, false
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive reports whether the session is still running.
func alive(sess *wrapper.Session) bool {
	done := make(chan struct{})
	go func() {
		_, _ = sess.Wait()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(20 * time.Millisecond):
		return true
	}
}

// TestKeepAlive_DefaultModeStillEndsTheRun pins today's run-to-completion
// behaviour, which the zero value keeps: a reply about rate limiting, then
// silence, ends in blocked_by_cost and a SIGTERM.
func TestKeepAlive_DefaultModeStillEndsTheRun(t *testing.T) {
	sess := scriptSession(t, printLines(t, "I added rate limiting to the login endpoint.")+"; exec sleep 30", false)
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != wrapper.StatusBlockedByCost || res.Signal == "" {
		t.Fatalf("Result = %+v, want blocked_by_cost after a signal", res)
	}
}

// TestKeepAlive_QuietHarnessOutlivesThePhrase: the same script under
// keep-alive is still running three classify windows later, with no verdict.
func TestKeepAlive_QuietHarnessOutlivesThePhrase(t *testing.T) {
	sess := scriptSession(t, printLines(t, "I added rate limiting to the login endpoint.")+"; exec sleep 30", true)
	evs := collect(sess)
	time.Sleep(kaOutlive)
	if !alive(sess) {
		res, _ := sess.Wait()
		t.Fatalf("keep-alive harness ended: %+v", res)
	}
	if got := evs.statuses(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
	if st := sess.Snapshot().Status; st != "" {
		t.Fatalf("Snapshot().Status = %q, want empty", st)
	}
}

// TestKeepAlive_BannerIsReportedNotEnforced: a real wall is reported once, as
// a non-terminal event carrying its reset time, and the process lives on until
// the caller stops it.
func TestKeepAlive_BannerIsReportedNotEnforced(t *testing.T) {
	sess := scriptSession(t, printLines(t, banner)+"; exec sleep 30", true)
	evs := collect(sess)
	var ev wrapper.SessionEvent
	eventually(t, "the banner's verdict", func() (ok bool) {
		ev, ok = evs.first(wrapper.StatusBlockedByCost)
		return ok
	})
	if ev.ResumeAt.IsZero() || ev.Class != wrapper.ErrRateLimited {
		t.Fatalf("event = %+v, want the reset time and ErrRateLimited", ev)
	}
	time.Sleep(kaOutlive)
	if !alive(sess) {
		t.Fatal("a reported wall ended the harness")
	}
	if got := evs.statuses(); len(got) != 1 {
		t.Fatalf("events = %v, want exactly the one verdict", got)
	}
	if st := sess.Snapshot().Status; st != wrapper.StatusBlockedByCost {
		t.Fatalf("Snapshot().Status = %q, want the standing verdict", st)
	}

	// Stop after a verdict ends interrupted.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	res, _ := sess.Wait()
	if res.Status != wrapper.StatusInterrupted {
		t.Fatalf("Result.Status = %q, want interrupted", res.Status)
	}
}

// TestKeepAlive_LaterOutputConsumesTheVerdict: once the harness writes past a
// verdict's evidence the status clears — without an event — and neither the
// new output nor the silence after it brings the old verdict back.
func TestKeepAlive_LaterOutputConsumesTheVerdict(t *testing.T) {
	sess := scriptSession(t, printLines(t, banner)+"; sleep 0.4; "+printLines(t, "Done. The tests pass.")+"; exec sleep 30", true)
	evs := collect(sess)
	eventually(t, "the banner's verdict", func() bool {
		_, ok := evs.first(wrapper.StatusBlockedByCost)
		return ok
	})
	eventually(t, "the status to clear", func() bool { return sess.Snapshot().Status == "" })
	time.Sleep(kaOutlive)
	if got := evs.statuses(); len(got) != 1 {
		t.Fatalf("events = %v, want only the first verdict", got)
	}
	if st := sess.Snapshot().Status; st != "" {
		t.Fatalf("Snapshot().Status = %q after quiet, want empty", st)
	}
}

// TestKeepAlive_RepeatedBannerIsReportedAgain: the same wall painted again,
// after other output, is new evidence and a second verdict.
func TestKeepAlive_RepeatedBannerIsReportedAgain(t *testing.T) {
	sess := scriptSession(t, printLines(t, banner)+"; sleep 0.4; "+printLines(t, "Retrying later.")+"; sleep 0.4; "+printLines(t, banner)+"; exec sleep 30", true)
	evs := collect(sess)
	eventually(t, "two verdicts", func() bool {
		n := 0
		for _, st := range evs.statuses() {
			if st == wrapper.StatusBlockedByCost {
				n++
			}
		}
		return n == 2
	})
}

// TestKeepAlive_OldAPIErrorDoesNotMaskANewBanner: the API-error matcher runs
// first, so a whole-window scan kept answering with an old API Error line and
// never reached a later session-limit banner. A verdict consumes its evidence,
// so the banner is read on its own.
func TestKeepAlive_OldAPIErrorDoesNotMaskANewBanner(t *testing.T) {
	sess := scriptSession(t, printLines(t, "API Error: 529 Overloaded")+"; sleep 0.4; "+printLines(t, banner)+"; exec sleep 30", true)
	evs := collect(sess)
	eventually(t, "the banner's verdict after the API error's", func() bool {
		_, ok := evs.first(wrapper.StatusBlockedByCost)
		return ok
	})
	api, ok := evs.first(wrapper.StatusAPIError)
	if !ok || api.HTTPCode != 529 {
		t.Fatalf("events = %v, want the API error first", evs.statuses())
	}
}

// TestKeepAlive_AdversarialRowsNeverClassify: ordinary coding-agent output
// that happens to contain a phrase-arm phrase. The default mode ends each run
// on it after the classify window; keep-alive reads nothing into it.
func TestKeepAlive_AdversarialRowsNeverClassify(t *testing.T) {
	rows := []struct {
		name, line string
	}{
		{"assistant prose about rate limiting", "⏺ I added rate limiting to the login endpoint."},
		{"a diff adding a retry message", `+	return errors.New("Something went wrong, please try again")`},
		{"a test log with a fetch failure", "--- FAIL: TestLoad: TypeError: fetch failed"},
		{"a counter that resets at midnight", "⏺ The counter resets at midnight."},
		{"prose about usage limits (chat's not-a-wall row)", "⏺ Your plan's usage limit resets nightly; the session limit is separate."},
		{"a reply quoting the wall mid-sentence (chat's not-a-wall row)", "⏺ The docs say that when you've hit your session limit the CLI prints a banner."},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			script := printLines(t, row.line) + "; exec sleep 30"

			def := scriptSession(t, script, false)
			res, err := def.Wait()
			if err != nil {
				t.Fatalf("default Wait: %v", err)
			}
			if res.Status != wrapper.StatusBlockedByCost && res.Status != wrapper.StatusRetryLater {
				t.Fatalf("default mode Result = %+v, want the terminal verdict it has always reached", res)
			}

			ka := scriptSession(t, script, true)
			evs := collect(ka)
			time.Sleep(kaOutlive)
			if !alive(ka) {
				t.Fatal("keep-alive harness ended")
			}
			if got := evs.statuses(); len(got) != 0 {
				t.Fatalf("keep-alive events = %v, want none", got)
			}
		})
	}
}

// TestConfig_NegativeIdleThresholdsRefused: a negative IdleClassify made every
// tick idle, so any phrase killed at once; it is refused in every mode, as is
// a negative IdleQuiet.
func TestConfig_NegativeIdleThresholdsRefused(t *testing.T) {
	for _, keepAlive := range []bool{false, true} {
		for _, cfg := range []wrapper.Config{
			{IdleQuiet: -1},
			{IdleClassify: -1},
		} {
			cfg.BinaryPath = "/bin/sh"
			cfg.Stdout = io.Discard
			cfg.KeepAliveOnClassification = keepAlive
			if _, err := wrapper.Start(context.Background(), cfg); !errors.Is(err, wrapper.ErrInvalidConfig) {
				t.Errorf("Start(IdleQuiet=%v, IdleClassify=%v, keepAlive=%v) = %v, want ErrInvalidConfig",
					cfg.IdleQuiet, cfg.IdleClassify, keepAlive, err)
			}
		}
	}
}

// TestKeepAlive_ResultIsTheExits: no verdict ends a keep-alive run, so
// Result.Status is always the exit's; a failed exit's Class says why only when
// the evidence still stands at the end — the harness wrote nothing after the
// verdict, or the exit pass finds it in what came after the last one.
func TestKeepAlive_ResultIsTheExits(t *testing.T) {
	cases := []struct {
		name      string
		script    func(t *testing.T) string
		wantClass wrapper.ErrorClass
	}{
		{
			name:      "a verdict with nothing after it explains the exit",
			script:    func(t *testing.T) string { return printLines(t, banner) + "; sleep 0.4; exit 3" },
			wantClass: wrapper.ErrRateLimited,
		},
		{
			name: "a verdict the harness moved past does not",
			script: func(t *testing.T) string {
				return printLines(t, banner) + "; sleep 0.4; " + printLines(t, "Done.") + "; sleep 0.4; exit 3"
			},
			wantClass: wrapper.ErrNone,
		},
		{
			name:      "the exit pass explains a fast failure no mid-run pass saw",
			script:    func(t *testing.T) string { return printLines(t, "API Error: 529 Overloaded") + "; exit 3" },
			wantClass: wrapper.ClassifyOutput("claude", "API Error: 529 Overloaded").Class,
		},
		{
			name:      "prose is no explanation",
			script:    func(t *testing.T) string { return printLines(t, "I added rate limiting.") + "; exit 3" },
			wantClass: wrapper.ErrNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := scriptSession(t, tc.script(t), true).Wait()
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if res.Status != wrapper.StatusFailed || res.ExitCode != 3 {
				t.Fatalf("Result = %+v, want the exit's own failed status", res)
			}
			if res.Class != tc.wantClass {
				t.Fatalf("Result.Class = %q, want %q", res.Class, tc.wantClass)
			}
		})
	}
}
