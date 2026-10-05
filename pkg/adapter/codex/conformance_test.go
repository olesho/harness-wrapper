package codex

import (
	"fmt"
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

// realCodex is the codex binary HW_REAL_CODEX names — the native one, which
// must be the pinned version; the test skips without one.
func realCodex(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HW_REAL_CODEX")
	if bin == "" {
		t.Skip("HW_REAL_CODEX does not name a codex binary")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	pin, _ := versions.Pinned(Name)
	if got := strings.Fields(string(out)); len(got) == 0 || got[len(got)-1] != pin {
		t.Fatalf("%s is %q; the profile is verified against codex %s", bin, strings.TrimSpace(string(out)), pin)
	}
	return bin
}

// distribution lays out a harness distribution under a temporary root: the
// codex binary.
func distribution(t *testing.T, codexBin string) string {
	t.Helper()
	return distributionAt(t, codexBin, t.TempDir())
}

// distributionAt lays the distribution out under root.
func distributionAt(t *testing.T, codexBin, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(codexBin, BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	return root
}

// pointAt rewrites a provisioned config.toml to reach the mock: a model
// provider of its own, which needs no OpenAI login, retrying a failed
// response as often as the mock's retry budget says.
func pointAt(mock *mockapi.Server) func(conformance.T, contract.Layout, *contract.ProvisionResult) {
	return func(t conformance.T, _ contract.Layout, r *contract.ProvisionResult) {
		for i, f := range r.Files {
			if f.Root != contract.RootConfig || f.Path != configFile {
				continue
			}
			provider := fmt.Sprintf(`
[model_providers.mock]
name = "mock"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = %d
stream_idle_timeout_ms = 120000
`, mock.URL()+"/v1", mock.RetryBudget)
			r.Files[i] = contract.TextFile(f.Root, f.Path, f.Mode, "model_provider = \"mock\"\n"+string(f.Content())+provider)
			return
		}
		t.Errorf("no %s rendered", configFile)
	}
}

// heard is the last request the mock answered for a turn of the kit's: the
// conversation codex sent the model.
func heard(mock *mockapi.Server) func(conformance.T, contract.Session) string {
	return func(conformance.T, contract.Session) string {
		reqs := mock.Requests()
		for i := len(reqs) - 1; i >= 0; i-- {
			if strings.HasPrefix(reqs[i].Scenario, "PING") {
				return string(reqs[i].Body)
			}
		}
		return ""
	}
}

// kitFixture is the conformance kit's fixture for the profile: the pinned
// codex in the distribution at root, driving the mock.
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
			f := filepath.Join(l.Secrets, CredentialAPIKey)
			if err := os.WriteFile(f, []byte("sk-mock-placeholder-not-a-credential\n"), 0o600); err != nil {
				t.Errorf("staging the credential: %v", err)
			}
			return &contract.CredentialFile{Kind: CredentialAPIKey, File: f}
		},
		Provisioned: pointAt(mock),
		Kill: func(t conformance.T, s contract.Session) {
			tr, ok := adapter.TransportOf(s).(*transport)
			if !ok {
				t.Errorf("no codex to kill")
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
		Heard:   heard(mock),
		Timeout: 90 * time.Second,
		MCP:     true,
	}
}

// The Codex profile passes the conformance kit against the pinned codex and
// the mock Responses API: every scenario, with a real codex app-server, its
// rollout and its retries.
func TestCodexConforms(t *testing.T) {
	bin := realCodex(t)
	mock := mockapi.Start()
	mock.KeepBodies = true
	defer mock.Close()
	root := distribution(t, bin)
	conformance.Run(conformance.Testing(t), kitFixture(root, mock))
	var persona, workspace bool
	for _, r := range mock.Requests() {
		persona = persona || strings.Contains(r.Input, "PERSONA-MARKER")
		workspace = workspace || strings.Contains(r.Input, "# Workspace")
	}
	if !persona || !workspace {
		t.Errorf("the model saw the persona: %v, the workspace's AGENTS.md: %v", persona, workspace)
	}
}
