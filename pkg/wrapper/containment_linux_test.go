//go:build linux

package wrapper_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

// lockedBuffer is a concurrency-safe output sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// containedTestSetup requires Landlock ABI 9 (a failure, not a skip, in the
// CI cells that set HW_LANDLOCK_REQUIRE_ABI), registers a profile for the
// mock harness and isolates managed state.
func containedTestSetup(t *testing.T) {
	t.Helper()
	if _, err := landlock.Probe(); err != nil {
		if v := os.Getenv("HW_LANDLOCK_REQUIRE_ABI"); v != "" && v != "0" {
			t.Fatalf("Landlock ABI 9 required: %v", err)
		}
		t.Skipf("Landlock ABI 9 unavailable: %v", err)
	}
	bin, _ := filepath.EvalSymlinks(mockHarnessBin)
	t.Cleanup(contain.RegisterTestProfile(contain.TestProfile{Harness: "mock", ExecDirs: []string{filepath.Dir(bin)}}))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func containedConfig(t *testing.T, out *lockedBuffer, rec *traceRecorder, args ...string) wrapper.Config {
	return wrapper.Config{
		BinaryPath:  mockHarnessBin,
		Args:        args,
		WorkingDir:  t.TempDir(),
		Stdout:      out,
		Trace:       rec,
		Harness:     "mock",
		WaitDelay:   500 * time.Millisecond,
		Containment: &wrapper.Containment{Kind: wrapper.ContainmentLandlock},
	}
}

func TestContainedSessionCompletes(t *testing.T) {
	containedTestSetup(t)
	out, rec := &lockedBuffer{}, &traceRecorder{}
	s, err := wrapper.Start(context.Background(), containedConfig(t, out, rec, "--mode", "completed"))
	if err != nil {
		t.Fatal(err)
	}
	applied := s.Containment()
	if applied == nil || applied.Fingerprint == "" || applied.ABI < 9 {
		t.Fatalf("applied policy = %+v", applied)
	}
	res := waitOrFail(t, s, 30*time.Second)
	if res.Status != wrapper.StatusIdle || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(out.String(), "DONE") {
		t.Fatalf("output lacks DONE: %q", out.String())
	}
	final := s.Containment()
	if final.Supervision.Cleanup == "" {
		t.Fatal("no cleanup outcome after Wait")
	}
	if final.Supervision.Mode == containment.SupervisionCgroup {
		if final.Supervision.Cleanup != "complete" {
			t.Fatalf("supervised cleanup: %s", final.Supervision.Cleanup)
		}
		if _, err := os.Stat(final.Supervision.Cgroup); !os.IsNotExist(err) {
			t.Fatalf("session cgroup left behind: %v", err)
		}
		if _, err := os.Stat(final.State.Home); !os.IsNotExist(err) {
			t.Fatalf("ephemeral state left behind: %v", err)
		}
	} else if !strings.HasPrefix(final.Supervision.Cleanup, "incomplete") {
		t.Fatalf("unsupervised cleanup reported %q", final.Supervision.Cleanup)
	}
	for _, kind := range []string{"containment_applied", "containment_cleanup"} {
		if rec.find(kind) == nil {
			t.Errorf("trace lacks %s: %v", kind, rec.kinds())
		}
	}
	if ev := rec.find("containment_applied"); ev != nil && ev.Fields["fingerprint"] != applied.Fingerprint {
		t.Errorf("trace fingerprint %v, applied %s", ev.Fields["fingerprint"], applied.Fingerprint)
	}
}

// TestContainedSessionInteractive: stdin reaches the harness and the wrapper
// can still resize the PTY, whose master it opened outside the domain.
func TestContainedSessionInteractive(t *testing.T) {
	containedTestSetup(t)
	out, rec := &lockedBuffer{}, &traceRecorder{}
	s, err := wrapper.Start(context.Background(), containedConfig(t, out, rec, "--mode", "needs-input", "--prompt", "Proceed? "))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(132, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "Proceed?") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := s.WriteStdin([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	res := waitOrFail(t, s, 30*time.Second)
	if res.Status != wrapper.StatusIdle {
		t.Fatalf("result = %+v (output %q)", res, out.String())
	}
}

// TestContainedStopEndsSession: Stop on a contained harness ends it, and Wait
// returns only once the session cgroup is empty.
func TestContainedStopEndsSession(t *testing.T) {
	containedTestSetup(t)
	out, rec := &lockedBuffer{}, &traceRecorder{}
	s, err := wrapper.Start(context.Background(), containedConfig(t, out, rec, "--mode", "stuck"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "Thinking") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	res := waitOrFail(t, s, time.Second)
	if res.Status != wrapper.StatusInterrupted {
		t.Fatalf("result = %+v", res)
	}
	if a := s.Containment(); a.Supervision.Mode == containment.SupervisionCgroup {
		if _, err := os.Stat(a.Supervision.Cgroup); !os.IsNotExist(err) {
			t.Fatal("cgroup still present after Wait")
		}
	}
}

// TestContainedContextCancel: cancelling the context interrupts a contained
// harness the way it interrupts an uncontained one.
func TestContainedContextCancel(t *testing.T) {
	containedTestSetup(t)
	out, rec := &lockedBuffer{}, &traceRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := wrapper.Start(ctx, containedConfig(t, out, rec, "--mode", "stuck"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	res := waitOrFail(t, s, 30*time.Second)
	if res.Status != wrapper.StatusInterrupted {
		t.Fatalf("result = %+v", res)
	}
}

// TestContainedAndUncontainedCoexist: contained sessions with distinct
// policies and an uncontained one run in one wrapper process at once.
func TestContainedAndUncontainedCoexist(t *testing.T) {
	containedTestSetup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, rec := &lockedBuffer{}, &traceRecorder{}
			cfg := containedConfig(t, out, rec, "--mode", "completed", "--steps", "5")
			switch i {
			case 3:
				cfg.Containment = nil // the uncontained one
			case 1:
				cfg.Containment.ReadOnly = []string{t.TempDir()}
			}
			s, err := wrapper.Start(context.Background(), cfg)
			if err != nil {
				errs <- err
				return
			}
			res, err := s.Wait()
			if err != nil || res.Status != wrapper.StatusIdle {
				errs <- errors.New("session " + string(rune('0'+i)) + " did not complete: " + string(res.Status))
				return
			}
			if (i == 3) != (s.Containment() == nil) {
				errs <- errors.New("containment reported on the wrong session")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestContainedRefusalStartsNothing: a refused request returns an
// invalid-config error and never runs the harness.
func TestContainedRefusalStartsNothing(t *testing.T) {
	containedTestSetup(t)
	out, rec := &lockedBuffer{}, &traceRecorder{}
	cfg := containedConfig(t, out, rec, "--mode", "completed")
	cfg.Harness = "opencode" // no profile
	_, err := wrapper.Start(context.Background(), cfg)
	if !errors.Is(err, wrapper.ErrContainmentRefused) || !errors.Is(err, wrapper.ErrInvalidConfig) {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "" {
		t.Fatal("a refused launch ran the harness")
	}
	ev := rec.find("containment_refused")
	if ev == nil || ev.Fields["stage"] != "profile" {
		t.Fatalf("refusal trace = %+v", ev)
	}
	if rec.find("containment_applied") != nil {
		t.Fatal("a refused launch reported an applied policy")
	}
}

// TestContainedBinaryNotFound keeps ErrBinaryNotFound's meaning.
func TestContainedBinaryNotFound(t *testing.T) {
	containedTestSetup(t)
	out, rec := &lockedBuffer{}, &traceRecorder{}
	cfg := containedConfig(t, out, rec)
	cfg.BinaryPath = filepath.Join(t.TempDir(), "no-such-harness")
	res, err := wrapper.Run(context.Background(), cfg)
	if !errors.Is(err, wrapper.ErrBinaryNotFound) || res.Status != wrapper.StatusBinaryNotFound {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

// TestContainedStartRefusedWithoutLandlock: on a kernel that cannot enforce
// (the refusal cells), Start returns ErrContainmentRefused and the harness
// never runs. Skipped where Landlock is available, unless
// HW_LANDLOCK_EXPECT_UNAVAILABLE says the cell must lack it.
func TestContainedStartRefusedWithoutLandlock(t *testing.T) {
	if _, err := landlock.Probe(); err == nil {
		if os.Getenv("HW_LANDLOCK_EXPECT_UNAVAILABLE") != "" {
			t.Fatal("this cell must lack Landlock ABI 9, but it is available")
		}
		t.Skip("Landlock ABI 9 is available here")
	}
	bin, _ := filepath.EvalSymlinks(mockHarnessBin)
	t.Cleanup(contain.RegisterTestProfile(contain.TestProfile{Harness: "mock", ExecDirs: []string{filepath.Dir(bin)}}))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out, rec := &lockedBuffer{}, &traceRecorder{}
	_, err := wrapper.Start(context.Background(), containedConfig(t, out, rec, "--mode", "completed"))
	if !errors.Is(err, wrapper.ErrContainmentRefused) || !errors.Is(err, wrapper.ErrInvalidConfig) {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "" {
		t.Fatal("the harness ran")
	}
	if ev := rec.find("containment_refused"); ev == nil || ev.Fields["stage"] != "kernel" {
		t.Fatalf("refusal trace = %+v", ev)
	}
}

func (r *traceRecorder) find(kind string) *trace.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.events {
		if r.events[i].Kind == kind {
			return &r.events[i]
		}
	}
	return nil
}

// waitOrFail bounds a Wait so a regression hangs no test binary.
func waitOrFail(t *testing.T, s *wrapper.Session, d time.Duration) wrapper.Result {
	t.Helper()
	type r struct {
		res wrapper.Result
		err error
	}
	ch := make(chan r, 1)
	go func() {
		res, err := s.Wait()
		ch <- r{res, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("Wait: %v", got.err)
		}
		return got.res
	case <-time.After(d):
		t.Fatalf("Wait did not return within %s", d)
	}
	return wrapper.Result{}
}
