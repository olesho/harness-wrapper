package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
			{Name: "kept", HTTP: &contract.HTTPConnector{URL: "https://kept.example.com", HeadersFile: map[string]string{"Authorization": "/w/secrets/kept-auth", "X-Key": "/w/secrets/kept-key"}}},
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
		`[mcp_servers."kept"]`, `env_http_headers = { "Authorization" = "HW_MCP_HEADER_1", "X-Key" = "HW_MCP_HEADER_2" }`,
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
	var roots []string
	for _, h := range res.HistoryRoots {
		roots = append(roots, h.String())
	}
	// The rollouts and the memory, and what codex keeps of a thread outside
	// its rollout: its name, its goal with the goal's log, and the profile's
	// account of both.
	want := "config/sessions config/memory config/session_index.jsonl config/goals_1.sqlite config/goals_1.sqlite-wal scratch/native"
	if len(res.SecretPaths) != 1 || res.SecretPaths[0].Path != authFile || strings.Join(roots, " ") != want {
		t.Errorf("secret paths %+v, history roots %v, want %s", res.SecretPaths, roots, want)
	}
	if len(res.HistoryRelocations) != 0 {
		t.Errorf("history relocations %+v, want none", res.HistoryRelocations)
	}
}

// A request that loads a saved thread relocates nothing: a thread resumes by
// its id wherever its rollout's directory is. One codex of another version
// saved is refused.
func TestProvisionLoadsAThread(t *testing.T) {
	a := adapter.New(Profile{})
	d := a.Describe()
	if !d.Has(contract.CapSessionLoad) || !d.Has(contract.CapAutonomousTurns) || !d.Loads(adapter.ArchiveFormat, d.Harness.Version) {
		t.Fatalf("the profile does not load its own version's threads: %v %+v", d.Capabilities, d.Load)
	}
	l := contract.Layout{Home: "/w/home", Config: "/w/config", Workspace: "/w/ws", Secrets: "/w/secrets", Scratch: "/w/scratch"}
	src := contract.Layout{Home: "/old/home", Config: "/old/config", Workspace: "/old/ws", Secrets: "/old/secrets", Scratch: "/old/scratch"}
	req := contract.ProvisionRequest{
		Contract: contract.Version, HarnessRoot: "/opt/codex", Layout: l,
		Spec: contract.AgentSpec{PermissionPosture: contract.PostureBypass},
		Load: &contract.LoadSource{Format: adapter.ArchiveFormat, Harness: d.Harness, Layout: src, Workspace: "/old/ws"},
	}
	res, err := a.Provision(req)
	if err != nil || len(res.HistoryRelocations) != 0 {
		t.Fatalf("Provision = %+v %v, want no relocation", res.HistoryRelocations, err)
	}
	req.Load.Harness.Version = "0.1.0"
	if _, err := a.Provision(req); contract.CodeOf(err) != contract.CodeUnsupported {
		t.Errorf("a thread another codex saved: %v, want unsupported", err)
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

// A header of headers_file is read from its file into the variable
// config.toml names for it, for codex's environment alone; its value is in
// no file Provision renders.
func TestHeadersFromFiles(t *testing.T) {
	res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/codex", Layout: layout(), Spec: spec()})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseOpenConfig(res.OpenConfig)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"HW_MCP_HEADER_1": "/w/secrets/kept-auth", "HW_MCP_HEADER_2": "/w/secrets/kept-key"}; !reflect.DeepEqual(cfg.HeaderFiles, want) {
		t.Fatalf("the open config's header files %v, want %v", cfg.HeaderFiles, want)
	}
	dir := t.TempDir()
	auth, key := filepath.Join(dir, "auth"), filepath.Join(dir, "key")
	if err := os.WriteFile(auth, []byte("Bearer v-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("k=2"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := headerEnv(map[string]string{"HW_MCP_HEADER_1": auth, "HW_MCP_HEADER_2": key})
	if err != nil || !reflect.DeepEqual(env, []string{"HW_MCP_HEADER_1=Bearer v-1", "HW_MCP_HEADER_2=k=2"}) {
		t.Errorf("headerEnv: %q %v", env, err)
	}
	if _, err := headerEnv(map[string]string{"HW_MCP_HEADER_1": filepath.Join(dir, "gone")}); err == nil {
		t.Error("a header's file gone, and no error")
	}
}
