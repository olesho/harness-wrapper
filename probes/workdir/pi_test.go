package workdir

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/pi"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Pi's live run reads, beside HW_REAL_PI (the pinned pi of an unpacked
// release, its package.json beside it):
const (
	// envPiKey names a file holding the model's provider's API key.
	envPiKey = "HW_WORKDIR_PI_KEY_FILE"
	// envPiModel names the model, <provider>/<id>; an OpenRouter haiku
	// when unset.
	envPiModel = "HW_WORKDIR_PI_MODEL"
)

// The pinned Pi works in a directory of its own (session_workdir), with a
// real account: one Session at a time, as Pi runs.
func TestPiWorkDirsLive(t *testing.T) {
	bin, key := os.Getenv("HW_REAL_PI"), os.Getenv(envPiKey)
	if bin == "" || key == "" {
		t.Skipf("HW_REAL_PI and %s name the pinned pi and a key", envPiKey)
	}
	if out, err := exec.Command(bin, "--version").Output(); err != nil || strings.TrimSpace(string(out)) != pinned(pi.Name) {
		t.Fatalf("%s is pi %q, %v: the profile pins %s", bin, out, err, pinned(pi.Name))
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pi", "package.json"} {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(bin), name)) //nolint:gosec // the release's files, from the environment
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "bin", name), b, 0o755); err != nil { //nolint:gosec // an executable
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(pi.ExtensionPath(root), pi.Extension, 0o644); err != nil { //nolint:gosec // read by pi
		t.Fatal(err)
	}
	run(t, fixture{
		name: pi.Name, adapter: adapter.New(pi.Profile{}), root: root,
		spec: contract.AgentSpec{Model: cmp.Or(os.Getenv(envPiModel), "openrouter/anthropic/claude-haiku-4.5")},
		credential: func(t *testing.T, l contract.Layout) *contract.CredentialFile {
			return stage(t, key, filepath.Join(l.Secrets, pi.CredentialKind), pi.CredentialKind)
		},
	}, &report{})
}
