package claudecodetui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// realClaude is the claude binary HW_REAL_CLAUDE names, which must be the
// pinned version; the test skips without one.
func realClaude(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HW_REAL_CLAUDE")
	if bin == "" {
		t.Skip("HW_REAL_CLAUDE does not name a claude binary")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	pin, _ := versions.Pinned(claudecode.Name)
	if got := strings.Fields(string(out)); len(got) == 0 || got[0] != pin {
		t.Fatalf("%s is claude %q; the profile is verified against %s", bin, strings.TrimSpace(string(out)), pin)
	}
	return bin
}

// distribution lays out a harness distribution under a temporary root: the
// claude binary, and the hook helper built from this tree.
func distribution(t *testing.T, claudeBin string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(claudeBin, claudecode.BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	// HW_CLAUDE_CODE_HOOK names a built hook helper, for a host without the
	// go tool; otherwise it is built from this tree.
	if hook := os.Getenv("HW_CLAUDE_CODE_HOOK"); hook != "" {
		if err := os.Symlink(hook, claudecode.HookPath(root)); err != nil {
			t.Fatal(err)
		}
		return root
	}
	build := exec.Command("go", "build", "-o", claudecode.HookPath(root), "github.com/olesho/harness-wrapper/cmd/claude-code-hook")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the hook helper: %v\n%s", err, out)
	}
	return root
}

// pointAt rewrites a provisioned open_config to reach the mock: its URL, and
// few retries, so an exhausted error ends in seconds.
func pointAt(mock *mockapi.Server) func(conformance.T, contract.Layout, *contract.ProvisionResult) {
	return func(t conformance.T, _ contract.Layout, r *contract.ProvisionResult) {
		cfg, err := claudecode.ParseOpenConfig(r.OpenConfig)
		if err != nil {
			t.Errorf("open_config: %v", err)
			return
		}
		cfg.Env = append(cfg.Env, "ANTHROPIC_BASE_URL="+mock.URL(), "CLAUDE_CODE_MAX_RETRIES=2")
		r.OpenConfig, _ = json.Marshal(cfg)
	}
}

// The Claude Code TUI profile passes the conformance kit against the pinned
// claude and the mock Messages API: every scenario its Descriptor's
// capabilities ask for, with a real claude on a terminal, its hooks, its
// transcript and its debug log.
func TestClaudeTUIConforms(t *testing.T) {
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	conformance.Run(conformance.Testing(t), kitFixture(distribution(t, bin), mock))
	var persona bool
	for _, r := range mock.Requests() {
		persona = persona || strings.Contains(r.System, "PERSONA-MARKER")
	}
	if !persona {
		t.Error("the persona never reached the model's system prompt")
	}
}

// The version policy against a claude other than the pin, HW_NONPIN_CLAUDE
// (and the pin, HW_REAL_CLAUDE): flexible opens it and reports its version,
// with the debug log's [engine] line still required; strict refuses it with
// version_unsupported. Only the kit's version-policy scenario runs.
func TestClaudeTUIVersionPolicy(t *testing.T) {
	if os.Getenv("HW_NONPIN_CLAUDE") == "" {
		t.Skip("HW_NONPIN_CLAUDE does not name a claude other than the pin")
	}
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	f := kitFixture(distribution(t, bin), mock)
	f.Skip = conformance.Only("version-policy")
	conformance.Run(conformance.Testing(t), f)
}

// nonPinClaude makes the distribution's claude, at bin, the one
// HW_NONPIN_CLAUDE names — a claude other than the pin — until restored; nil
// without one.
func nonPinClaude(bin string) func(conformance.T) (string, func()) {
	other := os.Getenv("HW_NONPIN_CLAUDE")
	if other == "" {
		return nil
	}
	return func(t conformance.T) (string, func()) {
		out, err := exec.Command(other, "--version").Output()
		f := strings.Fields(string(out))
		if err != nil || len(f) == 0 {
			t.Errorf("%s --version: %v", other, err)
			return "", func() {}
		}
		pin, err := os.Readlink(bin)
		if err != nil {
			t.Errorf("the distribution's claude: %v", err)
			return f[0], func() {}
		}
		relink := func(to string) {
			_ = os.Remove(bin)
			if err := os.Symlink(to, bin); err != nil {
				t.Errorf("linking %s: %v", to, err)
			}
		}
		relink(other)
		return f[0], func() { relink(pin) }
	}
}

// kitFixture is the conformance kit's fixture for the profile: the pinned
// claude in the distribution at root, driving the mock.
func kitFixture(root string, mock *mockapi.Server) conformance.Fixture {
	return conformance.Fixture{
		Adapter:     adapter.New(Profile{}),
		HarnessRoot: root,
		Spec: contract.AgentSpec{
			Instructions:      contract.Instructions{Persona: "PERSONA-MARKER", Workspace: "# Workspace\n"},
			Skills:            []contract.Skill{{Name: "review", Files: []contract.SkillFile{{Path: "SKILL.md", Content: "---\nname: review\ndescription: Review code.\n---\n# Review\n"}}}},
			Memory:            &contract.Memory{Files: []contract.MemoryFile{{Path: "notes.md", Content: "remember\n"}}},
			PermissionPosture: contract.PostureBypass,
		},
		Credential: func(t conformance.T, l contract.Layout) *contract.CredentialFile {
			f := filepath.Join(l.Secrets, claudecode.CredentialKind)
			if err := os.WriteFile(f, []byte("mock-placeholder-not-a-credential\n"), 0o600); err != nil {
				t.Errorf("staging the credential: %v", err)
			}
			return &contract.CredentialFile{Kind: claudecode.CredentialKind, File: f}
		},
		Provisioned: pointAt(mock),
		Kill: func(t conformance.T, s contract.Session) {
			tr, ok := adapter.TransportOf(s).(*transport)
			if !ok {
				t.Errorf("no claude to kill")
				return
			}
			tr.kill()
			<-tr.exited
		},
		HideBinary: func(t conformance.T) func() {
			if err := os.Rename(claudecode.BinaryPath(root), claudecode.BinaryPath(root)+".hidden"); err != nil {
				t.Errorf("hiding the binary: %v", err)
			}
			return func() { _ = os.Rename(claudecode.BinaryPath(root)+".hidden", claudecode.BinaryPath(root)) }
		},
		Timeout: 90 * time.Second,
		MCP:     true,
		NonPin:  nonPinClaude(claudecode.BinaryPath(root)),
	}
}
