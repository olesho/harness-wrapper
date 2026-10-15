package workdir

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Claude Code's live run reads, beside HW_REAL_CLAUDE (the pinned claude):
const (
	// envClaudeToken names a file holding a `claude setup-token` token.
	envClaudeToken = "HW_WORKDIR_CLAUDE_TOKEN_FILE" //nolint:gosec // an environment variable's name
	// envClaudeModel names the model; haiku when unset.
	envClaudeModel = "HW_WORKDIR_CLAUDE_MODEL"
)

// The pinned Claude Code works in a directory of its own (session_workdir),
// with a real account.
func TestClaudeWorkDirsLive(t *testing.T) {
	bin, token := os.Getenv("HW_REAL_CLAUDE"), os.Getenv(envClaudeToken)
	if bin == "" || token == "" {
		t.Skipf("HW_REAL_CLAUDE and %s name the pinned claude and a token", envClaudeToken)
	}
	if out, err := exec.Command(bin, "--version").Output(); err != nil || !strings.HasPrefix(string(out), pinned(claudecode.Name)+" ") {
		t.Fatalf("%s is claude %q, %v: the profile pins %s", bin, out, err, pinned(claudecode.Name))
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, claudecode.BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", claudecode.HookPath(root), "github.com/olesho/harness-wrapper/cmd/claude-code-hook").CombinedOutput(); err != nil {
		t.Fatalf("building the hook helper: %v\n%s", err, out)
	}
	run(t, fixture{
		name: claudecode.Name, adapter: adapter.New(claudecode.Profile{}), root: root,
		spec: contract.AgentSpec{Model: cmp.Or(os.Getenv(envClaudeModel), "haiku")},
		credential: func(t *testing.T, l contract.Layout) *contract.CredentialFile {
			return stage(t, token, filepath.Join(l.Secrets, claudecode.CredentialKind), claudecode.CredentialKind)
		},
	}, &report{})
}

// stage copies the credential at from into the layout's secrets, at to.
func stage(t *testing.T, from, to, kind string) *contract.CredentialFile {
	t.Helper()
	b, err := os.ReadFile(from) //nolint:gosec // the credential's path, from the environment
	if err != nil {
		t.Fatalf("the credential: %v", err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return &contract.CredentialFile{Kind: kind, File: to}
}
