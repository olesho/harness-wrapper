package pi

import (
	"encoding/json"
	"errors"
	"slices"
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
		Model: "anthropic/claude-haiku-4-5", Effort: "high",
		Instructions: contract.Instructions{Persona: "You are careful.", Workspace: "# Workspace\n"},
		Skills:       []contract.Skill{{Name: "review", Files: []contract.SkillFile{{Path: "SKILL.md", Content: "# Review\n"}}}},
		Memory:       &contract.Memory{Files: []contract.MemoryFile{{Path: "notes.md", Content: "hi"}}},
		Connectors: []contract.Connector{
			{Name: "probe", Stdio: &contract.StdioConnector{Command: "/bin/probe", Args: []string{"-v"}, Env: map[string]string{"K": "v"}}},
			{Name: "docs", HTTP: &contract.HTTPConnector{URL: "https://mcp.example.com", Headers: map[string]string{"X-Team": "t"}, HeadersEnv: map[string]string{"Authorization": "DOCS_TOKEN"}}},
			{Name: "kept", HTTP: &contract.HTTPConnector{URL: "https://kept.example.com", HeadersFile: map[string]string{"Authorization": "/w/secrets/kept auth"}}},
		},
		PermissionPosture: contract.PostureBypass,
		Credential:        &contract.CredentialRef{Kind: CredentialKind},
	}
}

func provision(t *testing.T, s contract.AgentSpec) (map[string]string, openConfig, contract.ProvisionResult) {
	t.Helper()
	res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/pi", Layout: layout(), Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range res.Files {
		files[string(f.Root)+"/"+f.Path] = string(f.Content())
	}
	cfg, err := parseOpenConfig(res.OpenConfig)
	if err != nil {
		t.Fatal(err)
	}
	return files, cfg, res
}

// Provision renders the agent dir — settings.json, APPEND_SYSTEM.md with the
// persona and the memory, mcp.json, the skills and memory files — the
// workspace's AGENTS.md, and open_config.
func TestProvision(t *testing.T) {
	files, cfg, res := provision(t, spec())
	var settings struct {
		Retry struct {
			Enabled    bool `json:"enabled"`
			MaxRetries int  `json:"maxRetries"`
		} `json:"retry"`
		CacheWarming string `json:"cacheWarming"`
		Telemetry    *bool  `json:"enableInstallTelemetry"`
	}
	if err := json.Unmarshal([]byte(files["config/settings.json"]), &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.Retry.Enabled || settings.Retry.MaxRetries != maxRetries || settings.CacheWarming != "off" || settings.Telemetry == nil || *settings.Telemetry {
		t.Errorf("settings.json: %s", files["config/settings.json"])
	}
	if a := files["config/APPEND_SYSTEM.md"]; !strings.Contains(a, "You are careful.") || !strings.Contains(a, "/w/config/memory") {
		t.Errorf("APPEND_SYSTEM.md: %q", a)
	}
	var mcp struct {
		Servers map[string]struct {
			Exposure string            `json:"exposure"`
			Command  string            `json:"command"`
			Args     []string          `json:"args"`
			Env      map[string]string `json:"env"`
			URL      string            `json:"url"`
			Headers  map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(files["config/mcp.json"]), &mcp); err != nil {
		t.Fatal(err)
	}
	probe, docs, kept := mcp.Servers["probe"], mcp.Servers["docs"], mcp.Servers["kept"]
	if probe.Exposure != "direct" || probe.Command != "/bin/probe" || !slices.Equal(probe.Args, []string{"-v"}) || probe.Env["K"] != "v" {
		t.Errorf("stdio server: %+v", probe)
	}
	if docs.URL != "https://mcp.example.com" || docs.Headers["X-Team"] != "t" || docs.Headers["Authorization"] != "${DOCS_TOKEN}" {
		t.Errorf("http server: %+v", docs)
	}
	if kept.Headers["Authorization"] != `!cat '/w/secrets/kept auth'` {
		t.Errorf("a header from a file: %+v", kept)
	}
	for _, f := range []string{"config/skills/review/SKILL.md", "config/memory/notes.md"} {
		if _, ok := files[f]; !ok {
			t.Errorf("no %s", f)
		}
	}
	if files["workspace/AGENTS.md"] != "# Workspace\n" {
		t.Errorf("the workspace's AGENTS.md: %q", files["workspace/AGENTS.md"])
	}
	if cfg.Binary != "/opt/pi/bin/pi" || cfg.Extension != "/opt/pi/hw-tag.ts" || cfg.WorkingDir != "/w/ws" ||
		cfg.AgentDir != "/w/config" || cfg.SessionDir != "/w/config/sessions" || cfg.Provider != "anthropic" || cfg.MaxRetries != maxRetries {
		t.Errorf("open_config: %+v", cfg)
	}
	if !slices.Equal(cfg.Args, []string{"--model", "anthropic/claude-haiku-4-5", "--thinking", "high"}) {
		t.Errorf("args: %v", cfg.Args)
	}
	for _, want := range []string{"HOME=/w/home", "PI_CODING_AGENT_DIR=/w/config", "PI_OFFLINE=1", "PI_TELEMETRY=0"} {
		if !slices.Contains(cfg.Env, want) {
			t.Errorf("env has no %s: %v", want, cfg.Env)
		}
	}
	if !slices.Contains(res.SecretPaths, contract.RootPath{Root: contract.RootConfig, Path: authFile}) {
		t.Errorf("secret paths: %v", res.SecretPaths)
	}
	if !slices.Contains(res.HistoryRoots, contract.RootPath{Root: contract.RootConfig, Path: sessionsDir}) {
		t.Errorf("history roots: %v", res.HistoryRoots)
	}
}

// Without connectors pi loads no MCP configuration at all, and without a
// persona or memory APPEND_SYSTEM.md is still written, empty, so it
// replaces one an earlier spec had.
func TestProvisionBare(t *testing.T) {
	s := spec()
	s.Connectors, s.Memory, s.Instructions, s.Effort = nil, nil, contract.Instructions{}, ""
	files, cfg, _ := provision(t, s)
	if _, ok := files["config/mcp.json"]; ok {
		t.Error("mcp.json with no connector")
	}
	if a, ok := files["config/APPEND_SYSTEM.md"]; !ok || a != "" {
		t.Errorf("APPEND_SYSTEM.md: %q, %v", a, ok)
	}
	if !slices.Equal(cfg.Args, []string{"--model", "anthropic/claude-haiku-4-5", "--no-mcp"}) {
		t.Errorf("args: %v", cfg.Args)
	}
}

// A model must name a provider the profile serves: the key goes to that
// provider's hosts alone.
func TestProvisionRefusesModel(t *testing.T) {
	for _, m := range []string{"claude-haiku-4-5", "nonesuch/model", "anthropic/", "amazon-bedrock/anthropic.claude"} {
		s := spec()
		s.Model = m
		_, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/pi", Layout: layout(), Spec: s})
		var ce *contract.Error
		if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidSpec || ce.Field != "model" {
			t.Errorf("model %q: %v, want invalid_spec on model", m, err)
		}
	}
}
