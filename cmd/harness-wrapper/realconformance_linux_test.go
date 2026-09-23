//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// TestRealClaudeStructuredRouting is structured-run's part of the
// authenticated conformance runs that activate the claude-code profile (G5 in
// the containment plan's Validation). Three real one-turn runs share one
// working directory: contained in private state, contained as the login in a
// caller StateDir, and uncontained. Each must report its own harness session,
// and the transcript and usage structured-run reads back must come from that
// session's own state, never another's.
//
// It needs HW_REAL_CLAUDE (the pinned binary), HW_REAL_CLAUDE_STATE_DIR (a
// StateDir signed in with contain-login), HW_REAL_CLAUDE_OAUTH_TOKEN (a token
// for the other two, as `claude setup-token` prints), and the account's quota.
func TestRealClaudeStructuredRouting(t *testing.T) {
	bin, stateDir, token := os.Getenv("HW_REAL_CLAUDE"), os.Getenv("HW_REAL_CLAUDE_STATE_DIR"), os.Getenv("HW_REAL_CLAUDE_OAUTH_TOKEN")
	if bin == "" || stateDir == "" || token == "" {
		t.Skip("set HW_REAL_CLAUDE, HW_REAL_CLAUDE_STATE_DIR and HW_REAL_CLAUDE_OAUTH_TOKEN to run real structured turns")
	}
	if _, err := landlock.Probe(landlock.MinimumABI); err != nil {
		t.Fatalf("Landlock unavailable: %v", err)
	}
	t.Cleanup(contain.ActivateProfilesForTest())
	// A short state root: claude refuses a private TMPDIR over 79 bytes.
	state, err := os.MkdirTemp("", "hw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HARNESS_BINARY_CLAUDE", bin)
	wd := t.TempDir()
	t.Chdir(wd)

	// The uncontained run's own config root, onboarded and trusting wd.
	config := filepath.Join(t.TempDir(), ".claude")
	seed, _ := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               map[string]any{wd: map[string]any{"hasTrustDialogAccepted": true}},
	})
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, ".claude.json"), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	contained := []string{"--contain", "landlock", "--contain-restrict-tcp", "--contain-allow-tcp", "443"}
	harness := []string{"claude", "--", "--model", "sonnet"}
	stamp := strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	runs := []struct {
		name, word string
		args       []string
		env        map[string]string // "" unsets
	}{
		{
			"private", "heron-" + stamp, append(append([]string{}, contained...), harness...),
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token, "CLAUDE_CONFIG_DIR": ""},
		},
		{
			"caller", "ibis-" + stamp, append(append(append([]string{}, contained...), "--contain-state-dir", stateDir), harness...),
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "", "CLAUDE_CONFIG_DIR": ""},
		},
		{
			"uncontained", "egret-" + stamp, harness,
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token, "CLAUDE_CONFIG_DIR": config},
		},
	}
	results := make([]*turnproto.StructuredTurnResult, len(runs))
	for i, r := range runs {
		for k, v := range r.env {
			t.Setenv(k, v)
			if v == "" {
				_ = os.Unsetenv(k)
			}
		}
		out, code := captureStructuredRun(t, "This is an automated test. Reply with only the word "+r.word+".", r.args)
		res, ok := turnproto.ParseLastJSONLine([]byte(out))
		if !ok || code != 0 {
			t.Fatalf("%s: exit %d; output tail: %s", r.name, code, out[max(0, len(out)-2000):])
		}
		results[i] = res
		state := "uncontained"
		if res.Containment != nil {
			state = res.Containment.State.Mode + " " + res.Containment.State.HarnessState
		}
		t.Logf("%s: %s, harness session %s, state %s, usage %+v, %d transcript entries",
			r.name, res.Status, res.HarnessSessionID, state, res.Usage, len(res.TranscriptEntries))
		if !strings.Contains(res.Reply, r.word) {
			t.Errorf("%s: reply %q, want %s", r.name, res.Reply, r.word)
		}
		if (res.Containment != nil) != (r.name != "uncontained") {
			t.Errorf("%s: containment reported %v", r.name, res.Containment != nil)
		}
		if res.Usage == nil || res.Usage.OutputTokens == 0 {
			t.Errorf("%s: no usage read back from its transcript (%q)", r.name, res.TranscriptError)
		}
	}
	seen := map[string]string{}
	for i, r := range runs {
		res := results[i]
		if other, dup := seen[res.HarnessSessionID]; dup || res.HarnessSessionID == "" {
			t.Errorf("%s: harness session %q (already %s's)", r.name, res.HarnessSessionID, other)
		}
		seen[res.HarnessSessionID] = r.name
		var text strings.Builder
		for _, e := range res.TranscriptEntries {
			text.WriteString(e.Text + "\n")
		}
		if !strings.Contains(text.String(), r.word) {
			t.Errorf("%s: its transcript entries lack its word %s", r.name, r.word)
		}
		for _, o := range runs {
			if o.name != r.name && strings.Contains(text.String(), o.word) {
				t.Errorf("%s: its transcript entries hold %s's word %s", r.name, o.name, o.word)
			}
		}
	}
}
