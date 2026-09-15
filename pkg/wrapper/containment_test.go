package wrapper_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

// traceRecorder collects trace events.
type traceRecorder struct {
	mu     sync.Mutex
	events []trace.Event
}

func (r *traceRecorder) Emit(e trace.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *traceRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

// TestContainmentUnsupportedOffLinux: requesting containment on a platform
// without Landlock is an invalid-config error, before anything starts.
func TestContainmentUnsupportedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("containment is supported on linux")
	}
	var out bytes.Buffer
	_, err := wrapper.Start(context.Background(), wrapper.Config{
		BinaryPath:  mockHarnessBin,
		Args:        []string{"--mode", "completed"},
		Stdout:      &out,
		Containment: &wrapper.Containment{Kind: wrapper.ContainmentLandlock},
	})
	if !errors.Is(err, wrapper.ErrContainmentUnsupported) || !errors.Is(err, wrapper.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrContainmentUnsupported wrapping ErrInvalidConfig", err)
	}
	if out.Len() != 0 {
		t.Fatal("a refused launch produced harness output")
	}
}

// TestContainmentInvalidRequest: a malformed request is refused as invalid
// config on every platform.
func TestContainmentInvalidRequest(t *testing.T) {
	var out bytes.Buffer
	_, err := wrapper.Start(context.Background(), wrapper.Config{
		BinaryPath:  mockHarnessBin,
		Args:        []string{"--mode", "completed"},
		Stdout:      &out,
		Containment: &wrapper.Containment{Kind: "seatbelt"},
	})
	if !errors.Is(err, wrapper.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if runtime.GOOS == "linux" && !errors.Is(err, wrapper.ErrContainmentRefused) {
		t.Fatalf("err = %v, want ErrContainmentRefused", err)
	}
}

// TestUncontainedIsUnchanged pins G9: with containment unset nothing of the
// feature runs — no containment trace, no applied policy, no managed-state
// directory, and the harness sees exactly the configured environment.
func TestUncontainedIsUnchanged(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent-home-for-test", "HW_MARKER=uncontained"}
	var out bytes.Buffer
	rec := &traceRecorder{}
	envBin := "/usr/bin/env"
	if _, err := os.Stat(envBin); err != nil {
		t.Skip("/usr/bin/env not available")
	}
	s, err := wrapper.Start(context.Background(), wrapper.Config{
		BinaryPath: envBin,
		Env:        env,
		Stdout:     &out,
		Trace:      rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Wait()
	if err != nil || res.Status != wrapper.StatusIdle {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
	if s.Containment() != nil {
		t.Fatal("an uncontained session reports an applied policy")
	}
	for _, k := range rec.kinds() {
		if strings.HasPrefix(k, "containment_") {
			t.Fatalf("uncontained launch emitted %s", k)
		}
	}
	var got []string
	for _, line := range strings.Split(strings.ReplaceAll(out.String(), "\r", ""), "\n") {
		if line != "" {
			got = append(got, line)
		}
	}
	want := env
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("harness environment changed:\n got %q\nwant %q", got, want)
	}
	if ents, _ := os.ReadDir(stateHome); len(ents) != 0 {
		t.Fatalf("an uncontained launch created %d entries under XDG_STATE_HOME", len(ents))
	}
}
