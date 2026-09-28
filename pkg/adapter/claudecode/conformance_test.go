package claudecode

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
	pin, _ := versions.Pinned(Name)
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
	if err := os.Symlink(claudeBin, BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	// HW_CLAUDE_CODE_HOOK names a built hook helper, for a host without the
	// go tool; otherwise it is built from this tree.
	if hook := os.Getenv("HW_CLAUDE_CODE_HOOK"); hook != "" {
		if err := os.Symlink(hook, HookPath(root)); err != nil {
			t.Fatal(err)
		}
		return root
	}
	build := exec.Command("go", "build", "-o", HookPath(root), "github.com/olesho/harness-wrapper/cmd/claude-code-hook")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the hook helper: %v\n%s", err, out)
	}
	return root
}

// pointAt rewrites a provisioned open_config to reach the mock: its URL, and
// few retries, so an exhausted error ends in seconds.
func pointAt(mock *mockapi.Server) func(conformance.T, contract.Layout, *contract.ProvisionResult) {
	return func(t conformance.T, _ contract.Layout, r *contract.ProvisionResult) {
		var cfg openConfig
		if err := json.Unmarshal(r.OpenConfig, &cfg); err != nil {
			t.Errorf("open_config: %v", err)
			return
		}
		cfg.Env = append(cfg.Env, "ANTHROPIC_BASE_URL="+mock.URL(), "CLAUDE_CODE_MAX_RETRIES=2")
		r.OpenConfig, _ = json.Marshal(cfg)
	}
}

// The Claude Code profile passes the conformance kit against the pinned claude
// and the mock Messages API: every scenario, with a real claude process, its
// transcript and its hooks.
func TestClaudeConforms(t *testing.T) {
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	root := distribution(t, bin)
	conformance.Run(conformance.Testing(t), conformance.Fixture{
		Adapter:     adapter.New(Profile{}),
		HarnessRoot: root,
		Spec: contract.AgentSpec{
			Instructions:      contract.Instructions{Persona: "PERSONA-MARKER", Workspace: "# Workspace\n"},
			Skills:            []contract.Skill{{Name: "review", Files: []contract.SkillFile{{Path: "SKILL.md", Content: "---\nname: review\ndescription: Review code.\n---\n# Review\n"}}}},
			Memory:            &contract.Memory{Files: []contract.MemoryFile{{Path: "notes.md", Content: "remember\n"}}},
			PermissionPosture: contract.PostureBypass,
		},
		Credential: func(t conformance.T, l contract.Layout) *contract.CredentialFile {
			f := filepath.Join(l.Secrets, CredentialKind)
			if err := os.WriteFile(f, []byte("mock-placeholder-not-a-credential\n"), 0o600); err != nil {
				t.Errorf("staging the credential: %v", err)
			}
			return &contract.CredentialFile{Kind: CredentialKind, File: f}
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
			if err := os.Rename(BinaryPath(root), BinaryPath(root)+".hidden"); err != nil {
				t.Errorf("hiding the binary: %v", err)
			}
			return func() { _ = os.Rename(BinaryPath(root)+".hidden", BinaryPath(root)) }
		},
		Timeout: 90 * time.Second,
	})
	var persona bool
	for _, r := range mock.Requests() {
		persona = persona || strings.Contains(r.System, "PERSONA-MARKER")
	}
	if !persona {
		t.Error("the persona never reached the model's system prompt")
	}
}
