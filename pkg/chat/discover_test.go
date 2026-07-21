package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/discovery/models"
)

// TestDiscoverModels_UnsupportedHarness pins the first arm of the error
// contract: a harness with no parseable `/model` picker (pi/opencode/generic)
// fast-fails with ErrPickerUnsupported BEFORE any session is launched — so it
// needs no binary. Mirrors the TS `pickerHeader === null` throw.
func TestDiscoverModels_UnsupportedHarness(t *testing.T) {
	for _, h := range []string{"pi", "opencode", "generic", "", "unknown"} {
		t.Run(h, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := DiscoverModels(ctx, DiscoverModelsOptions{
				Harness:    h,
				BinaryPath: "/nonexistent/harness-binary",
			})
			if !errors.Is(err, ErrPickerUnsupported) {
				t.Fatalf("err = %v, want ErrPickerUnsupported", err)
			}
			if got != nil {
				t.Errorf("models = %v, want nil", got)
			}
		})
	}
}

// TestDiscoverModels_SupportedSet confirms the supported set matches the
// parser's header gate (claude/claude-code/codex) and normalizes
// case/whitespace — the gate that decides ErrPickerUnsupported.
func TestDiscoverModels_SupportedSet(t *testing.T) {
	supported := []string{"claude", "claude-code", "codex", "  Claude-Code  ", "CODEX"}
	for _, h := range supported {
		if !pickerSupported(h) {
			t.Errorf("pickerSupported(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"pi", "opencode", "generic", ""} {
		if pickerSupported(h) {
			t.Errorf("pickerSupported(%q) = true, want false", h)
		}
	}
}

// TestDiscoverModels_AuthRequired pins the auth fast-fail arm: on a harness
// sitting in a first-run onboarding wall ("Select login method"), readiness
// surfaces ErrAuthRequired, so DiscoverModels returns it immediately instead of
// writing `/model` and hanging to the render deadline. Uses a real fake-harness
// PTY so the readiness gate runs end to end.
func TestDiscoverModels_AuthRequired(t *testing.T) {
	bin := buildFakeHarness(t)

	// Paint an onboarding WALL (login-method screen) that never becomes ready.
	// onboardingWall matches "select login method"; readyForInput stays false, so
	// waitReadyForSend short-circuits with ErrAuthRequired without any dwell.
	script := fakeharness.New("claude-code").
		Raw(0, "Claude Code").
		Raw(0, "Select login method").
		Raw(0, "❯ 1. Claude account").
		StayAliveUntilStopped().
		Build()

	// A render budget well under the test timeout: if the auth gate did NOT
	// fast-fail, the test would instead hang to the render deadline — so this
	// bounds that failure loudly.
	got, err := runDiscover(t, bin, script, 3*time.Second)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
	if got != nil {
		t.Errorf("models = %v, want nil", got)
	}
}

// TestDiscoverModels_PickerRenders drives the happy path end to end: an idle
// claude-code composer (ready), then a `/model` picker screen. DiscoverModels
// must gate on readiness, write `/model`, poll the settled screen, and return
// the parsed models.
func TestDiscoverModels_PickerRenders(t *testing.T) {
	bin := buildFakeHarness(t)

	script := fakeharness.New("claude-code").
		Idle().        // ready composer: "Claude Code" + "❯"
		AwaitSubmit(). // block until the driver writes "/model" + CSI 13u
		Raw(0, modelPickerScreen()).
		StayAliveUntilStopped().
		Build()

	got, err := runDiscover(t, bin, script, 5*time.Second)
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(got), got)
	}
	byID := map[string]models.Info{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if _, ok := byID["default"]; !ok {
		t.Errorf("missing model id %q in %v", "default", got)
	}
	if _, ok := byID["opus"]; !ok {
		t.Errorf("missing model id %q in %v", "opus", got)
	}
	// The "Opus ✔" row is the active model; the "Default (recommended)" row is
	// the default. Confirm the markers round-tripped through the parser.
	if m := byID["opus"]; !m.Current {
		t.Errorf("opus: Current = false, want true (%+v)", m)
	}
	if m := byID["default"]; !m.IsDefault {
		t.Errorf("default: IsDefault = false, want true (%+v)", m)
	}
}

// TestDiscoverModels_PickerTimeout pins the render-deadline arm: the composer is
// ready (so the auth gate passes) but the picker never renders, so DiscoverModels
// returns ErrPickerTimeout after the render budget elapses.
func TestDiscoverModels_PickerTimeout(t *testing.T) {
	bin := buildFakeHarness(t)

	// Idle → AwaitSubmit → hold WITHOUT ever painting a picker. The driver passes
	// readiness, writes "/model", then polls a screen that never becomes a picker.
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		StayAliveUntilStopped().
		Build()

	got, err := runDiscover(t, bin, script, 400*time.Millisecond)
	if !errors.Is(err, ErrPickerTimeout) {
		t.Fatalf("err = %v, want ErrPickerTimeout", err)
	}
	if got != nil {
		t.Errorf("models = %v, want nil", got)
	}
}

// modelPickerScreen returns a claude-code `/model` picker screen: the "Select
// model" header (the parser's gate) plus two rows in the "label  description"
// two-column shape rowRe expects (2+ spaces between the columns).
func modelPickerScreen() string {
	return strings.Join([]string{
		"Select model",
		"",
		"  1. Default (recommended)   Sonnet 4.5 for daily coding",
		"  2. Opus ✔                  Opus 4.1 for deep work",
		"",
	}, "\n")
}

// runDiscover writes script to a temp file, points the fake harness at it via
// FAKEHARNESS_SCRIPT (full environment so the child keeps PATH/TERM, matching
// openFake), and runs DiscoverModels against that binary with the given render
// budget. The ambient context carries a generous timeout so a hung readiness
// gate fails the test rather than blocking forever.
func runDiscover(t *testing.T, bin string, script fakeharness.Script, renderTimeout time.Duration) ([]models.Info, error) {
	t.Helper()

	data, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(scriptPath, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	return DiscoverModels(ctx, DiscoverModelsOptions{
		Harness:       script.Harness,
		BinaryPath:    bin,
		Env:           append(os.Environ(), fakeharness.EnvVar+"="+scriptPath),
		Cols:          120,
		Rows:          40,
		RenderTimeout: renderTimeout,
	})
}
