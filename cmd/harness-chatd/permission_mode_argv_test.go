package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// Behavioural acceptance for the `permission_mode` request field: the string on
// the wire must reach the LAUNCHED PROCESS's argv, per HW-74's mapping table
// (pkg/wrapper/wrapper.go, argsWithHarnessPermissionMode).
//
// Why argv and not wrapper.Config: "reaches wrapper.Config unchanged" is not
// observable from an HTTP test — that struct is internal to chat.Open /
// harness.RunTurn. The fake harness dumps its own launch argv to
// FAKEHARNESS_ARGV_OUT (internal/fakeharness/script.go, ArgvOutVar), which is
// the only end-to-end observation point. fakeharness.CapturedArgv is the wrong
// tool: it covers a DIRECT exec, not a wrapper-driven PTY launch. The precedent
// followed here is pkg/chat/resume_conformance_test.go (argvOutPath /
// fakeLaunchEnv / readArgv).

// argvOutPath returns a fresh path for the fake harness's argv dump.
func argvOutPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "argv.json")
}

// fakeScriptEnvArgv is fakeScriptEnv plus the fake's argv-dump variable. It is
// a sibling rather than an extra parameter so the existing fakeScriptEnv
// callers stay untouched. Both fold in os.Environ() because wrapper.Start
// REPLACES the child environment — without it the fake loses PATH/TERM.
func fakeScriptEnvArgv(t *testing.T, s fakeharness.Script, argvOut string) []string {
	t.Helper()
	return append(fakeScriptEnv(t, s), fakeharness.ArgvOutVar+"="+argvOut)
}

// readArgvDump polls the argv dump the fake writes at startup. The write races
// the launch call's return (the HTTP response can land first), so retry briefly
// instead of reading once. Mirrors readArgv in pkg/chat/resume_conformance_test.go,
// but with a longer budget: that precedent's 2s is marginal here, because these
// cases launch a real PTY child while the rest of `go test -race ./...` saturates
// the box, and an observed miss took just over 2s. Only the WAIT is longer — a
// dump that never appears still fails.
func readArgvDump(t *testing.T, path string) []string {
	t.Helper()
	for i := 0; i < 400; i++ {
		raw, err := os.ReadFile(path)
		if err == nil {
			var got []string
			if err := json.Unmarshal(raw, &got); err == nil {
				return got
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("argv dump never appeared at %s", path)
	return nil
}

// hasFlagPair reports whether argv carries flag immediately followed by value —
// the exact shape prependArgs (pkg/wrapper/wrapper.go) emits.
func hasFlagPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// countTok counts bare occurrences of tok in argv.
func countTok(argv []string, tok string) int {
	n := 0
	for _, a := range argv {
		if a == tok {
			n++
		}
	}
	return n
}

// TestPermissionModeArgv_OpenClaude covers the OPEN request path
// (POST /v1/conversations -> chat.Open) against a claude-code fake: the
// canonical rung "ask" must land in argv as claude's native
// `--permission-mode acceptEdits` (HW-74's mapping table row `ask`),
// prepended by prependArgs.
func TestPermissionModeArgv_OpenClaude(t *testing.T) {
	bin := fakeHarnessBin(t)
	argvOut := argvOutPath(t)
	env := fakeScriptEnvArgv(t, fakeharness.New("claude-code").
		Idle().
		StayAliveUntilStopped().
		Build(), argvOut)

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	openConversation(t, ts, openRequest{
		Harness:        "claude-code",
		BinaryPath:     bin,
		Env:            env,
		PermissionMode: "ask",
	})

	argv := readArgvDump(t, argvOut)
	if !hasFlagPair(argv, "--permission-mode", "acceptEdits") {
		t.Fatalf("argv missing `--permission-mode acceptEdits`: %q", argv)
	}
}

// TestPermissionModeArgv_RunTurnCodex covers the ONE-SHOT request path
// (POST /v1/turns -> harness.RunTurn) against a codex fake, and with it the
// second row of HW-74's mapping table: "manual" -> `-s read-only -a untrusted`.
//
// The script must drive a COMPLETE turn, not just Idle: /v1/turns requires
// exit_after_turn=true and harness.RunTurn blocks until the turn reaches a
// terminal state. Codex completes the instant a fresh "Token usage: …" footer
// is painted (CodexReply), so an Idle-only script would block to the deadline.
func TestPermissionModeArgv_RunTurnCodex(t *testing.T) {
	const sessionID = "abcdef01-2345-6789-abcd-ef0123456789"
	bin := fakeHarnessBin(t)
	argvOut := argvOutPath(t)
	env := fakeScriptEnvArgv(t, fakeharness.New("codex").
		Session(sessionID).
		Idle().
		AwaitSubmit().
		CodexWorking(30, "Thinking").
		CodexReply(40, "reply: "+fakeharness.PromptRef()).
		Build(), argvOut)

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	exitAfter := true
	body, _ := json.Marshal(runTurnRequest{
		Harness:        "codex",
		BinaryPath:     bin,
		Env:            env,
		Prompt:         "ship the permission mode",
		ExitAfterTurn:  &exitAfter,
		PermissionMode: "manual",
	})
	resp, err := http.Post(ts.URL+"/v1/turns", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/turns: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}

	argv := readArgvDump(t, argvOut)
	if !hasFlagPair(argv, "-s", "read-only") {
		t.Fatalf("argv missing `-s read-only`: %q", argv)
	}
	if !hasFlagPair(argv, "-a", "untrusted") {
		t.Fatalf("argv missing `-a untrusted`: %q", argv)
	}
}

// TestPermissionModeArgv_ExplicitFlagWins is the negative row: when the caller
// already passes a same-axis flag in `args`, `permission_mode` injects nothing
// and argv comes through untouched.
//
// ONE spelling only, deliberately. argsContainAnyFlag's three-spelling coverage
// (bare token, `--flag=value`, clap-attached short `-sread-only`) is HW-74's
// unit-test surface in pkg/wrapper/permission_mode_test.go. This chatd row
// exists to prove the HTTP body reaches that decision at all — not to re-test
// the matcher.
//
// Note this is NOT built from `--dangerously-skip-permissions` + a non-bypass
// mode: HW-74 REJECTS that pairing, so it is a 400 case (covered by the
// error-mapping tests), not an argv-untouched case.
func TestPermissionModeArgv_ExplicitFlagWins(t *testing.T) {
	bin := fakeHarnessBin(t)
	argvOut := argvOutPath(t)
	env := fakeScriptEnvArgv(t, fakeharness.New("claude-code").
		Idle().
		StayAliveUntilStopped().
		Build(), argvOut)

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	openConversation(t, ts, openRequest{
		Harness:    "claude-code",
		BinaryPath: bin,
		Env:        env,
		// Same axis as permission_mode, spelled explicitly by the caller.
		Args:           []string{"--permission-mode", "acceptEdits"},
		PermissionMode: "plan",
	})

	argv := readArgvDump(t, argvOut)
	if n := countTok(argv, "--permission-mode"); n != 1 {
		t.Fatalf("argv has %d `--permission-mode` flags, want exactly the caller's 1: %q", n, argv)
	}
	if !hasFlagPair(argv, "--permission-mode", "acceptEdits") {
		t.Fatalf("caller's `--permission-mode acceptEdits` was rewritten: %q", argv)
	}
	if hasFlagPair(argv, "--permission-mode", "plan") {
		t.Fatalf("permission_mode was injected despite an explicit same-axis flag: %q", argv)
	}
}

// TestPermissionModeArgv_ListingPresenceAndAbsence pins BOTH halves of the
// listing contract: a conversation opened WITH permission_mode reports it, and
// one opened WITHOUT it omits the key entirely rather than emitting "".
//
// The body is decoded into []map[string]json.RawMessage, not the obvious
// []conversationSummary: decoding into the struct erases the presence/absence
// distinction (a missing key and "" both land as the zero value), so the
// absence half would pass silently even if `omitempty` were dropped from the
// struct tag. Raw keys are the only spelling that actually tests it.
func TestPermissionModeArgv_ListingPresenceAndAbsence(t *testing.T) {
	bin := fakeHarnessBin(t)

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	newEnv := func() []string {
		return fakeScriptEnv(t, fakeharness.New("claude-code").
			Idle().
			StayAliveUntilStopped().
			Build())
	}

	withMode := openConversation(t, ts, openRequest{
		Harness:        "claude-code",
		BinaryPath:     bin,
		Env:            newEnv(),
		PermissionMode: "ask",
	})
	withoutMode := openConversation(t, ts, openRequest{
		Harness:    "claude-code",
		BinaryPath: bin,
		Env:        newEnv(),
	})
	if withMode == withoutMode {
		t.Fatalf("both opens returned the same conversation id %q", withMode)
	}

	resp, err := http.Get(ts.URL + "/v1/conversations")
	if err != nil {
		t.Fatalf("GET /v1/conversations: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	var convs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &convs); err != nil {
		t.Fatalf("decode conversations: %v (body = %s)", err, raw)
	}
	byID := make(map[string]map[string]json.RawMessage, len(convs))
	for _, c := range convs {
		var id string
		if err := json.Unmarshal(c["id"], &id); err != nil {
			t.Fatalf("decode summary id: %v", err)
		}
		byID[id] = c
	}

	present, ok := byID[withMode]
	if !ok {
		t.Fatalf("conversation %q missing from listing: %s", withMode, raw)
	}
	gotMode, ok := present["permission_mode"]
	if !ok {
		t.Fatalf("summary for %q has no permission_mode key: %s", withMode, raw)
	}
	if string(gotMode) != `"ask"` {
		t.Fatalf("permission_mode = %s, want \"ask\"", gotMode)
	}

	absent, ok := byID[withoutMode]
	if !ok {
		t.Fatalf("conversation %q missing from listing: %s", withoutMode, raw)
	}
	if v, ok := absent["permission_mode"]; ok {
		t.Fatalf("permission_mode key present (= %s) for a conversation opened without one; omitempty should drop it: %s", v, raw)
	}
}

// TestPermissionModeEnv_OpenBypassAddsNoSandboxEnv is the WIRE-altitude half of
// harness-wrapper's no-silent-injection property (cmd/harness-wrapper/sandbox_defaults.go):
// a `permission_mode: "bypass"` arriving over the chatd wire delivers the ARG
// half only. The spawned harness must see `--permission-mode bypassPermissions`
// in argv and IS_SANDBOX **unset** — chatd has no --sandbox-defaults equivalent,
// so a caller wanting the env half must put IS_SANDBOX=1 in `env` itself. That
// is the documented contract, not a gap: see "permission_mode semantics" in
// docs/md/guide/gateway.md and the same caveat on openRequest.PermissionMode
// (types.go).
//
// Why a shell shim rather than the fake's argv dump: the fake records argv but
// not its environment. openRequest.BinaryPath and .Env are caller-supplied, so
// the structured_run_test.go recording-shim idiom drops straight in — the shim
// dumps "$@" and IS_SANDBOX, then EXECs the fake (exec, not fork, keeps the
// harness a direct child of the PTY).
func TestPermissionModeEnv_OpenBypassAddsNoSandboxEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script recording shim is Unix-only")
	}
	// Ambient-IS_SANDBOX hygiene: fakeScriptEnv folds in os.Environ(), so
	// without this the assertion below would read the TEST HOST's environment
	// and invert on any box (or container) that already exports IS_SANDBOX.
	// Unset rather than t.Setenv("IS_SANDBOX", "") — the latter DEFINES the key.
	if prev, ok := os.LookupEnv("IS_SANDBOX"); ok {
		os.Unsetenv("IS_SANDBOX")
		t.Cleanup(func() { os.Setenv("IS_SANDBOX", prev) })
	}

	bin := fakeHarnessBin(t)
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "record.txt")
	shimPath := filepath.Join(dir, "claude-shim")
	shim := fmt.Sprintf(`#!/bin/sh
{
  printf 'ARGS:'
  for a in "$@"; do printf ' %%s' "$a"; done
  printf '\n'
  printf 'IS_SANDBOX=%%s\n' "${IS_SANDBOX-unset}"
} > %q
exec %q "$@"
`, recordPath, bin)
	if err := os.WriteFile(shimPath, []byte(shim), 0o700); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	openConversation(t, ts, openRequest{
		Harness:    "claude-code",
		BinaryPath: shimPath,
		Env: fakeScriptEnv(t, fakeharness.New("claude-code").
			Idle().
			StayAliveUntilStopped().
			Build()),
		PermissionMode: "bypass",
	})

	rec := readShimRecord(t, recordPath)
	// Whole recorded line, not a bare substring: "IS_SANDBOX=1" would also match
	// an ambient IS_SANDBOX=10.
	if !strings.Contains(rec, "\nIS_SANDBOX=unset\n") {
		t.Fatalf("bypass over the wire set IS_SANDBOX; want it unset; record:\n%s", rec)
	}
	if !strings.Contains(rec, "--permission-mode bypassPermissions") {
		t.Fatalf("argv missing `--permission-mode bypassPermissions`; record:\n%s", rec)
	}
}

// readShimRecord polls for the recording shim's dump on the same budget and for
// the same reason as readArgvDump: the shim writes at launch, which races the
// HTTP response.
func readShimRecord(t *testing.T, path string) string {
	t.Helper()
	for i := 0; i < 400; i++ {
		if raw, err := os.ReadFile(path); err == nil && bytes.Contains(raw, []byte("IS_SANDBOX=")) {
			return string(raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shim record never appeared at %s", path)
	return ""
}
