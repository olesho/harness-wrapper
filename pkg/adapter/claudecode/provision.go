package claudecode

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"

	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/harness/claude"
	tclaude "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// Limits on what one spec renders.
const (
	MaxPersona     = 64 << 10
	MaxSkillFile   = 256 << 10
	MaxSkillFiles  = 64
	MaxMemoryFile  = 256 << 10
	MaxProfileSize = 16 << 20 // every file together
)

// Files the profile renders, relative to their roots.
const (
	settingsFile  = "settings.json"
	claudeJSON    = ".claude.json"
	personaFile   = "persona.md"
	mcpFile       = "mcp.json"
	headersDir    = "mcp-headers" // a headersHelper script per server, beside mcp.json
	skillsDir     = "skills"
	memoryDir     = "memory"
	workspaceFile = "CLAUDE.md"
)

// hookOwner marks the profile's entries in settings.json.
const hookOwner = "harness-wrapper"

// cleanupPeriodDays is how long claude keeps a transcript: the Session's
// record, so as good as for ever.
const cleanupPeriodDays = 3650

// openConfig is the profile's open_config: everything Start launches claude
// with, rendered from the spec and the layout.
type openConfig struct {
	Binary string `json:"binary"`
	// Args are claude's arguments beyond the session's and the transport's.
	Args []string `json:"args"`
	// Env is claude's environment beyond the Host's PATH, LANG, LC_* and TZ,
	// and the credential.
	Env        []string `json:"env"`
	WorkingDir string   `json:"working_dir"`
	// Spool is the hook spool: where the hook helper writes, and the record
	// reader reads. It is the scratch root itself, beside the submission
	// markers' directory, so a spool a host kept before this profile — at
	// the root it now names scratch — is read where it is.
	Spool string `json:"spool"`
}

func parseOpenConfig(raw []byte) (openConfig, error) {
	var cfg openConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("open_config: %w", err)
	}
	if cfg.Binary == "" || cfg.WorkingDir == "" || cfg.Spool == "" {
		return cfg, fmt.Errorf("open_config: binary, working_dir and spool are required")
	}
	return cfg, nil
}

// hookSpec is the hooks claude runs: hw's claude hook provider's entries and
// its per-tool ones, without the yield guard — no caller asks an agent to
// yield, and the guard would run on every tool call.
func hookSpec() *harnesscore.HookSpec {
	hp := claude.Profile{}.StaticHookProvider()
	base := hp.HookSpec()
	events := append([]harnesscore.HookEntry(nil), base.Events...)
	if th, ok := hp.(harnesscore.ToolHookProvider); ok {
		events = append(events, th.ToolHookEntries()...)
	}
	return &harnesscore.HookSpec{ConfigPath: base.ConfigPath, Owner: hookOwner, Events: events}
}

// projectsDir is where claude keeps its transcripts, beneath the config root:
// one directory per working directory, named for it (tclaude.EncodedCWD).
const projectsDir = "projects"

// relocations are where a saved Session's history goes in a new environment.
// claude names a working directory's transcripts for its resolved path, so the
// one directory the source's workspace named moves to the one the new
// workspace names — a subagent's transcript, beneath it, with it — and nothing
// inside is rewritten. Memory keeps its place.
func relocations(load *contract.LoadSource, l contract.Layout) []contract.Relocation {
	if load == nil {
		return nil
	}
	from, to := tclaude.EncodedCWD(load.Workspace), tclaude.EncodedCWD(l.Workspace)
	if from == to {
		return nil
	}
	return []contract.Relocation{{
		From: contract.RootPath{Root: contract.RootConfig, Path: path.Join(projectsDir, from)},
		To:   contract.RootPath{Root: contract.RootConfig, Path: path.Join(projectsDir, to)},
	}}
}

// Provision renders claude's configuration for the spec: settings.json with the
// hooks, .claude.json with onboarding, bypass and workspace trust answered,
// the persona, skills, memory, mcp.json and the workspace's CLAUDE.md, and
// open_config with claude's arguments and environment. For a request that
// loads a saved Session it names the history's one relocation.
func (Profile) Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error) {
	spec, l := req.Spec, req.Layout
	invalid := func(field, format string, args ...any) error {
		return &contract.Error{Code: contract.CodeInvalidSpec, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	if len(spec.Instructions.Persona) > MaxPersona {
		return contract.ProvisionResult{}, invalid("instructions.persona", "%d bytes, more than %d", len(spec.Instructions.Persona), MaxPersona)
	}

	settings, err := settingsJSON(req.HarnessRoot, l.Config)
	if err != nil {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
	onboarding, err := json.MarshalIndent(map[string]any{
		"hasCompletedOnboarding":        true,
		"bypassPermissionsModeAccepted": true,
		"numStartups":                   1,
		"projects": map[string]any{
			l.Workspace: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true},
		},
	}, "", "  ")
	if err != nil {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
	files := []contract.File{
		contract.TextFile(contract.RootConfig, settingsFile, "0600", string(settings)),
		contract.TextFile(contract.RootConfig, claudeJSON, "0600", string(onboarding)),
		// Always written, empty or not: every launch names it.
		contract.TextFile(contract.RootConfig, personaFile, "0600", spec.Instructions.Persona),
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
	args := []string{
		"--append-system-prompt-file", filepath.Join(l.Config, personaFile),
		"--system-prompt-snapshot", "off",
	}
	if len(spec.Connectors) > 0 {
		mcp, helpers, err := mcpJSON(spec.Connectors, l.Config)
		if err != nil {
			return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
		}
		files = append(files, helpers...)
		files = append(files, contract.TextFile(contract.RootConfig, mcpFile, "0600", string(mcp)))
		args = append(args, "--mcp-config", filepath.Join(l.Config, mcpFile), "--strict-mcp-config")
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

	// The permission posture is bypass: isolation is the runtime's.
	args = append(args, "--permission-mode", "bypassPermissions")
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	cfg := openConfig{
		Binary: BinaryPath(req.HarnessRoot),
		Args:   args,
		Env: []string{
			"HOME=" + l.Home,
			"USER=" + filepath.Base(l.Home),
			"CLAUDE_CONFIG_DIR=" + l.Config,
			"TERM=xterm-256color",
			harnesscore.EnvSpool + "=" + l.Scratch,
			harnesscore.EnvHookCwd + "=" + l.Workspace,
			harnesscore.EnvHome + "=" + l.Home,
			harnesscore.EnvConfigDir + "=" + l.Config,
			// claude refuses to bypass permissions as root outside a sandbox;
			// the runtime's isolation is the sandbox.
			"IS_SANDBOX=1",
			"DISABLE_AUTOUPDATER=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		},
		WorkingDir: l.Workspace,
		Spool:      l.Scratch,
	}
	oc, err := json.Marshal(cfg)
	if err != nil {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
	return contract.ProvisionResult{
		Files:      files,
		OpenConfig: oc,
		HistoryRoots: []contract.RootPath{
			{Root: contract.RootConfig, Path: projectsDir},
			{Root: contract.RootConfig, Path: memoryDir},
		},
		SecretPaths:        []contract.RootPath{{Root: contract.RootConfig, Path: ".credentials.json"}},
		HistoryRelocations: relocations(req.Load, l),
	}, nil
}

// settingsJSON is claude's settings.json: transcripts kept, the bypass
// prompt skipped, memory in the config root, and the hooks, each running the
// distribution's hook helper.
func settingsJSON(harnessRoot, config string) ([]byte, error) {
	base, err := json.Marshal(map[string]any{
		"cleanupPeriodDays":                 cleanupPeriodDays,
		"skipDangerousModePermissionPrompt": true,
		"autoMemoryDirectory":               path.Join(config, memoryDir),
	})
	if err != nil {
		return nil, err
	}
	return harnesscore.RenderSettingsJSONHooks(base, hookSpec(), []string{HookPath(harnessRoot)}, "claude")
}

// mcpJSON is mcp.json: one server per connector, and the files it needs
// beside it. An http connector's headers_env names the variable each
// header's value is read from, which claude expands as ${VAR}; its
// headers_file, the file each value is read from, by a script claude runs
// each time it connects (headersHelper), so the value is in no
// configuration and no process's environment. A script is a file of its
// own under config, holding paths and no value; claude hands headersHelper
// to a shell, which runs it with /bin/sh, since no provisioned file is
// executable.
func mcpJSON(conns []contract.Connector, config string) ([]byte, []contract.File, error) {
	servers := map[string]any{}
	var helpers []contract.File
	for _, c := range conns {
		switch {
		case c.Stdio != nil:
			s := map[string]any{"type": "stdio", "command": c.Stdio.Command}
			if len(c.Stdio.Args) > 0 {
				s["args"] = c.Stdio.Args
			}
			if len(c.Stdio.Env) > 0 {
				s["env"] = c.Stdio.Env
			}
			servers[c.Name] = s
		case c.HTTP != nil:
			s := map[string]any{"type": "http", "url": c.HTTP.URL}
			headers := map[string]string{}
			for k, v := range c.HTTP.Headers {
				headers[k] = v
			}
			names := make([]string, 0, len(c.HTTP.HeadersEnv))
			for k := range c.HTTP.HeadersEnv {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				headers[k] = "${" + c.HTTP.HeadersEnv[k] + "}"
			}
			if len(headers) > 0 {
				s["headers"] = headers
			}
			if len(c.HTTP.HeadersFile) > 0 {
				rel := path.Join(headersDir, c.Name+".sh")
				helpers = append(helpers, contract.TextFile(contract.RootConfig, rel, "0600", adapter.HeadersScript(c.Name, c.HTTP.HeadersFile)))
				s["headersHelper"] = "/bin/sh " + adapter.ShellQuote(filepath.Join(config, rel))
			}
			servers[c.Name] = s
		}
	}
	b, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return b, helpers, err
}
