package wrapper

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

// slowReader delays every read, standing in for an output goroutine that the
// scheduler has not run yet when the harness exits.
type slowReader struct {
	r     io.Reader
	delay time.Duration
}

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.r.Read(p)
}

// TestFinalOutputIsReadBeforeThePTYCloses: a harness that prints its error and
// exits at once must still have that error classified. The supervisor used to
// close the PTY master as soon as the process was reaped, discarding whatever
// the output goroutine had not read yet — which on a fast failure is the very
// line the exit classifier needs, so a transport error came back as a plain
// failure. Seen in CI as TestRun_FailedExitTransportUpgradesToRetryLater and
// TestRun_CostLimitedModeReportsBlockedByCost failing under load on Linux; the
// slow reader makes the interleaving certain instead of rare.
func TestFinalOutputIsReadBeforeThePTYCloses(t *testing.T) {
	orig := ptyOutputReader
	ptyOutputReader = func(ptmx io.Reader) io.Reader { return slowReader{r: ptmx, delay: 80 * time.Millisecond} }
	defer func() { ptyOutputReader = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Run(ctx, Config{
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", `printf 'Error: connect ECONNREFUSED 127.0.0.1:443\n'; exit 7`},
		Harness:    "claude",
		Stdout:     io.Discard,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusRetryLater {
		t.Fatalf("Status = %q (reason %q), want %q: the harness's last line was lost before the exit classifier read it", res.Status, res.Reason, StatusRetryLater)
	}
}

// traceLog records trace events for assertions.
type traceLog struct {
	mu     sync.Mutex
	events []trace.Event
}

func (l *traceLog) Emit(e trace.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *traceLog) fields(t *testing.T, kind string) map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.Kind == kind {
			return e.Fields
		}
	}
	t.Fatalf("no %s trace event", kind)
	return nil
}

// TestExitedHarnessOutputIsReadToItsEnd: once the harness and its process
// group are gone nothing holds the terminal open, so the output goroutine
// reaches the end of the output by itself and the supervisor never falls back
// on its drain budget. A platform whose master read did not report the end
// would make every Wait cost the whole budget.
func TestExitedHarnessOutputIsReadToItsEnd(t *testing.T) {
	var (
		log   traceLog
		mu    sync.Mutex
		lines []string
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Run(ctx, Config{
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", `printf 'the last line\n'`},
		Stdout:     io.Discard,
		Trace:      &log,
		OnLine: func(line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, line)
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := log.fields(t, "pty_closed")["output_drained"]; got != true {
		t.Fatalf("pty_closed output_drained = %v, want true: the output never reached its end after the harness exited", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "the last line") }) {
		t.Fatalf("lines = %q, want the harness's last line", lines)
	}
}
