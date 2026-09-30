package saveload

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/codex"
	"github.com/olesho/harness-wrapper/pkg/contract"
	tcodex "github.com/olesho/harness-wrapper/pkg/transcript/codex"
)

const (
	// envCodex names the pinned codex binary — the native one — as the
	// profile's own tests do.
	envCodex = "HW_REAL_CODEX"
	// envCodexKey names a file holding an OpenAI API key, and envCodexToken
	// one holding a ChatGPT workspace's Codex access token: either is the
	// live run's credential.
	envCodexKey   = "HW_SAVELOAD_CODEX_API_KEY_FILE"      //nolint:gosec // an environment variable's name
	envCodexToken = "HW_SAVELOAD_CODEX_ACCESS_TOKEN_FILE" //nolint:gosec // an environment variable's name
	// envCodexLogin names the auth.json of a codex logged in with ChatGPT
	// (~/.codex/auth.json): the live run borrows that login. Neither of the
	// adapter's credential kinds is such a login, so this run's credential
	// does not travel as an agentd agent's would: see Adjust.
	envCodexLogin = "HW_SAVELOAD_CODEX_LOGIN" //nolint:gosec // an environment variable's name
	// envCodexModel names the live run's model; codex's default otherwise.
	envCodexModel = "HW_SAVELOAD_CODEX_MODEL"
)

// The files codex keeps a thread's name and goal in, beside its rollout: the
// candidate recipe saves them with it.
const (
	codexIndex = "session_index.jsonl"
	codexGoals = "goals_1.sqlite"
)

// codexCLI is Codex in the probe.
type codexCLI struct{}

func (codexCLI) Name() string      { return codex.Name }
func (codexCLI) BinaryEnv() string { return envCodex }

func (codexCLI) Version(out string) string {
	if f := strings.Fields(out); len(f) > 0 {
		return f[len(f)-1]
	}
	return ""
}

func (codexCLI) LiveReady() (bool, string) {
	return os.Getenv(envCodexKey) != "" || os.Getenv(envCodexToken) != "" || os.Getenv(envCodexLogin) != "",
		envCodexKey + " names a file holding an OpenAI API key, " + envCodexToken + " one holding a Codex access token, or " +
			envCodexLogin + " the auth.json of a ChatGPT login"
}

// Distribution is agentd's codex distribution: bin/codex, the native binary.
func (codexCLI) Distribution(p *probe, root string) {
	p.t.Helper()
	if err := os.Symlink(p.bin, codex.BinaryPath(root)); err != nil {
		p.t.Fatal(err)
	}
}

// Spec is the spec agentd's API gives an agent with no repositories. Against
// the mock the model is named "mock": codex's default model speaks a form of
// the Responses API that sends no tools.
func (codexCLI) Spec(p *probe, memory []contract.MemoryFile) contract.AgentSpec {
	spec := contract.AgentSpec{
		Instructions:      contract.Instructions{Persona: persona, Workspace: workspaceNote},
		Memory:            &contract.Memory{Files: memory},
		PermissionPosture: contract.PostureBypass,
		Model:             os.Getenv(envCodexModel),
	}
	if p.mode == modeMock {
		spec.Model = "mock"
	}
	return spec
}

// Adjust, in mock mode, gives the rendered config.toml a model provider of its
// own at the mock, the way the profile's own tests do. In a live run on a
// borrowed ChatGPT login it lends codex the login.
func (c codexCLI) Adjust(p *probe, r *contract.ProvisionResult) (string, error) {
	switch {
	case p.mock != nil:
		return "", c.pointAt(p.mock, r)
	case os.Getenv(envCodexLogin) != "" && os.Getenv(envCodexKey) == "" && os.Getenv(envCodexToken) == "":
		return c.lendLogin(os.Getenv(envCodexLogin), r)
	}
	return "", nil
}

// lendLogin gives the environment a ChatGPT login to run on: the login's
// access token, in an auth.json of the environment's own, which codex reads
// once its credential store is a file. The adapter's profile keeps codex's
// credentials in memory and takes an API key or a workspace's access token,
// so this is not how an agentd agent gets a credential; it is how a probe
// borrows the one login at hand.
//
// The refresh token is never copied. A refresh token is spent when it is
// used, and a copy that refreshed would log the lender out.
func (codexCLI) lendLogin(from string, r *contract.ProvisionResult) (string, error) {
	data, err := os.ReadFile(from) //nolint:gosec // the login the caller named
	if err != nil {
		return "", err
	}
	var login map[string]any
	if err := json.Unmarshal(data, &login); err != nil {
		return "", fmt.Errorf("%s: %w", from, err)
	}
	tokens, _ := login["tokens"].(map[string]any)
	if access, _ := tokens["access_token"].(string); access == "" {
		return "", fmt.Errorf("%s holds no ChatGPT login", from)
	}
	tokens["refresh_token"] = "withheld-by-the-saveload-probe"
	lent, err := json.Marshal(login)
	if err != nil {
		return "", err
	}
	for i, f := range r.Files {
		if f.Root != contract.RootConfig || f.Path != "config.toml" {
			continue
		}
		const memory, file = `cli_auth_credentials_store = "ephemeral"`, `cli_auth_credentials_store = "file"`
		if !strings.Contains(string(f.Content()), memory) {
			return "", errors.New("the rendered config.toml names no credential store")
		}
		r.Files[i] = contract.TextFile(f.Root, f.Path, f.Mode, strings.Replace(string(f.Content()), memory, file, 1))
		r.Files = append(r.Files, contract.TextFile(contract.RootConfig, "auth.json", "0600", string(lent)))
		return "a ChatGPT login's access token, lent in config/auth.json (no refresh token; codex's credential store set to file)", nil
	}
	return "", errors.New("no config.toml rendered")
}

// pointAt gives the rendered config.toml a model provider of its own at the
// mock.
func (codexCLI) pointAt(mock *mockapi.Server, r *contract.ProvisionResult) error {
	for i, f := range r.Files {
		if f.Root != contract.RootConfig || f.Path != "config.toml" {
			continue
		}
		provider := fmt.Sprintf(`
[model_providers.mock]
name = "mock"
base_url = %q
wire_api = "responses"
requires_openai_auth = true
request_max_retries = 0
stream_max_retries = %d
stream_idle_timeout_ms = 120000
`, mock.URL()+"/v1", mock.RetryBudget)
		r.Files[i] = contract.TextFile(f.Root, f.Path, f.Mode, "model_provider = \"mock\"\n"+string(f.Content())+provider)
		return nil
	}
	return errors.New("no config.toml rendered")
}

// Credential stages the key where agentd's launcher does: a file named for
// its kind in the secrets root.
func (codexCLI) Credential(p *probe, l contract.Layout, label string) *contract.CredentialFile {
	p.t.Helper()
	kind, name, secret := codex.CredentialAPIKey, "openai-api-key", "sk-"+placeholder(label)
	if p.mode == modeLive {
		from := os.Getenv(envCodexKey)
		if from == "" {
			kind, name, from = codex.CredentialAccessToken, "codex-access-token", os.Getenv(envCodexToken)
		}
		if from == "" {
			return nil // a borrowed ChatGPT login: Adjust lends it
		}
		b, err := os.ReadFile(from) //nolint:gosec // the credential file the caller named
		if err != nil {
			p.t.Fatalf("the live credential: %v", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	file := filepath.Join(l.Secrets, name)
	if err := os.WriteFile(file, []byte(secret+"\n"), 0o600); err != nil {
		p.t.Fatal(err)
	}
	return &contract.CredentialFile{Kind: kind, File: file}
}

func (codexCLI) Script(m mode, n nonces) script {
	if m == modeMock {
		return mockScript(n)
	}
	return script{
		remember:    "Reply with exactly PONG" + n.A + " and nothing else.",
		tool:        "Use your shell tool to run exactly this command: echo " + n.T + " | tee " + noteFile + " — then reply with the command's output and nothing else.",
		recall:      "What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools.",
		recallReply: "PONG" + n.A,
		again:       "Earlier in this conversation you ran a shell command for me. What did it print? Reply with that output only, and use no tools.",
		againReply:  n.T,
		where:       "Use your shell tool to run exactly this command: pwd — then reply with the command's output and nothing else.",
	}
}

// FreshID is empty: codex chooses a thread's id, and agentd adopts it.
func (codexCLI) FreshID() string { return "" }

func (codexCLI) Record(e *environment, sessionID string) (string, error) {
	return tcodex.Rollout(e.layout.Config, sessionID)
}

// Variants: the candidate adds, to the adapter's history roots, the two files
// codex keeps a thread's name and goal in. The others take them apart: what
// today's delete archive holds, the rollout alone, and the rollout with each
// of the two — and the goals database with fewer of its sidecars, to see which
// of them a copy needs.
func (codexCLI) Variants(src *environment, sessionID string) []variant {
	res := src.result
	history := func(root contract.Root, path string) bool {
		return beneath(res.HistoryRoots, root, path) && !beneath(res.SecretPaths, root, path)
	}
	rollout := func(root contract.Root, path string) bool {
		return root == contract.RootConfig && strings.HasPrefix(path, "sessions/") && strings.HasSuffix(path, "-"+sessionID+".jsonl")
	}
	index := func(root contract.Root, path string) bool { return root == contract.RootConfig && path == codexIndex }
	// A SQLite database is its file and whatever sidecars sit beside it.
	goals := func(root contract.Root, path string) bool {
		return root == contract.RootConfig && strings.HasPrefix(path, codexGoals)
	}
	return []variant{
		{
			Name: "candidate",
			Note: "the adapter's history_roots (config/sessions, config/memory) less its secret_paths, " + codexIndex + ", " + codexGoals + " with its sidecars, and the workspace's note",
			holds: func(root contract.Root, path string) bool {
				if root == contract.RootWorkspace {
					return path == noteFile
				}
				return history(root, path) || index(root, path) || goals(root, path)
			},
		},
		{Name: "history-roots", Note: "the adapter's history_roots alone: what a delete archives today", holds: history},
		{Name: "rollout-only", Note: "the thread's rollout alone", holds: rollout},
		{
			Name: "rollout+index", Note: "the rollout and " + codexIndex,
			holds: func(root contract.Root, path string) bool { return rollout(root, path) || index(root, path) },
		},
		{
			Name: "rollout+goals", Note: "the rollout and " + codexGoals + " with its sidecars",
			holds: func(root contract.Root, path string) bool { return rollout(root, path) || goals(root, path) },
		},
		{
			Name: "rollout+goals-wal", Note: "the rollout, " + codexGoals + " and its -wal, without its -shm",
			holds: func(root contract.Root, path string) bool {
				return rollout(root, path) || goals(root, path) && !strings.HasSuffix(path, "-shm")
			},
		},
		{
			Name: "rollout+goals-db", Note: "the rollout and " + codexGoals + " without its sidecars",
			holds: func(root contract.Root, path string) bool {
				return rollout(root, path) || root == contract.RootConfig && path == codexGoals
			},
		},
	}
}

// Subagents: the probe's codex turns start none.
func (codexCLI) Subagents([]entry) []entry { return nil }

// Outside: codex was seen to write nothing outside CODEX_HOME and the
// workspace.
func (codexCLI) Outside(*environment) []string { return nil }

// Relocations: none. codex finds a thread's rollout by its id anywhere under
// CODEX_HOME/sessions, and takes the working directory from thread/resume.
func (codexCLI) Relocations(manifest, contract.Layout) []relocation { return nil }

// Conversation reads a Responses API request's input.
func (codexCLI) Conversation(body []byte) ([]item, error) {
	var req struct {
		Input []struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Content   json.RawMessage `json:"content"`
			Output    json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if len(req.Input) == 0 {
		return nil, errors.New("no input")
	}
	var out []item
	for _, in := range req.Input {
		switch {
		case in.Type == "message":
			var parts []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(in.Content, &parts) != nil {
				out = append(out, item{Role: in.Role, Type: "text", Text: blockText(in.Content)})
				continue
			}
			for _, part := range parts {
				out = append(out, item{Role: in.Role, Type: "text", Text: part.Text})
			}
		case strings.HasSuffix(in.Type, "_call"):
			out = append(out, item{Role: "assistant", Type: "tool_use", Name: in.Name, Text: in.Arguments})
		case strings.HasSuffix(in.Type, "_call_output"):
			out = append(out, item{Role: "tool", Type: "tool_result", Text: blockText(in.Output)})
		default:
			out = append(out, item{Role: in.Role, Type: in.Type})
		}
	}
	return out, nil
}

// codexOpenConfig is the Codex profile's open_config, which the probe reads to
// run codex by hand.
type codexOpenConfig struct {
	Binary     string   `json:"binary"`
	Env        []string `json:"env"`
	WorkingDir string   `json:"working_dir"`
	CodexHome  string   `json:"codex_home"`
}

// appServer is a codex app-server the probe speaks to itself: for what the
// Harness Adapter Interface has no words for — a thread's name and its goal —
// and for a resume with no adapter between. It runs as the adapter runs codex:
// the same binary, working directory and environment.
type appServer struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan []byte
	next  int
}

func startAppServer(e *environment) (*appServer, error) {
	var cfg codexOpenConfig
	if err := json.Unmarshal(e.result.OpenConfig, &cfg); err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.Binary, "app-server") //nolint:gosec // the pinned binary, in the probe's environment
	cmd.Dir = cfg.WorkingDir
	cmd.Env = append(adapter.HostEnv(), cfg.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a := &appServer{cmd: cmd, stdin: stdin, lines: make(chan []byte, 64)}
	go func() {
		defer close(a.lines)
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				a.lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	if err := a.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "saveload-probe", "title": "saveload probe", "version": "0"}}, nil); err != nil {
		a.close()
		return nil, err
	}
	if err := a.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

func (a *appServer) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = a.stdin.Write(append(b, '\n'))
	return err
}

// call sends a request and waits for its answer, which it decodes into out.
// Notifications are passed over, and a request of codex's own is refused.
func (a *appServer) call(method string, params, out any) error {
	a.next++
	id := a.next
	if err := a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	timeout := time.After(time.Minute)
	for {
		select {
		case line, ok := <-a.lines:
			if !ok {
				return fmt.Errorf("%s: codex exited", method)
			}
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &msg) != nil {
				continue
			}
			if msg.Method != "" {
				if len(msg.ID) > 0 {
					_ = a.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "not supported by the probe"}})
				}
				continue
			}
			var got int
			if json.Unmarshal(msg.ID, &got) != nil || got != id {
				continue
			}
			if msg.Error != nil {
				return fmt.Errorf("%s: codex: %s (%d)", method, msg.Error.Message, msg.Error.Code)
			}
			if out == nil {
				return nil
			}
			return json.Unmarshal(msg.Result, out)
		case <-timeout:
			return fmt.Errorf("%s: no answer within a minute", method)
		}
	}
}

// close ends codex: stdin's end asks an idle app-server to exit.
func (a *appServer) close() {
	_ = a.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = a.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = a.cmd.Process.Kill()
		<-done
	}
}

// resume resumes the thread in the app-server, with the parameters the
// adapter's transport gives.
func (a *appServer) resume(e *environment, threadID string) error {
	return a.call("thread/resume", map[string]any{
		"threadId": threadID, "cwd": e.layout.Workspace, "approvalPolicy": "never", "sandbox": "danger-full-access",
	}, nil)
}

// aux reads the thread's name and goal.
func (a *appServer) aux(threadID string) (map[string]string, error) {
	var read struct {
		Thread struct {
			Name *string `json:"name"`
		} `json:"thread"`
	}
	if err := a.call("thread/read", map[string]any{"threadId": threadID}, &read); err != nil {
		return nil, err
	}
	var goal struct {
		Goal *struct {
			Objective string `json:"objective"`
			Status    string `json:"status"`
		} `json:"goal"`
	}
	if err := a.call("thread/goal/get", map[string]any{"threadId": threadID}, &goal); err != nil {
		return nil, err
	}
	out := map[string]string{"name": "", "goal": "", "goal_status": ""}
	if read.Thread.Name != nil {
		out["name"] = *read.Thread.Name
	}
	if goal.Goal != nil {
		out["goal"], out["goal_status"] = goal.Goal.Objective, goal.Goal.Status
	}
	return out, nil
}

// Resume resumes the thread by its id in an app-server of the probe's own.
func (codexCLI) Resume(e *environment, sessionID string) (string, error) {
	a, err := startAppServer(e)
	if err != nil {
		return "", err
	}
	defer a.close()
	if err := a.resume(e, sessionID); err != nil {
		if strings.Contains(err.Error(), "no rollout found") {
			err = fmt.Errorf("%w: %w", errNoHistory, err)
		}
		return err.Error(), err
	}
	return "thread/resume resumed " + sessionID, nil
}

// SetAux names the thread and gives it a goal, as a user of codex would. The
// goal is paused: an active one makes codex start turns of its own, which
// would race the probe's.
func (codexCLI) SetAux(e *environment, sessionID string) (map[string]string, error) {
	a, err := startAppServer(e)
	if err != nil {
		return nil, err
	}
	defer a.close()
	if err := a.resume(e, sessionID); err != nil {
		return nil, err
	}
	n := e.p.nonce
	if err := a.call("thread/name/set", map[string]any{"threadId": sessionID, "name": "saveload probe " + n.M}, nil); err != nil {
		return nil, err
	}
	if err := a.call("thread/goal/set", map[string]any{
		"threadId": sessionID, "objective": "Keep the save and load probe's notes " + n.M, "status": "paused",
	}, nil); err != nil {
		return nil, err
	}
	return a.aux(sessionID)
}

// Aux reads the thread's name and goal back, resuming it first as any client
// that opens the thread does.
func (codexCLI) Aux(e *environment, sessionID string) (map[string]string, error) {
	a, err := startAppServer(e)
	if err != nil {
		return nil, err
	}
	defer a.close()
	if err := a.resume(e, sessionID); err != nil {
		return nil, err
	}
	return a.aux(sessionID)
}
