//go:build linux

package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

func requireLandlock(t *testing.T) {
	t.Helper()
	if _, err := landlock.Probe(0); err != nil {
		if v := os.Getenv("HW_LANDLOCK_REQUIRE_ABI"); v != "" && v != "0" {
			t.Fatalf("Landlock ABI 9 required: %v", err)
		}
		t.Skipf("Landlock ABI 9 unavailable: %v", err)
	}
}

// TestRunTurnContained drives one turn of the fake harness inside a Landlock
// domain through RunTurn, a single-launch caller: the conversation is never
// reopened, so it needs no supervision, its record is not resumable, and the
// harness id lives in the containment record, never the legacy field.
func TestRunTurnContained(t *testing.T) {
	requireLandlock(t)
	for _, supervised := range []bool{true, false} {
		t.Run(map[bool]string{true: "supervision-if-available", false: "no-supervision"}[supervised], func(t *testing.T) {
			if !supervised {
				t.Cleanup(contain.DisableSupervisionForTest())
			}
			// The fake paints a resume hint naming its own id; the id the launch
			// assigned must be the one recorded.
			const hintID = "123e4567-e89b-12d3-a456-426614174000"
			bin := fakeBin(t)
			realBin, _ := filepath.EvalSymlinks(bin)
			t.Cleanup(contain.RegisterTestProfile(contain.TestProfile{
				Harness: "claude-code", ExecDirs: []string{filepath.Dir(realBin)}, ConfigEnv: "CLAUDE_CONFIG_DIR",
			}))
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			argvOut := filepath.Join(t.TempDir(), "argv.json")
			env := append(scriptEnv(t, fakeharness.New("claude-code").
				Session(hintID).
				Idle().
				AwaitSubmit().
				Working(30, "Working").
				Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
				StayAliveUntilStopped().
				Build()), fakeharness.ArgvOutVar+"="+argvOut)
			var scriptDir string
			for _, kv := range env {
				if v, ok := cutPrefix(kv, fakeharness.EnvVar+"="); ok {
					scriptDir = filepath.Dir(v)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := harness.RunTurn(ctx, harness.TurnConfig{
				Harness:       "claude",
				BinaryPath:    bin,
				Env:           env,
				WorkingDir:    t.TempDir(),
				Prompt:        "ship the contained turn",
				ExitAfterTurn: true,
				Containment: &wrapper.Containment{
					Kind:      wrapper.ContainmentLandlock,
					ReadOnly:  []string{scriptDir},
					ReadWrite: []string{filepath.Dir(argvOut)},
					PassEnv:   []string{fakeharness.EnvVar, fakeharness.ArgvOutVar},
				},
			})
			if err != nil {
				t.Fatalf("RunTurn: %v", err)
			}
			if res.Turn.State != chat.TurnStateComplete {
				t.Fatalf("Turn.State = %q", res.Turn.State)
			}
			a := res.Containment
			if a == nil || a.Fingerprint == "" {
				t.Fatalf("no applied policy on a contained turn: %+v", a)
			}
			argv := readArgv(t, argvOut)
			if len(argv) < 2 || argv[0] != "--session-id" {
				t.Fatalf("argv = %q, want the assigned --session-id first", argv)
			}
			if res.Session.HarnessSessionID != "" || res.Session.HarnessID() != argv[1] {
				t.Fatalf("harness id legacy=%q accessor=%q, want the assigned %q in the record only",
					res.Session.HarnessSessionID, res.Session.HarnessID(), argv[1])
			}
			rec := res.Session.Containment
			if rec == nil || rec.Resumable || rec.LastLaunch == nil {
				t.Fatalf("single-launch record = %+v", rec)
			}
			switch a.Supervision.Mode {
			case containment.SupervisionCgroup:
				if a.Supervision.Cleanup != "complete" {
					t.Fatalf("cleanup = %q", a.Supervision.Cleanup)
				}
				if _, err := os.Stat(a.State.Home); !os.IsNotExist(err) {
					t.Fatalf("private state left after a complete cleanup: %v", err)
				}
			default:
				if supervised && os.Getenv("HW_LANDLOCK_REQUIRE_SUPERVISION") != "" {
					t.Fatalf("supervision required but %s: %s", a.Supervision.Mode, a.Supervision.Reason)
				}
				if _, err := os.Stat(a.State.Home); err != nil {
					t.Fatalf("unsupervised state must be kept for the caller: %v", err)
				}
			}
		})
	}
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}
