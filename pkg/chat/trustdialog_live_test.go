package chat

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// The live folder-trust repro. It is the only thing that actually proves the
// fix: every other test in this package feeds the confirm loop a screen the
// test itself painted, and the bug was never in what the harness BELIEVED the
// screen said — the keys were right and the parse was right. What went wrong
// was between the write and the real TUI, so only the real TUI can settle it.
//
// Env-gated, following resize_deadlock_linux_test.go: CI and `make test` skip
// it, because it needs the claude binary and a writable profile. Run it with
//
//	HW_LIVE_TRUST=1 go test ./pkg/chat -run TrustDialogLive -v
//
// It COSTS NOTHING: no prompt is ever sent, so no tokens are spent. It opens a
// conversation in a fresh directory claude has never seen, lets the InputPolicy
// answer the trust dialog, and asserts the dialog is gone and the composer is
// reachable inside a bounded wall clock. A run that hangs FAILS — the hang is
// the bug.
//
// CLAUDE_CONFIG_DIR points at a COPY of a working profile with its `projects`
// map emptied, so the dialog is guaranteed to appear (claude shows it once per
// unseen path) and the real profile is never touched. The proof that claude
// itself accepted the answer — rather than the harness having merely stopped
// seeing the dialog — is that claude writes hasTrustDialogAccepted for the temp
// path into that copy, which the assertions below read back.
const (
	liveTrustEnv     = "HW_LIVE_TRUST"
	liveTrustBudget  = 60 * time.Second
	liveTrustProfile = "HW_LIVE_TRUST_PROFILE" // optional override of ~/.claude.json
)

func TestTrustDialogLive(t *testing.T) {
	if os.Getenv(liveTrustEnv) != "1" {
		t.Skipf("live trust-dialog repro: set %s=1 (needs the real claude binary; sends no prompt, costs no tokens)", liveTrustEnv)
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}

	workDir := t.TempDir()
	configDir := liveTrustConfigDir(t)
	ptyLog := filepath.Join(t.TempDir(), "pty.raw")

	conv, err := Open(context.Background(), Options{
		Harness:    chatClaudeCode,
		BinaryPath: bin,
		WorkingDir: workDir,
		Env: append(os.Environ(),
			"CLAUDE_CONFIG_DIR="+configDir,
			"HOME="+configDir,
		),
		Store: newFakeStore(),
		Cols:  120,
		Rows:  40,
		InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
		}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conv.Close(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), liveTrustBudget)
	defer cancel()

	// waitReadyForSend is the whole assertion: it returns nil only when the
	// composer is painted, ErrInputPending / *InputUnresolvedError when the
	// dialog is still up, and ctx.DeadlineExceeded on the hang this test exists
	// to fail on.
	readyErr := conv.waitReadyForSend(ctx)

	final := conv.screen.Snapshot().Text
	if err := os.WriteFile(ptyLog, []byte(final), 0o600); err == nil {
		t.Logf("final screen written to %s", ptyLog)
	}

	if readyErr != nil {
		t.Fatalf("composer not reachable within %s: %v\n--- final screen ---\n%s", liveTrustBudget, readyErr, final)
	}
	if claudecode.AnchorPresent(final) {
		t.Fatalf("the trust dialog is still on screen after the answer:\n%s", final)
	}

	// Claude's OWN record that it accepted the answer. Without this the test
	// would pass on a dialog that merely scrolled out of view.
	if !liveTrustAccepted(t, configDir, workDir) {
		t.Fatalf("claude did not record hasTrustDialogAccepted for %s in %s", workDir, configDir)
	}
}

// liveTrustConfigDir copies the caller's claude profile into a temp dir and
// empties its `projects` map, so the trust dialog is guaranteed to appear and
// the real profile is never written to.
func liveTrustConfigDir(t *testing.T) string {
	t.Helper()
	src := os.Getenv(liveTrustProfile)
	if src == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home directory to copy a claude profile from: %v", err)
		}
		src = filepath.Join(home, ".claude.json")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("no usable claude profile at %s: %v (set %s to point at one)", src, err, liveTrustProfile)
	}
	var profile map[string]any
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Skipf("claude profile at %s is not JSON: %v", src, err)
	}
	profile["projects"] = map[string]any{}

	dir := t.TempDir()
	out, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("re-marshal profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), out, 0o600); err != nil {
		t.Fatalf("write profile copy: %v", err)
	}
	return dir
}

// liveTrustAccepted reports whether claude wrote hasTrustDialogAccepted for
// workDir into the profile copy. Claude flushes its config asynchronously, so
// this polls rather than reading once.
func liveTrustAccepted(t *testing.T, configDir, workDir string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
		if err == nil {
			var profile struct {
				Projects map[string]struct {
					HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
				} `json:"projects"`
			}
			if json.Unmarshal(raw, &profile) == nil {
				for path, p := range profile.Projects {
					// t.TempDir can hand back a /var symlink to /private/var on
					// darwin; claude records the resolved path.
					if p.HasTrustDialogAccepted && strings.HasSuffix(path, strings.TrimPrefix(workDir, "/private")) {
						return true
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}
