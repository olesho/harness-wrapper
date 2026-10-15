package workdir

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/codex"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Codex's live run reads, beside HW_REAL_CODEX (the pinned native codex, its
// codex-code-mode-host beside it):
const (
	// envCodexLogin names a ChatGPT login's auth.json. The run lends its
	// access token alone, never its refresh token, as a runtime lends one.
	envCodexLogin = "HW_WORKDIR_CODEX_LOGIN"
	// envCodexModel names the model; codex's default when unset.
	envCodexModel = "HW_WORKDIR_CODEX_MODEL"
)

// The pinned Codex works in a directory of its own (session_workdir), with a
// real account.
func TestCodexWorkDirsLive(t *testing.T) {
	bin, login := os.Getenv("HW_REAL_CODEX"), os.Getenv(envCodexLogin)
	if bin == "" || login == "" {
		t.Skipf("HW_REAL_CODEX and %s name the pinned codex and a ChatGPT login", envCodexLogin)
	}
	if out, err := exec.Command(bin, "--version").Output(); err != nil || !strings.Contains(string(out), pinned(codex.Name)) {
		t.Fatalf("%s is codex %q, %v: the profile pins %s", bin, out, err, pinned(codex.Name))
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, codex.BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	// A code-mode model runs its tools through the host beside codex.
	host := filepath.Join(filepath.Dir(bin), "codex-code-mode-host")
	if _, err := os.Stat(host); err != nil {
		t.Fatalf("codex-code-mode-host beside %s: %v", bin, err)
	}
	if err := os.Symlink(host, filepath.Join(root, "bin", "codex-code-mode-host")); err != nil {
		t.Fatal(err)
	}
	run(t, fixture{
		name: codex.Name, adapter: adapter.New(codex.Profile{}), root: root,
		spec: contract.AgentSpec{Model: os.Getenv(envCodexModel)},
		credential: func(t *testing.T, l contract.Layout) *contract.CredentialFile {
			return lend(t, login, filepath.Join(l.Secrets, codex.CredentialLogin))
		},
	}, &report{})
}

// lend stages a ChatGPT login at to as a runtime lends one: its access
// token, never its refresh token.
func lend(t *testing.T, from, to string) *contract.CredentialFile {
	t.Helper()
	b, err := os.ReadFile(from) //nolint:gosec // the login's path, from the environment
	if err != nil {
		t.Fatalf("the login: %v", err)
	}
	var auth map[string]any
	if err := json.Unmarshal(b, &auth); err != nil {
		t.Fatal("the login is not a codex auth.json")
	}
	tokens, _ := auth["tokens"].(map[string]any)
	if tokens == nil {
		t.Fatal("the login holds no tokens")
	}
	delete(tokens, "refresh_token")
	out, _ := json.Marshal(auth)
	if err := os.WriteFile(to, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return &contract.CredentialFile{Kind: codex.CredentialLogin, File: to}
}
