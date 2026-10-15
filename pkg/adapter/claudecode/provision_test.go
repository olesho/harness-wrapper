package claudecode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// golden is what agentd's own renderer produced before this profile
// (testdata/agentd-profile.json: agentd's internal/node/profile at 5c5e5a6,
// with hw v0.19.0), for the spec it holds.
type golden struct {
	Spec struct {
		Model, Effort, Persona string
		Skills                 []struct {
			Name  string
			Files []struct{ Path, Content string }
		}
		Memory     []struct{ Path, Content string }
		Connectors []struct {
			Name string
			MCP  json.RawMessage
		}
	}
	Files []struct {
		Root, Path, Mode, Content string
	}
	Env            []string
	Args           []string
	HwInjectedArgs []string `json:"hw_injected_args"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	b, err := os.ReadFile("testdata/agentd-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// goldenRequest is the Provision request for the golden's spec and layout.
func goldenRequest(t *testing.T, g golden) contract.ProvisionRequest {
	t.Helper()
	spec := contract.AgentSpec{
		Model: g.Spec.Model, Effort: g.Spec.Effort,
		Instructions:      contract.Instructions{Persona: g.Spec.Persona},
		PermissionPosture: contract.PostureBypass,
		Credential:        &contract.CredentialRef{Kind: CredentialKind},
	}
	for _, f := range g.Files {
		if f.Root == "workspace" && f.Path == "CLAUDE.md" {
			spec.Instructions.Workspace = f.Content
		}
	}
	for _, sk := range g.Spec.Skills {
		s := contract.Skill{Name: sk.Name}
		for _, f := range sk.Files {
			s.Files = append(s.Files, contract.SkillFile{Path: f.Path, Content: f.Content})
		}
		spec.Skills = append(spec.Skills, s)
	}
	spec.Memory = &contract.Memory{}
	for _, f := range g.Spec.Memory {
		spec.Memory.Files = append(spec.Memory.Files, contract.MemoryFile{Path: f.Path, Content: f.Content})
	}
	for _, c := range g.Spec.Connectors {
		var mcp struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(c.MCP, &mcp); err != nil {
			t.Fatal(err)
		}
		conn := contract.Connector{Name: c.Name}
		if mcp.Type == "http" {
			conn.HTTP = &contract.HTTPConnector{URL: mcp.URL, Headers: mcp.Headers}
		} else {
			conn.Stdio = &contract.StdioConnector{Command: mcp.Command, Args: mcp.Args, Env: mcp.Env}
		}
		spec.Connectors = append(spec.Connectors, conn)
	}
	return contract.ProvisionRequest{
		Contract: contract.Version, HarnessRoot: "/opt/harnesses/claude-code",
		// agentd's spool directory is the scratch root.
		Layout: contract.Layout{Home: "/w/home", Config: "/w/profile", Workspace: "/w/workspace", Secrets: "/w/secrets", Scratch: "/w/spool"},
		Spec:   spec,
	}
}

// unquote reverses posixSingleQuote.
func unquote(s string) string {
	s = strings.TrimPrefix(strings.TrimSuffix(s, "'"), "'")
	return strings.ReplaceAll(s, `'\''`, "'")
}

// hookCall is a rendered hook command's program, its arguments and its owner.
func hookCall(t *testing.T, cmd string) (prog string, args []string, owner string) {
	t.Helper()
	body, owner, ok := strings.Cut(cmd, " # harness-wrapper-hook:")
	if !ok || !strings.HasPrefix(body, "sh -c ") {
		t.Fatalf("hook command %q is not one RenderHookCommand writes", cmd)
	}
	inner := unquote(strings.TrimPrefix(body, "sh -c "))
	_, exec, ok := strings.Cut(inner, "; exec ")
	if !ok {
		t.Fatalf("hook command %q execs nothing", inner)
	}
	var toks []string
	for _, f := range strings.Split(exec, "' '") {
		toks = append(toks, strings.Trim(f, "'"))
	}
	return toks[0], toks[1:], owner
}

// hooksOf is a settings.json's hooks as (event, matcher) → the arguments each
// command passes after its program, and the settings without its hooks.
func hooksOf(t *testing.T, settings string) (map[string][]string, map[string]any, []string, []string) {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		t.Fatal(err)
	}
	var hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	raw, _ := json.Marshal(s["hooks"])
	_ = json.Unmarshal(raw, &hooks)
	delete(s, "hooks")
	out := map[string][]string{}
	var progs, owners []string
	for event, ms := range hooks {
		for _, m := range ms {
			for _, h := range m.Hooks {
				prog, args, owner := hookCall(t, h.Command)
				progs, owners = append(progs, prog), append(owners, owner)
				key := event + "[" + m.Matcher + "]"
				out[key] = append(out[key], strings.Join(args[len(args)-2:], " "))
			}
		}
	}
	return out, s, progs, owners
}

// Provision keeps the meaning of the profile agentd rendered before it. The
// differences are the reviewed ones: each hook runs the distribution's helper
// under this profile's owner; agentd's own variables and the yield file are
// gone; PATH and LANG are the Host's, added at launch; and the model, effort
// and permission flags hw added at launch are in the arguments; .claude.json
// marks claude's auto-mode nudge seen (testdata's copy has the key added,
// 2026-10-10). With agentd's
// spool directory as the scratch root, the spool is where it was.
func TestProvisionMatchesAgentdProfile(t *testing.T) {
	g := loadGolden(t)
	req := goldenRequest(t, g)
	res, err := adapter.New(Profile{}).Provision(req)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]contract.File{}
	for _, f := range res.Files {
		got[string(f.Root)+"/"+f.Path] = f
	}
	for _, want := range g.Files {
		key := want.Root + "/" + want.Path
		f, ok := got[key]
		if !ok {
			t.Errorf("no %s", key)
			continue
		}
		delete(got, key)
		mode, _ := f.FileMode()
		if mode.String() != want.Mode {
			t.Errorf("%s: mode %s, want %s", key, mode, want.Mode)
		}
		switch want.Path {
		case settingsFile:
			wantHooks, wantRest, _, _ := hooksOf(t, want.Content)
			gotHooks, gotRest, progs, owners := hooksOf(t, string(f.Content()))
			if !reflect.DeepEqual(gotRest, wantRest) {
				t.Errorf("settings.json without its hooks:\n%v\nwant\n%v", gotRest, wantRest)
			}
			if !reflect.DeepEqual(gotHooks, wantHooks) {
				t.Errorf("settings.json hooks:\n%v\nwant\n%v", gotHooks, wantHooks)
			}
			for i := range progs {
				if progs[i] != HookPath(req.HarnessRoot) || owners[i] != hookOwner {
					t.Errorf("a hook runs %s owned by %s, want %s owned by %s", progs[i], owners[i], HookPath(req.HarnessRoot), hookOwner)
				}
			}
		case mcpFile:
			var a, b any
			_ = json.Unmarshal(f.Content(), &a)
			_ = json.Unmarshal([]byte(want.Content), &b)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("mcp.json:\n%s\nwant\n%s", f.Content(), want.Content)
			}
		default:
			if string(f.Content()) != want.Content {
				t.Errorf("%s:\n%s\nwant\n%s", key, f.Content(), want.Content)
			}
		}
	}
	for key := range got {
		t.Errorf("a file agentd did not render: %s", key)
	}

	cfg, err := parseOpenConfig(res.OpenConfig)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]string{}, g.Args...), g.HwInjectedArgs...); !reflect.DeepEqual(cfg.Args, want) {
		t.Errorf("args %q, want %q", cfg.Args, want)
	}
	if cfg.Binary != BinaryPath(req.HarnessRoot) || cfg.WorkingDir != req.Layout.Workspace {
		t.Errorf("binary %s, working dir %s", cfg.Binary, cfg.WorkingDir)
	}
	drop := map[string]bool{"PATH": true, "LANG": true, "AGENTD_AGENT_ID": true, "AGENTD_LAUNCH_ID": true, "HW_YIELD_FILE": true}
	var wantEnv, gotEnv []string
	for _, kv := range g.Env {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			wantEnv = append(wantEnv, kv)
		}
	}
	gotEnv = append(gotEnv, cfg.Env...)
	if cfg.Spool != "/w/spool" {
		t.Errorf("spool %s, want the scratch root, /w/spool", cfg.Spool)
	}
	sort.Strings(wantEnv)
	sort.Strings(gotEnv)
	if !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Errorf("env %q, want %q", gotEnv, wantEnv)
	}
}

// Provision is pure, and refuses what claude does not take.
func TestProvisionRefuses(t *testing.T) {
	g := loadGolden(t)
	req := goldenRequest(t, g)
	a := adapter.New(Profile{})
	r1, err1 := a.Provision(req)
	r2, err2 := a.Provision(req)
	if err1 != nil || err2 != nil || !reflect.DeepEqual(r1, r2) {
		t.Fatalf("two renderings differ: %v %v", err1, err2)
	}
	for name, mutate := range map[string]func(*contract.ProvisionRequest){
		"an api key": func(r *contract.ProvisionRequest) {
			r.Spec.Credential = &contract.CredentialRef{Kind: "anthropic_api_key"}
		},
		"a huge persona": func(r *contract.ProvisionRequest) { r.Spec.Instructions.Persona = strings.Repeat("x", MaxPersona+1) },
		"an odd effort":  func(r *contract.ProvisionRequest) { r.Spec.Effort = "ultra" },
	} {
		bad := goldenRequest(t, g)
		mutate(&bad)
		if _, err := a.Provision(bad); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

// At the gated posture claude runs in its default mode and asks the stream for
// each permission; everything else is bypass's, byte for byte. ProvisionWith
// leaves how claude asks to its caller.
func TestProvisionGated(t *testing.T) {
	g := loadGolden(t)
	bypass, err := Profile{}.Provision(goldenRequest(t, g))
	if err != nil {
		t.Fatal(err)
	}
	req := goldenRequest(t, g)
	req.Spec.PermissionPosture = contract.PostureGated
	gated, err := Profile{}.Provision(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bypass.Files, gated.Files) {
		t.Error("gated renders other files than bypass")
	}
	args := func(r contract.ProvisionResult) []string {
		cfg, err := parseOpenConfig(r.OpenConfig)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Args
	}
	want := strings.Replace(strings.Join(args(bypass), " "), "--permission-mode bypassPermissions",
		"--permission-mode default --permission-prompt-tool stdio", 1)
	if got := strings.Join(args(gated), " "); got != want {
		t.Errorf("gated args\n%s\nwant\n%s", got, want)
	}
	with, err := ProvisionWith(req, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args(with), " "); strings.Contains(got, "--permission-prompt-tool") || !strings.Contains(got, "--permission-mode default") {
		t.Errorf("ProvisionWith at gated: %s", got)
	}
}

// A request that loads a saved Session names the one move claude's history
// needs: the project directory the source's workspace named, to the one the
// new workspace names. Nothing moves when both name the same.
func TestProvisionRelocatesALoadedSession(t *testing.T) {
	g := loadGolden(t)
	req := goldenRequest(t, g)
	a := adapter.New(Profile{})
	d := a.Describe()
	if !d.Has(contract.CapSessionLoad) || !d.Loads(adapter.ArchiveFormat, d.Harness.Version) {
		t.Fatalf("the profile does not load its own version's Sessions: %+v", d.Load)
	}
	src := req.Layout
	src.Workspace = "/srv/agents/old.one/work_space"
	req.Load = &contract.LoadSource{Format: adapter.ArchiveFormat, Harness: d.Harness, Layout: src, Workspace: "/private" + src.Workspace}
	res, err := a.Provision(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []contract.Relocation{{
		From: contract.RootPath{Root: contract.RootConfig, Path: "projects/-private-srv-agents-old-one-work-space"},
		To:   contract.RootPath{Root: contract.RootConfig, Path: "projects/" + strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(req.Layout.Workspace)},
	}}
	if !reflect.DeepEqual(res.HistoryRelocations, want) {
		t.Errorf("history_relocations = %+v, want %+v", res.HistoryRelocations, want)
	}
	moved := contract.Relocate(res.HistoryRelocations, contract.RootPath{Root: contract.RootConfig, Path: "projects/-private-srv-agents-old-one-work-space/s/subagents/agent-1.jsonl"})
	if moved.Path != want[0].To.Path+"/s/subagents/agent-1.jsonl" {
		t.Errorf("a subagent's transcript moves to %s", moved)
	}
	if kept := contract.Relocate(res.HistoryRelocations, contract.RootPath{Root: contract.RootConfig, Path: "memory/notes.md"}); kept.Path != "memory/notes.md" {
		t.Errorf("memory moves to %s", kept)
	}

	req.Load.Workspace = req.Layout.Workspace
	if res, err = a.Provision(req); err != nil || len(res.HistoryRelocations) != 0 {
		t.Errorf("the same workspace: %+v %v, want no relocation", res.HistoryRelocations, err)
	}
	req.Load.Harness.Version = "0.0.1"
	if _, err = a.Provision(req); contract.CodeOf(err) != contract.CodeUnsupported {
		t.Errorf("a Session another claude saved: %v, want unsupported", err)
	}
	req.Load = nil
	if res, err = a.Provision(req); err != nil || len(res.HistoryRelocations) != 0 {
		t.Errorf("no load: %+v %v, want no relocation", res.HistoryRelocations, err)
	}
}

// A load that names the working directories its Sessions had of their own
// (session_workdir) moves each one's project directory too, to the one the
// same directory beneath the new workspace names; the workspace's moves as
// before, and a transcript beneath one directory never lands in another's.
func TestProvisionRelocatesWorkDirs(t *testing.T) {
	g := loadGolden(t)
	req := goldenRequest(t, g)
	a := adapter.New(Profile{})
	d := a.Describe()
	if !d.Has(contract.CapSessionWorkDir) {
		t.Fatal("the profile declares no session_workdir")
	}
	src := req.Layout
	src.Workspace = "/srv/old/ws"
	req.Load = &contract.LoadSource{Format: adapter.ArchiveFormat, Harness: d.Harness, Layout: src, Workspace: src.Workspace, WorkDirs: []string{"wt/a", "wt/b_c"}}
	res, err := a.Provision(req)
	if err != nil {
		t.Fatal(err)
	}
	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace
	want := []contract.Relocation{
		{From: contract.RootPath{Root: contract.RootConfig, Path: "projects/-srv-old-ws"}, To: contract.RootPath{Root: contract.RootConfig, Path: "projects/" + enc(req.Layout.Workspace)}},
		{From: contract.RootPath{Root: contract.RootConfig, Path: "projects/-srv-old-ws-wt-a"}, To: contract.RootPath{Root: contract.RootConfig, Path: "projects/" + enc(req.Layout.Workspace+"/wt/a")}},
		{From: contract.RootPath{Root: contract.RootConfig, Path: "projects/-srv-old-ws-wt-b-c"}, To: contract.RootPath{Root: contract.RootConfig, Path: "projects/" + enc(req.Layout.Workspace+"/wt/b_c")}},
	}
	if !reflect.DeepEqual(res.HistoryRelocations, want) {
		t.Errorf("history_relocations = %+v, want %+v", res.HistoryRelocations, want)
	}
	if moved := contract.Relocate(res.HistoryRelocations, contract.RootPath{Root: contract.RootConfig, Path: "projects/-srv-old-ws-wt-a/s.jsonl"}); moved.Path != want[1].To.Path+"/s.jsonl" {
		t.Errorf("a transcript of wt/a moves to %s", moved)
	}
	req.Contract = "harness-adapter/1.8"
	if _, err := a.Provision(req); contract.CodeOf(err) != contract.CodeProtocol {
		t.Errorf("work_dirs in a 1.8 request: %v, want protocol", err)
	}
}

// A Session's own working directory is where claude runs and its hooks
// resolve from; the agent's open_config is left as it was.
func TestOpenConfigIn(t *testing.T) {
	cfg := openConfig{WorkingDir: "/w", Env: []string{"HOME=/h", harnesscore.EnvHookCwd + "=/w", "X=1"}}
	if got := cfg.In(""); !reflect.DeepEqual(got, cfg) {
		t.Errorf("In(\"\") = %+v", got)
	}
	got := cfg.In("/w/wt/a")
	if got.WorkingDir != "/w/wt/a" || !reflect.DeepEqual(got.Env, []string{"HOME=/h", "X=1", harnesscore.EnvHookCwd + "=/w/wt/a"}) {
		t.Errorf("In(/w/wt/a) = %+v", got)
	}
	if cfg.WorkingDir != "/w" || cfg.Env[1] != harnesscore.EnvHookCwd+"=/w" {
		t.Errorf("In changed the agent's open_config: %+v", cfg)
	}
}

// An http connector's headers_file reaches claude through a headersHelper
// script beside mcp.json: the script holds paths, no value, and prints each
// header's value from its file as JSON, escaped, each time claude connects
// and runs the helper in a shell. Like every provisioned file it is not
// executable.
func TestProvisionHeadersFromFiles(t *testing.T) {
	dir, config := t.TempDir(), filepath.Join(t.TempDir(), "it's config")
	token, odd := filepath.Join(dir, "token"), filepath.Join(dir, "it's odd")
	if err := os.WriteFile(token, []byte("Bearer sk-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(odd, []byte("a \"quoted\" \\ value\twith a tab"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := contract.ProvisionRequest{
		Contract: contract.Version, HarnessRoot: t.TempDir(),
		Layout: contract.Layout{Home: "/w/home", Config: config, Workspace: "/w/workspace", Secrets: "/w/secrets", Scratch: "/w/spool"},
		Spec: contract.AgentSpec{
			PermissionPosture: contract.PostureBypass,
			Connectors: []contract.Connector{{Name: "docs", HTTP: &contract.HTTPConnector{
				URL: "https://mcp.example.com/mcp", Headers: map[string]string{"X-Team": "t"},
				HeadersFile: map[string]string{"Authorization": token, "X-Odd": odd},
			}}},
		},
	}
	res, err := (Profile{}).Provision(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("the result: %v", err)
	}
	var mcp struct {
		MCPServers map[string]struct {
			Headers       map[string]string `json:"headers"`
			HeadersHelper string            `json:"headersHelper"`
		} `json:"mcpServers"`
	}
	var script contract.File
	for _, f := range res.Files {
		switch f.Path {
		case mcpFile:
			if err := json.Unmarshal(f.Content(), &mcp); err != nil {
				t.Fatal(err)
			}
		case headersDir + "/docs.sh":
			script = f
		}
	}
	srv := mcp.MCPServers["docs"]
	if len(srv.Headers) != 1 || srv.Headers["X-Team"] != "t" {
		t.Fatalf("the server %+v", srv)
	}
	if script.Path == "" || script.Mode != "0600" || strings.Contains(string(script.Content()), "sk-1") {
		t.Fatalf("the helper %q, mode %s:\n%s", script.Path, script.Mode, script.Content())
	}
	sh := filepath.Join(config, script.Path)
	if err := os.MkdirAll(filepath.Dir(sh), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sh, script.Content(), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command("/bin/sh", "-c", srv.HeadersHelper)
	out, err := helper.Output()
	if err != nil {
		t.Fatalf("the helper: %v", err)
	}
	var headers map[string]string
	if err := json.Unmarshal(out, &headers); err != nil {
		t.Fatalf("the helper's output %q: %v", out, err)
	}
	want := map[string]string{"Authorization": "Bearer sk-1", "X-Odd": "a \"quoted\" \\ value\twith a tab"}
	if !reflect.DeepEqual(headers, want) {
		t.Errorf("the helper's headers %q, want %q", headers, want)
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/bin/sh", "-c", srv.HeadersHelper).Run(); err == nil {
		t.Error("the helper succeeded with a header's file gone")
	}
}
