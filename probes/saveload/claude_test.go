package saveload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/contract"
	tclaude "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

const (
	// envClaude names the pinned claude binary, as the profile's own tests do.
	envClaude = "HW_REAL_CLAUDE"
	// envClaudeHook names a built claude-code-hook, for a host without the go
	// tool; otherwise it is built from this tree.
	envClaudeHook = "HW_CLAUDE_CODE_HOOK"
	// envClaudeToken names a file holding a `claude setup-token` token: the
	// live run's credential.
	envClaudeToken = "HW_SAVELOAD_CLAUDE_TOKEN_FILE" //nolint:gosec // an environment variable's name
	// envClaudeModel names the live run's model; haiku otherwise.
	envClaudeModel = "HW_SAVELOAD_CLAUDE_MODEL"
)

// What every agent of the probe is told: nothing of a run's nonces.
const (
	persona       = "You are the agent of a save-and-load probe. Do exactly what each message asks, and be brief."
	workspaceNote = "# Workspace\n\nThis workspace has no repositories.\n"
)

// claudeCode is Claude Code in the probe.
type claudeCode struct{}

func (claudeCode) Name() string      { return claudecode.Name }
func (claudeCode) BinaryEnv() string { return envClaude }

func (claudeCode) Version(out string) string {
	if f := strings.Fields(out); len(f) > 0 {
		return f[0]
	}
	return ""
}

func (claudeCode) LiveReady() (bool, string) {
	return os.Getenv(envClaudeToken) != "", envClaudeToken + " names a file holding a `claude setup-token` token"
}

// Distribution is agentd's claude-code distribution: bin/claude and the hook
// helper, bin/claude-code-hook.
func (claudeCode) Distribution(p *probe, root string) {
	t := p.t
	t.Helper()
	if err := os.Symlink(p.bin, claudecode.BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	hook := os.Getenv(envClaudeHook)
	if hook == "" {
		hook = filepath.Join(p.work, "claude-code-hook")
		if _, err := os.Stat(hook); err != nil {
			build := exec.Command("go", "build", "-o", hook, "github.com/olesho/harness-wrapper/cmd/claude-code-hook")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("building the hook helper: %v\n%s", err, out)
			}
		}
	}
	if err := os.Symlink(hook, claudecode.HookPath(root)); err != nil {
		t.Fatal(err)
	}
}

// Spec is the spec agentd's API gives an agent with no repositories: a
// persona, the workspace's note, a memory, the bypass posture.
func (claudeCode) Spec(p *probe, memory []contract.MemoryFile) contract.AgentSpec {
	spec := contract.AgentSpec{
		Instructions:      contract.Instructions{Persona: persona, Workspace: workspaceNote},
		Memory:            &contract.Memory{Files: memory},
		PermissionPosture: contract.PostureBypass,
	}
	if p.mode == modeLive {
		spec.Model = "haiku"
		if m := os.Getenv(envClaudeModel); m != "" {
			spec.Model = m
		}
	}
	return spec
}

// claudeOpenConfig is the Claude Code profile's open_config, which the probe
// reads to point claude at the mock and to run claude by hand.
type claudeOpenConfig struct {
	Binary     string   `json:"binary"`
	Args       []string `json:"args"`
	Env        []string `json:"env"`
	WorkingDir string   `json:"working_dir"`
	Spool      string   `json:"spool"`
}

// PointAt gives claude the mock's address, and few retries, the way the
// profile's own tests do.
func (claudeCode) PointAt(mock *mockapi.Server, r *contract.ProvisionResult) error {
	var cfg claudeOpenConfig
	if err := json.Unmarshal(r.OpenConfig, &cfg); err != nil {
		return err
	}
	cfg.Env = append(cfg.Env, "ANTHROPIC_BASE_URL="+mock.URL(), "CLAUDE_CODE_MAX_RETRIES=2")
	var err error
	r.OpenConfig, err = json.Marshal(cfg)
	return err
}

// Credential stages the token where agentd's launcher does: a file named for
// its kind in the secrets root.
func (claudeCode) Credential(p *probe, l contract.Layout, label string) *contract.CredentialFile {
	p.t.Helper()
	token := placeholder(label)
	if p.mode == modeLive {
		b, err := os.ReadFile(os.Getenv(envClaudeToken))
		if err != nil {
			p.t.Fatalf("the live credential: %v", err)
		}
		token = strings.TrimSpace(string(b))
	}
	file := filepath.Join(l.Secrets, "claude-oauth-token")
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		p.t.Fatal(err)
	}
	return &contract.CredentialFile{Kind: claudecode.CredentialKind, File: file}
}

func (c claudeCode) Script(m mode, n nonces) script {
	if m == modeMock {
		return c.mockScript(n)
	}
	return script{
		remember:    "Reply with exactly PONG" + n.A + " and nothing else.",
		tool:        "Use the Bash tool to run exactly this command: echo " + n.T + " | tee " + noteFile + " — then reply with the command's output and nothing else.",
		recall:      "What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools.",
		recallReply: "PONG" + n.A,
		again:       "Earlier in this conversation you ran a shell command for me. What did it print? Reply with that output only, and use no tools.",
		againReply:  n.T,
		where:       "Use the Bash tool to run exactly this command: pwd — then reply with the command's output and nothing else.",
	}
}

// Script is the mock's, and one turn more: AGENT p hands p to a subagent.
func (claudeCode) mockScript(n nonces) script {
	sc := mockScript(n)
	sc.delegate, sc.delegateReply = "AGENT PING sub", "PONG sub"
	return sc
}

// mockScript speaks the mock's prompt language: PING n answers PONG n, and
// TOOL c runs c with the harness's shell tool.
func mockScript(n nonces) script {
	return script{
		remember:    "PING " + n.A,
		tool:        "TOOL echo " + n.T + " | tee " + noteFile,
		recall:      "PING again",
		recallReply: "PONG again",
		again:       "PING third",
		againReply:  "PONG third",
		where:       "TOOL pwd; cat " + noteFile,
	}
}

// FreshID is a UUID: agentd mints a Claude Code Session's id itself.
func (claudeCode) FreshID() string { return sessionid.NewUUID() }

func (claudeCode) Record(e *environment, sessionID string) (string, error) {
	return tclaude.Locate(sessionID, e.layout.Workspace, []string{"CLAUDE_CONFIG_DIR=" + e.layout.Config})
}

// Variants: the candidate is what the plan's archive holds — the adapter's
// history roots, less its secret paths — and the workspace's note; the other
// is the transcript alone.
func (claudeCode) Variants(src *environment, sessionID string) []variant {
	res := src.result
	transcript := "projects/" + tclaude.EncodedCWD(canonical(src.layout.Workspace)) + "/" + sessionID + ".jsonl"
	return []variant{
		{
			Name: "history-roots",
			Note: "the adapter's history_roots (config/projects, config/memory) less its secret_paths, and the workspace's note",
			holds: func(root contract.Root, path string) bool {
				if root == contract.RootWorkspace {
					return path == noteFile
				}
				return beneath(res.HistoryRoots, root, path) && !beneath(res.SecretPaths, root, path)
			},
		},
		{
			Name:  "transcript-only",
			Note:  "the Session's transcript alone",
			holds: func(root contract.Root, path string) bool { return root == contract.RootConfig && path == transcript },
		},
	}
}

// Relocations: claude keeps a workspace's transcripts in a directory named for
// the workspace's resolved path, and the adapter's record reader looks there.
// A saved Session's project directory therefore moves to the one the new
// workspace names; nothing inside it changes.
func (claudeCode) Relocations(m manifest, dst contract.Layout) []relocation {
	return []relocation{{
		From: contract.RootPath{Root: contract.RootConfig, Path: "projects/" + tclaude.EncodedCWD(m.Workspace)},
		To:   contract.RootPath{Root: contract.RootConfig, Path: "projects/" + tclaude.EncodedCWD(canonical(dst.Workspace))},
	}}
}

// Conversation reads a Messages API request's messages.
func (claudeCode) Conversation(body []byte) ([]item, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("no messages")
	}
	var out []item
	for _, m := range req.Messages {
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			out = append(out, item{Role: m.Role, Type: "text", Text: text})
			continue
		}
		var blocks []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, err
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				out = append(out, item{Role: m.Role, Type: "text", Text: b.Text})
			case "tool_use":
				out = append(out, item{Role: m.Role, Type: "tool_use", Name: b.Name, Text: string(b.Input)})
			case "tool_result":
				out = append(out, item{Role: m.Role, Type: "tool_result", Text: blockText(b.Content)})
			default:
				out = append(out, item{Role: m.Role, Type: b.Type})
			}
		}
	}
	return out, nil
}

// blockText is a content's text: a string, or its text blocks joined.
func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

// Resume runs claude by hand in the environment, as the adapter launches it
// but for the transport: `claude --resume <id> -p <prompt>`.
func (claudeCode) Resume(e *environment, sessionID string) (string, error) {
	var cfg claudeOpenConfig
	if err := json.Unmarshal(e.result.OpenConfig, &cfg); err != nil {
		return "", err
	}
	token, err := os.ReadFile(e.cred.File)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := append([]string{"--resume", sessionID, "-p", "PING resume"}, cfg.Args...)
	cmd := exec.CommandContext(ctx, cfg.Binary, args...) //nolint:gosec // the pinned binary, in the probe's environment
	cmd.Dir = cfg.WorkingDir
	cmd.Env = append(append(adapter.HostEnv(), cfg.Env...), "CLAUDE_CODE_OAUTH_TOKEN="+strings.TrimSpace(string(token)))
	out, err := cmd.CombinedOutput()
	detail := "claude --resume: " + strings.TrimSpace(string(out))
	switch {
	case err != nil && strings.Contains(string(out), "No conversation found"):
		return detail, fmt.Errorf("%w: %w", errNoHistory, err)
	case err != nil:
		return detail, fmt.Errorf("claude --resume %s: %w", sessionID, err)
	}
	return detail, nil
}

// Subagents: claude keeps a subagent's transcript beneath its Session's own
// directory, <project>/<session>/subagents/.
func (claudeCode) Subagents(entries []entry) []entry {
	var out []entry
	for _, e := range entries {
		if e.Root == contract.RootConfig && strings.Contains(e.Path, "/subagents/") {
			out = append(out, e)
		}
	}
	return out
}

// Outside: claude keeps a directory per workspace and session under
// /tmp/claude-<uid> — its tasks' output — whatever TMPDIR says
// (CLAUDE_CODE_TMPDIR moves it). An agentd workload's /tmp is its unit's own
// and goes with it.
func (claudeCode) Outside(e *environment) []string {
	return []string{filepath.Join("/tmp", "claude-"+strconv.Itoa(os.Getuid()), tclaude.EncodedCWD(canonical(e.layout.Workspace)))}
}

// Claude Code keeps nothing beside the transcript that a Save must name.

func (claudeCode) SetAux(*environment, string) (map[string]string, error) { return nil, nil }

func (claudeCode) Aux(*environment, string) (map[string]string, error) { return nil, nil }
