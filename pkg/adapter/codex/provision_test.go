package codex

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

func layout() contract.Layout {
	return contract.Layout{Home: "/w/home", Config: "/w/config", Workspace: "/w/ws", Secrets: "/w/secrets", Scratch: "/w/scratch"}
}

func spec() contract.AgentSpec {
	return contract.AgentSpec{
		Model: "gpt-5.1-codex", Effort: "high",
		Instructions: contract.Instructions{Persona: "You are careful.", Workspace: "# Workspace\n"},
		Skills:       []contract.Skill{{Name: "review", Files: []contract.SkillFile{{Path: "SKILL.md", Content: "# Review\n"}}}},
		Memory:       &contract.Memory{Files: []contract.MemoryFile{{Path: "notes.md", Content: "hi"}}},
		Connectors: []contract.Connector{
			{Name: "probe", Stdio: &contract.StdioConnector{Command: "/bin/probe", Args: []string{"-v", `say "hi"`}, Env: map[string]string{"K": "v"}}},
			{Name: "docs", HTTP: &contract.HTTPConnector{URL: "https://mcp.example.com", Headers: map[string]string{"X-Team": "t"}, HeadersEnv: map[string]string{"Authorization": "DOCS_TOKEN"}}},
		},
		PermissionPosture: contract.PostureBypass,
		Credential:        &contract.CredentialRef{Kind: CredentialAPIKey},
	}
}

// Provision renders CODEX_HOME — config.toml, AGENTS.md with the persona and
// the memory, the skills and memory files — the workspace's AGENTS.md, and
// open_config.
func TestProvision(t *testing.T) {
	res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/codex", Layout: layout(), Spec: spec()})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range res.Files {
		files[string(f.Root)+"/"+f.Path] = string(f.Content())
	}
	toml := files["config/config.toml"]
	for _, want := range []string{
		`model = "gpt-5.1-codex"`, `model_reasoning_effort = "high"`, `approval_policy = "never"`,
		`sandbox_mode = "danger-full-access"`, `cli_auth_credentials_store = "ephemeral"`, "plugins = false",
		`[mcp_servers."probe"]`, `command = "/bin/probe"`, `args = ["-v", "say \"hi\""]`, `env = { "K" = "v" }`,
		`[mcp_servers."docs"]`, `url = "https://mcp.example.com"`, `http_headers = { "X-Team" = "t" }`,
		`env_http_headers = { "Authorization" = "DOCS_TOKEN" }`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("config.toml has no %s:\n%s", want, toml)
		}
	}
	// Top-level keys come before every table, so a key added at the top
	// stays one.
	if i, j := strings.Index(toml, "cli_auth_credentials_store"), strings.Index(toml, "["); i < 0 || j < i {
		t.Errorf("a table before the top-level keys:\n%s", toml)
	}
	if a := files["config/AGENTS.md"]; !strings.Contains(a, "You are careful.") || !strings.Contains(a, "/w/config/memory") {
		t.Errorf("CODEX_HOME's AGENTS.md:\n%s", a)
	}
	if files["config/skills/review/SKILL.md"] != "# Review\n" || files["config/memory/notes.md"] != "hi" || files["workspace/AGENTS.md"] != "# Workspace\n" {
		t.Errorf("files %v", files)
	}
	var cfg openConfig
	if err := json.Unmarshal(res.OpenConfig, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != "/opt/codex/bin/codex" || cfg.WorkingDir != "/w/ws" || cfg.CodexHome != "/w/config" || strings.Join(cfg.Env, " ") != "HOME=/w/home USER=home CODEX_HOME=/w/config" {
		t.Errorf("open_config %+v", cfg)
	}
	if len(res.SecretPaths) != 1 || res.SecretPaths[0].Path != authFile || len(res.HistoryRoots) != 2 {
		t.Errorf("secret paths %+v, history roots %+v", res.SecretPaths, res.HistoryRoots)
	}
}

// A spec with no persona and no memory still replaces CODEX_HOME's
// AGENTS.md, empty.
func TestProvisionBare(t *testing.T) {
	res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{
		Contract: contract.Version, HarnessRoot: "/opt/codex", Layout: layout(),
		Spec: contract.AgentSpec{PermissionPosture: contract.PostureBypass},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Files {
		if f.Path == agentsFile && f.Root == contract.RootConfig && len(f.Content()) != 0 {
			t.Errorf("AGENTS.md %q", f.Content())
		}
		if strings.Contains(string(f.Content()), "model =") {
			t.Errorf("a model with none asked: %s", f.Content())
		}
	}
}

func TestProvisionRefuses(t *testing.T) {
	for name, mut := range map[string]func(*contract.AgentSpec){
		"persona too big": func(s *contract.AgentSpec) { s.Instructions.Persona = strings.Repeat("x", MaxPersona+1) },
		"skill file big":  func(s *contract.AgentSpec) { s.Skills[0].Files[0].Content = strings.Repeat("x", MaxSkillFile+1) },
		"memory big":      func(s *contract.AgentSpec) { s.Memory.Files[0].Content = strings.Repeat("x", MaxMemoryFile+1) },
		"effort unknown":  func(s *contract.AgentSpec) { s.Effort = "extreme" },
		"gated posture":   func(s *contract.AgentSpec) { s.PermissionPosture = contract.PostureGated },
	} {
		t.Run(name, func(t *testing.T) {
			s := spec()
			mut(&s)
			_, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/codex", Layout: layout(), Spec: s})
			var ce *contract.Error
			if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidSpec && ce.Code != contract.CodeUnsupported {
				t.Errorf("Provision = %v", err)
			}
		})
	}
}

// A TOML string escapes what would end or break it.
func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:      `"plain"`,
		`a "b" \c`:   `"a \"b\" \\c"`,
		"line\nnext": `"line\nnext"`,
		"bell\a":     `"bell\u0007"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
}
