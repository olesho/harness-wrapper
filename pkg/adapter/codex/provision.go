package codex

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Limits on what one spec renders.
const (
	MaxPersona     = 64 << 10
	MaxSkillFile   = 256 << 10
	MaxSkillFiles  = 64
	MaxMemoryFile  = 256 << 10
	MaxProfileSize = 16 << 20 // every file together
)

// Files the profile renders, relative to their roots. The config root is
// CODEX_HOME.
const (
	configFile    = "config.toml"
	agentsFile    = "AGENTS.md" // CODEX_HOME's: the persona; the workspace's: its map
	skillsDir     = "skills"
	memoryDir     = "memory"
	sessionsDir   = "sessions" // where codex writes its rollouts
	authFile      = "auth.json"
	workspaceFile = "AGENTS.md"
)

// openConfig is the profile's open_config: everything Start launches codex
// with, rendered from the spec and the layout.
type openConfig struct {
	Binary string `json:"binary"`
	// Env is codex's environment beyond the Host's PATH, LANG, LC_* and TZ,
	// and the credential.
	Env        []string `json:"env"`
	WorkingDir string   `json:"working_dir"`
	// CodexHome is CODEX_HOME: the configuration root, where codex writes
	// the thread's rollout.
	CodexHome string `json:"codex_home"`
}

func parseOpenConfig(raw []byte) (openConfig, error) {
	var cfg openConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("open_config: %w", err)
	}
	if cfg.Binary == "" || cfg.WorkingDir == "" || cfg.CodexHome == "" {
		return cfg, fmt.Errorf("open_config: binary, working_dir and codex_home are required")
	}
	return cfg, nil
}

// Provision renders codex's configuration for the spec: config.toml — the
// model and effort, no approvals and no sandbox of codex's own (the
// runtime's isolation is the sandbox), a credential store in memory only, no
// plugins, apps or analytics, and a server per connector — AGENTS.md in
// CODEX_HOME with the persona and where the memory is, the skills, the
// memory's files, and the workspace's AGENTS.md.
func (Profile) Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error) {
	spec, l := req.Spec, req.Layout
	invalid := func(field, format string, args ...any) error {
		return &contract.Error{Code: contract.CodeInvalidSpec, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	if len(spec.Instructions.Persona) > MaxPersona {
		return contract.ProvisionResult{}, invalid("instructions.persona", "%d bytes, more than %d", len(spec.Instructions.Persona), MaxPersona)
	}
	files := []contract.File{
		contract.TextFile(contract.RootConfig, configFile, "0600", configTOML(spec)),
		// Always written, empty or not, so a spec without a persona replaces
		// one that had it.
		contract.TextFile(contract.RootConfig, agentsFile, "0600", agentsMD(spec, l)),
	}
	for i, sk := range spec.Skills {
		if len(sk.Files) > MaxSkillFiles {
			return contract.ProvisionResult{}, invalid(fmt.Sprintf("skills[%d].files", i), "%d files, more than %d", len(sk.Files), MaxSkillFiles)
		}
		for j, f := range sk.Files {
			if len(f.Content) > MaxSkillFile {
				return contract.ProvisionResult{}, invalid(fmt.Sprintf("skills[%d].files[%d]", i, j), "%d bytes, more than %d", len(f.Content), MaxSkillFile)
			}
			files = append(files, contract.TextFile(contract.RootConfig, path.Join(skillsDir, sk.Name, f.Path), "0600", f.Content))
		}
	}
	if spec.Memory != nil {
		for j, f := range spec.Memory.Files {
			if len(f.Content) > MaxMemoryFile {
				return contract.ProvisionResult{}, invalid(fmt.Sprintf("memory.files[%d]", j), "%d bytes, more than %d", len(f.Content), MaxMemoryFile)
			}
			files = append(files, contract.TextFile(contract.RootConfig, path.Join(memoryDir, f.Path), "0600", f.Content))
		}
	}
	if spec.Instructions.Workspace != "" {
		files = append(files, contract.TextFile(contract.RootWorkspace, workspaceFile, "0644", spec.Instructions.Workspace))
	}
	total := 0
	for _, f := range files {
		total += len(f.Content())
	}
	if total > MaxProfileSize {
		return contract.ProvisionResult{}, invalid("", "%d bytes of files, more than %d", total, MaxProfileSize)
	}
	cfg := openConfig{
		Binary: BinaryPath(req.HarnessRoot),
		Env: []string{
			"HOME=" + l.Home,
			"USER=" + filepath.Base(l.Home),
			"CODEX_HOME=" + l.Config,
		},
		WorkingDir: l.Workspace,
		CodexHome:  l.Config,
	}
	oc, err := json.Marshal(cfg)
	if err != nil {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
	return contract.ProvisionResult{
		Files:      files,
		OpenConfig: oc,
		// A thread is its rollout, and what codex keeps of it elsewhere: its
		// name in the index, its goal in the goals database and that
		// database's write-ahead log — codex can exit with the goal's row
		// still in the log alone — and the profile's own account of both
		// (native.go). A thread resumes by its id whatever its working
		// directory, so a loaded one needs no relocation.
		HistoryRoots: []contract.RootPath{
			{Root: contract.RootConfig, Path: sessionsDir},
			{Root: contract.RootConfig, Path: memoryDir},
			{Root: contract.RootConfig, Path: indexFile},
			{Root: contract.RootConfig, Path: goalsFile},
			{Root: contract.RootConfig, Path: goalsLogFile},
			{Root: contract.RootScratch, Path: nativeDir},
		},
		SecretPaths: []contract.RootPath{{Root: contract.RootConfig, Path: authFile}},
	}, nil
}

// configTOML is config.toml: its top-level keys first, then its tables, so a
// key added at the top stays top-level.
func configTOML(spec contract.AgentSpec) string {
	var b strings.Builder
	b.WriteString("# Rendered by harness-wrapper's Codex profile.\n")
	if spec.Model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlString(spec.Model))
	}
	if spec.Effort != "" {
		fmt.Fprintf(&b, "model_reasoning_effort = %s\n", tomlString(spec.Effort))
	}
	// The permission posture is bypass: isolation is the runtime's.
	b.WriteString(`approval_policy = "never"
sandbox_mode = "danger-full-access"
`)
	if spec.Credential != nil && spec.Credential.Kind == CredentialLogin {
		b.WriteString(`# A lent ChatGPT login: the Host writes it to auth.json at every launch.
cli_auth_credentials_store = "file"
`)
	} else {
		b.WriteString(`# The credential comes from the Host at every launch; codex writes none down.
cli_auth_credentials_store = "ephemeral"
`)
	}
	b.WriteString(`
[features]
plugins = false
remote_plugin = false
apps = false

[analytics]
enabled = false
`)
	for _, c := range spec.Connectors {
		fmt.Fprintf(&b, "\n[mcp_servers.%s]\n", tomlString(c.Name))
		switch {
		case c.Stdio != nil:
			fmt.Fprintf(&b, "command = %s\n", tomlString(c.Stdio.Command))
			if len(c.Stdio.Args) > 0 {
				fmt.Fprintf(&b, "args = %s\n", tomlStrings(c.Stdio.Args))
			}
			if len(c.Stdio.Env) > 0 {
				fmt.Fprintf(&b, "env = %s\n", tomlTable(c.Stdio.Env))
			}
		case c.HTTP != nil:
			fmt.Fprintf(&b, "url = %s\n", tomlString(c.HTTP.URL))
			if len(c.HTTP.Headers) > 0 {
				fmt.Fprintf(&b, "http_headers = %s\n", tomlTable(c.HTTP.Headers))
			}
			// headers_env names the variable each header's value is read from.
			if len(c.HTTP.HeadersEnv) > 0 {
				fmt.Fprintf(&b, "env_http_headers = %s\n", tomlTable(c.HTTP.HeadersEnv))
			}
		}
	}
	return b.String()
}

// agentsMD is CODEX_HOME's AGENTS.md, which codex adds to every turn's
// instructions: the persona, and where the agent keeps its memory.
func agentsMD(spec contract.AgentSpec, l contract.Layout) string {
	var parts []string
	if p := strings.TrimSpace(spec.Instructions.Persona); p != "" {
		parts = append(parts, p)
	}
	if spec.Memory != nil {
		parts = append(parts, "## Memory\n\nYour memory is the directory "+filepath.Join(l.Config, memoryDir)+
			": read what is there when you start, and keep what you learn there, a file per topic.")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// tomlString is s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// tomlTable is m as an inline table, its keys in order.
func tomlTable(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kv := make([]string, len(keys))
	for i, k := range keys {
		kv[i] = tomlString(k) + " = " + tomlString(m[k])
	}
	return "{ " + strings.Join(kv, ", ") + " }"
}
