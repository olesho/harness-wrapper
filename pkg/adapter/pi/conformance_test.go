package pi

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

// realPi is the pi HW_REAL_PI names — the executable of an unpacked release,
// its package.json beside it (probes/pirpc/fetch.sh), which must be the
// pinned version; the test skips without one.
func realPi(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HW_REAL_PI")
	if bin == "" {
		t.Skip("HW_REAL_PI does not name a pi executable")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	pin, _ := versions.Pinned(Name)
	if got := strings.TrimSpace(string(out)); got != pin {
		t.Fatalf("%s is pi %q; the profile is verified against pi %s", bin, got, pin)
	}
	return bin
}

// distribution lays out a harness distribution under a temporary root, as a
// runtime installs it: copies of the pi executable and its package.json —
// all of a release that RPC mode needs (probes/pirpc) — and the tag
// extension.
func distribution(t *testing.T, piBin string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pi", "package.json"} {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(piBin), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "bin", name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ExtensionPath(root), Extension, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// pointAt points a provisioned agent dir at the mock: models.json re-points
// the providers' models at it, and settings.json retries sooner.
func pointAt(mock *mockapi.Server) func(conformance.T, contract.Layout, *contract.ProvisionResult) {
	return func(t conformance.T, _ contract.Layout, r *contract.ProvisionResult) {
		models, _ := json.Marshal(map[string]any{"providers": map[string]any{
			"anthropic": map[string]any{"baseUrl": mock.URL()},
			"openai":    map[string]any{"baseUrl": mock.URL() + "/v1"},
		}})
		r.Files = append(r.Files, contract.TextFile(contract.RootConfig, "models.json", "0600", string(models)))
		for i, f := range r.Files {
			if f.Root != contract.RootConfig || f.Path != settingsFile {
				continue
			}
			var s map[string]any
			if err := json.Unmarshal(f.Content(), &s); err != nil {
				t.Errorf("settings.json: %v", err)
				return
			}
			s["retry"].(map[string]any)["baseDelayMs"] = 200
			b, _ := json.Marshal(s)
			r.Files[i] = contract.TextFile(f.Root, f.Path, f.Mode, string(b))
			return
		}
		t.Errorf("no %s rendered", settingsFile)
	}
}

// heard is the last request the mock answered for a turn of the kit's: the
// conversation pi sent the model.
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

// kitFixture is the conformance kit's fixture for the profile: the pinned pi
// in the distribution at root, on model, driving the mock.
func kitFixture(root, model string, mock *mockapi.Server) conformance.Fixture {
	return conformance.Fixture{
		Adapter:     adapter.New(Profile{}),
		HarnessRoot: root,
		Spec: contract.AgentSpec{
			Model:             model,
			Instructions:      contract.Instructions{Persona: "PERSONA-MARKER", Workspace: "# Workspace\n"},
			Skills:            []contract.Skill{{Name: "review", Files: []contract.SkillFile{{Path: "SKILL.md", Content: "---\nname: review\ndescription: Review code.\n---\n# Review\n"}}}},
			Memory:            &contract.Memory{Files: []contract.MemoryFile{{Path: "notes.md", Content: "remember\n"}}},
			PermissionPosture: contract.PostureBypass,
		},
		Credential: func(t conformance.T, l contract.Layout) *contract.CredentialFile {
			f := filepath.Join(l.Secrets, CredentialKind)
			if err := os.WriteFile(f, []byte("sk-mock-placeholder-not-a-credential\n"), 0o600); err != nil {
				t.Errorf("staging the credential: %v", err)
			}
			return &contract.CredentialFile{Kind: CredentialKind, File: f}
		},
		Provisioned: pointAt(mock),
		Kill: func(t conformance.T, s contract.Session) {
			tr, ok := adapter.TransportOf(s).(*transport)
			if !ok {
				t.Errorf("no pi to kill")
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

// The Pi profile passes the conformance kit against the pinned pi and the
// mock: every scenario on OpenAI's Responses API, whose usage wall
// (usage_limit_reached) says when it resets.
func TestPiConforms(t *testing.T) {
	bin := realPi(t)
	mock := mockapi.Start()
	mock.KeepBodies = true
	// pi's last attempt at an ERR 529 is the one the mock answers overloaded.
	mock.RetryBudget = maxRetries
	defer mock.Close()
	conformance.Run(conformance.Testing(t), kitFixture(distribution(t, bin), "openai/gpt-4.1-mini", mock))
	var persona, workspace bool
	for _, r := range mock.Requests() {
		persona = persona || strings.Contains(r.System+r.Input, "PERSONA-MARKER")
		workspace = workspace || strings.Contains(r.System+r.Input, "# Workspace")
	}
	if !persona || !workspace {
		t.Errorf("the model saw the persona: %v, the workspace's AGENTS.md: %v", persona, workspace)
	}
}

// On Anthropic's Messages API too, but for its usage wall: under an API key
// a 429 is a rate limit, which pi retries and the profile reports as the
// API's own.
func TestPiConformsOnAnthropic(t *testing.T) {
	bin := realPi(t)
	mock := mockapi.Start()
	mock.KeepBodies = true
	// pi's last attempt at an ERR 529 is the one the mock answers overloaded.
	mock.RetryBudget = maxRetries
	defer mock.Close()
	f := kitFixture(distribution(t, bin), "anthropic/claude-haiku-4-5", mock)
	f.Skip = map[string]string{"usage-limit": "an API key meets no usage wall on Anthropic's API: its 429 is a rate limit"}
	conformance.Run(conformance.Testing(t), f)
}
