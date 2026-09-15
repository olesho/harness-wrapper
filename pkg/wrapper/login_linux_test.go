//go:build linux

package wrapper_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// mockLoginCode is the code the mock's login mode accepts.
const mockLoginCode = "mock-code#mock-state"

// loginTestSetup registers inactive profiles for the mock's two login flows
// (claude-style "mock-login" and codex-style "mock-device"), whose state root
// is $MOCK_CONFIG_DIR and whose credential variable is MOCK_AUTH_TOKEN.
func loginTestSetup(t *testing.T) {
	t.Helper()
	containedTestSetup(t)
	bin, _ := filepath.EvalSymlinks(mockHarnessBin)
	status := []string{"--mode", "login-status"}
	for _, tp := range []contain.TestProfile{
		{Harness: "mock-login", Login: &contain.TestLogin{
			Args: []string{"--mode", "login"}, Status: status,
			URL: `https://\S+/oauth/authorize\?\S+`, CodePrompt: "Paste code here if prompted",
			Success: "Login successful", LoggedIn: `"loggedIn":\s*true`,
		}},
		{Harness: "mock-device", Login: &contain.TestLogin{
			Args: []string{"--mode", "login-device"}, Status: status,
			URL: `https://\S*/codex/device\S*`, UserCode: `\b([A-Z0-9]{4,5}-[A-Z0-9]{4,6})\b`,
			Success: "Successfully logged in", LoggedIn: `"loggedIn":\s*true`,
		}},
	} {
		tp.ExecDirs, tp.ConfigEnv, tp.AuthEnv, tp.Inactive = []string{filepath.Dir(bin)}, "MOCK_CONFIG_DIR", []string{"MOCK_AUTH_TOKEN"}, true
		t.Cleanup(contain.RegisterTestProfile(tp))
	}
}

func loginConfig(t *testing.T, harness string, out *lockedBuffer) wrapper.LoginConfig {
	return wrapper.LoginConfig{
		Harness:    harness,
		BinaryPath: mockHarnessBin,
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Env:        append(os.Environ(), "MOCK_AUTH_TOKEN=from-the-environment"),
		Output:     out,
	}
}

func promptOrFail(t *testing.T, l *wrapper.Login) wrapper.LoginPrompt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := l.Prompt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoginWithCode: claude's shape. The page's code is typed at the prompt,
// the login lands in the StateDir, the lingering "press Enter" is answered,
// and the status command, run in the same StateDir, finds the login.
func TestLoginWithCode(t *testing.T) {
	loginTestSetup(t)
	out := &lockedBuffer{}
	cfg := loginConfig(t, "mock-login", out)
	l, err := wrapper.StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := promptOrFail(t, l)
	if p.URL != "https://login.example.test/oauth/authorize?code=true&state=mock" || !p.WantsCode || p.UserCode != "" {
		t.Fatalf("prompt = %+v", p)
	}
	if err := l.SubmitCode(" " + mockLoginCode + "\n"); err != nil {
		t.Fatal(err)
	}
	res, err := l.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !res.LoggedIn || !strings.Contains(res.Status, `"loggedIn": true`) {
		t.Fatalf("not signed in: %+v\noutput: %q", res, out.String())
	}
	if res.Result.Status != wrapper.StatusIdle || res.Result.ExitCode != 0 {
		t.Errorf("the login command did not end on its own after the Enter: %+v", res.Result)
	}
	if !strings.Contains(res.Output, "Login successful") {
		t.Errorf("login output: %q", res.Output)
	}
	a := res.Containment
	stateDir, _ := filepath.EvalSymlinks(cfg.StateDir)
	if a == nil || a.State.Mode != "caller" || a.State.StateDir != stateDir || a.Profile != "test-mock-login@test" {
		t.Fatalf("login policy = %+v", a)
	}
	if slices.Contains(a.Env, "MOCK_AUTH_TOKEN") {
		t.Errorf("the login received the credential variable: %v", a.Env)
	}
	cred := filepath.Join(cfg.StateDir, "home", ".config-mock-login", ".credentials.json")
	if _, err := os.Stat(cred); err != nil {
		t.Fatalf("the login is not in the StateDir: %v", err)
	}

	st, err := wrapper.LoginStatus(context.Background(), cfg)
	if err != nil || !st.LoggedIn {
		t.Fatalf("LoginStatus = %+v, %v", st, err)
	}
	if err := os.Remove(cred); err != nil {
		t.Fatal(err)
	}
	// The environment's credential would make the mock report a login; the
	// status check never passes it.
	st, err = wrapper.LoginStatus(context.Background(), cfg)
	if err != nil || st.LoggedIn {
		t.Fatalf("LoginStatus without the stored login = %+v, %v", st, err)
	}
}

// TestLoginWrongCode: a rejected code stores nothing; the harness asks again
// until the login is stopped, and Wait reports no login.
func TestLoginWrongCode(t *testing.T) {
	loginTestSetup(t)
	out := &lockedBuffer{}
	cfg := loginConfig(t, "mock-login", out)
	l, err := wrapper.StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	promptOrFail(t, l)
	if err := l.SubmitCode("wrong\x1b[A"); !errors.Is(err, wrapper.ErrInvalidConfig) {
		t.Errorf("a code with control characters: got %v, want ErrInvalidConfig", err)
	}
	if err := l.SubmitCode("wrong-code"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), "Invalid code") {
		if time.Now().After(deadline) {
			t.Fatalf("the mock never rejected the code: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := l.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.LoggedIn || res.Result.Status != wrapper.StatusInterrupted {
		t.Fatalf("result = %+v", res)
	}
}

// TestLoginDeviceCode: codex's shape. The page and a one-time code, no code
// to type back, and a login the harness completes by itself.
func TestLoginDeviceCode(t *testing.T) {
	loginTestSetup(t)
	cfg := loginConfig(t, "mock-device", &lockedBuffer{})
	l, err := wrapper.StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := promptOrFail(t, l)
	if p.URL != "https://login.example.test/codex/device" || p.UserCode != "MOCK-C0DE1" || p.WantsCode {
		t.Fatalf("prompt = %+v", p)
	}
	if err := l.SubmitCode("anything"); !errors.Is(err, wrapper.ErrInvalidConfig) {
		t.Errorf("SubmitCode on a device login: got %v, want ErrInvalidConfig", err)
	}
	res, err := l.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !res.LoggedIn || res.Result.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
}

// TestLoginOnlyForLogin: the exception that lets an inactive profile run is a
// login's alone. A session of the same profile is refused.
func TestLoginOnlyForLogin(t *testing.T) {
	loginTestSetup(t)
	cfg := containedConfig(t, &lockedBuffer{}, &traceRecorder{}, "--mode", "login")
	cfg.Harness = "mock-login"
	cfg.Containment.StateDir = t.TempDir()
	if _, err := wrapper.Start(context.Background(), cfg); !errors.Is(err, wrapper.ErrContainmentRefused) || !strings.Contains(err.Error(), "not activated") {
		t.Fatalf("a session of an inactive profile: got %v, want a refusal", err)
	}
}

// TestLoginPromptWhenCommandFails: a login command that ends before printing a
// sign-in page fails Prompt with its output.
func TestLoginPromptWhenCommandFails(t *testing.T) {
	loginTestSetup(t)
	bin, _ := filepath.EvalSymlinks(mockHarnessBin)
	t.Cleanup(contain.RegisterTestProfile(contain.TestProfile{
		Harness: "mock-broken", ExecDirs: []string{filepath.Dir(bin)}, Inactive: true,
		Login: &contain.TestLogin{Args: []string{"--mode", "failed"}, Status: []string{"--mode", "login-status"}, URL: `https://\S+`, LoggedIn: `"loggedIn":\s*true`},
	}))
	l, err := wrapper.StartLogin(context.Background(), loginConfig(t, "mock-broken", &lockedBuffer{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := l.Prompt(ctx); err == nil || !strings.Contains(err.Error(), "workspace is not writable") {
		t.Fatalf("Prompt = %v, want the command's output", err)
	}
	if res, err := l.Wait(); err != nil || res.LoggedIn {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
}
