package pi

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/adapter"
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

// Files the profile renders, relative to their roots. The config root is the
// agent dir, PI_CODING_AGENT_DIR.
const (
	settingsFile  = "settings.json"
	mcpFile       = "mcp.json"
	appendFile    = "APPEND_SYSTEM.md" // the persona, after pi's own system prompt
	skillsDir     = "skills"
	memoryDir     = "memory"
	sessionsDir   = "sessions" // --session-dir: where pi writes the session's file
	authFile      = "auth.json"
	workspaceFile = "AGENTS.md"
)

// openConfig is the profile's open_config: everything Start launches pi
// with, rendered from the spec and the layout.
type openConfig struct {
	Binary    string `json:"binary"`
	Extension string `json:"extension"`
	// Env is pi's environment beyond the Host's PATH, LANG, LC_* and TZ.
	Env []string `json:"env"`
	// Args are pi's arguments beyond its mode, session and extension: the
	// model and thinking level, and --no-mcp with no connector.
	Args       []string `json:"args"`
	WorkingDir string   `json:"working_dir"`
	// AgentDir is PI_CODING_AGENT_DIR, the configuration root: auth.json is
	// written there at every launch. SessionDir is where pi writes the
	// session's file.
	AgentDir   string `json:"agent_dir"`
	SessionDir string `json:"session_dir"`
	// Provider is pi's name for the provider the model belongs to: the key
	// is written under it.
	Provider string `json:"provider"`
	// MaxRetries is how often pi retries a failed model call
	// (settings.json): after that many, a failure is the run's end.
	MaxRetries int `json:"max_retries"`
}

func parseOpenConfig(raw []byte) (openConfig, error) {
	var cfg openConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("open_config: %w", err)
	}
	if cfg.Binary == "" || cfg.Extension == "" || cfg.WorkingDir == "" || cfg.AgentDir == "" || cfg.SessionDir == "" || cfg.Provider == "" {
		return cfg, fmt.Errorf("open_config: binary, extension, working_dir, agent_dir, session_dir and provider are required")
	}
	return cfg, nil
}

// Provision renders pi's configuration for the spec, in the agent dir:
// settings.json (retries on, no cache warming, no install telemetry),
// mcp.json with a server per connector, its tools declared to the model,
// APPEND_SYSTEM.md with the persona and where the memory is, the skills, the
// memory's files; and the workspace's AGENTS.md. The model names its
// provider ("anthropic/claude-…"): one the profile serves, or the spec is
// refused, since the key goes to that provider alone.
func (Profile) Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error) {
	spec, l := req.Spec, req.Layout
	invalid := func(field, format string, args ...any) error {
		return &contract.Error{Code: contract.CodeInvalidSpec, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	name, _, _, ok := splitModel(spec.Model)
	if !ok {
		return contract.ProvisionResult{}, invalid("model", "%q: pi's models are <provider>/<id>, with a provider the profile serves", spec.Model)
	}
	if len(spec.Instructions.Persona) > MaxPersona {
		return contract.ProvisionResult{}, invalid("instructions.persona", "%d bytes, more than %d", len(spec.Instructions.Persona), MaxPersona)
	}
	files := []contract.File{
		contract.TextFile(contract.RootConfig, settingsFile, "0600", settingsJSON()),
		// Always written, empty or not, so a spec without a persona replaces
		// one that had it.
		contract.TextFile(contract.RootConfig, appendFile, "0600", appendMD(spec, l)),
	}
	if len(spec.Connectors) > 0 {
		files = append(files, contract.TextFile(contract.RootConfig, mcpFile, "0600", mcpJSON(spec.Connectors)))
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
	args := []string{"--model", spec.Model}
	if spec.Effort != "" {
		args = append(args, "--thinking", spec.Effort)
	}
	if len(spec.Connectors) == 0 {
		args = append(args, "--no-mcp")
	}
	cfg := openConfig{
		Binary:    BinaryPath(req.HarnessRoot),
		Extension: ExtensionPath(req.HarnessRoot),
		Env: []string{
			"HOME=" + l.Home,
			"USER=" + filepath.Base(l.Home),
			"PI_CODING_AGENT_DIR=" + l.Config,
			// pi fetches its model catalog from pi.dev unless offline; the
			// catalog it ships with serves.
			"PI_OFFLINE=1",
			"PI_TELEMETRY=0",
		},
		Args:       args,
		WorkingDir: l.Workspace,
		AgentDir:   l.Config,
		SessionDir: filepath.Join(l.Config, sessionsDir),
		Provider:   name,
		MaxRetries: maxRetries,
	}
	oc, err := json.Marshal(cfg)
	if err != nil {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
	return contract.ProvisionResult{
		Files:      files,
		OpenConfig: oc,
		// A session is its file: pi keeps nothing of it elsewhere.
		HistoryRoots: []contract.RootPath{
			{Root: contract.RootConfig, Path: sessionsDir},
			{Root: contract.RootConfig, Path: memoryDir},
		},
		SecretPaths:     []contract.RootPath{{Root: contract.RootConfig, Path: authFile}},
		HistoryRewrites: rewrites(req.Load, l),
	}, nil
}

// rewrites are, for a load, the session header's working directory: pi finds
// a session by --session-id only among those whose header names its working
// directory, and starts a new, empty one under the same id otherwise. The
// session dir is in the config root, so nothing moves.
func rewrites(load *contract.LoadSource, l contract.Layout) []contract.Rewrite {
	if load == nil || load.Workspace == l.Workspace {
		return nil
	}
	return []contract.Rewrite{{
		Path:  contract.RootPath{Root: contract.RootConfig, Path: sessionsDir},
		Field: headerCwd, From: load.Workspace, To: l.Workspace,
	}}
}

// headerCwd is the session header's field naming its working directory.
const headerCwd = "cwd"

// maxRetries is how often pi retries a failed model call: its default.
const maxRetries = 3

// settingsJSON is settings.json: pi retries a failed model call maxRetries
// times, warms no prompt cache (each warming is a billed call), and pings no
// install telemetry.
func settingsJSON() string {
	b, _ := json.MarshalIndent(map[string]any{
		"retry":                  map[string]any{"enabled": true, "maxRetries": maxRetries},
		"cacheWarming":           "off",
		"enableInstallTelemetry": false,
	}, "", "  ")
	return string(b) + "\n"
}

// mcpJSON is mcp.json: a server per connector, in Claude Code's format, its
// tools declared to the model ("exposure": "direct"). A header's value from a
// variable is ${VAR}, which pi reads from its environment; one from a file is
// "!" and adapter.HeaderCommand, a command pi runs each time it connects,
// so the value is in no configuration and no process's environment, read as
// the other profiles' headers scripts read it.
func mcpJSON(conns []contract.Connector) string {
	servers := map[string]any{}
	for _, c := range conns {
		s := map[string]any{"exposure": "direct"}
		switch {
		case c.Stdio != nil:
			s["command"] = c.Stdio.Command
			if len(c.Stdio.Args) > 0 {
				s["args"] = c.Stdio.Args
			}
			if len(c.Stdio.Env) > 0 {
				s["env"] = c.Stdio.Env
			}
		case c.HTTP != nil:
			s["url"] = c.HTTP.URL
			headers := map[string]string{}
			for k, v := range c.HTTP.Headers {
				headers[k] = v
			}
			for k, v := range c.HTTP.HeadersEnv {
				headers[k] = "${" + v + "}"
			}
			for k, f := range c.HTTP.HeadersFile {
				headers[k] = "!" + adapter.HeaderCommand(f)
			}
			if len(headers) > 0 {
				s["headers"] = headers
			}
		}
		servers[c.Name] = s
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return string(b) + "\n"
}

// appendMD is APPEND_SYSTEM.md, which pi adds to its system prompt: the
// persona, and where the agent keeps its memory.
func appendMD(spec contract.AgentSpec, l contract.Layout) string {
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
