package claudecode

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

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
		Layout: contract.Layout{Home: "/w/home", Config: "/w/profile", Workspace: "/w/workspace", Secrets: "/w/secrets", Scratch: "/w/scratch"},
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
// under this profile's owner; the spool is in the scratch root; agentd's own
// variables and the yield file are gone; PATH and LANG are the Host's, added
// at launch; and the model, effort and permission flags hw added at launch
// are in the arguments.
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
	drop := map[string]bool{"PATH": true, "LANG": true, "AGENTD_AGENT_ID": true, "AGENTD_LAUNCH_ID": true, "HW_YIELD_FILE": true, "HW_EVENT_SPOOL": true}
	var wantEnv, gotEnv []string
	for _, kv := range g.Env {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			wantEnv = append(wantEnv, kv)
		}
	}
	for _, kv := range cfg.Env {
		if k, v, _ := strings.Cut(kv, "="); k == "HW_EVENT_SPOOL" {
			if v != "/w/scratch/spool" || cfg.Spool != v {
				t.Errorf("spool %s (open_config %s), want /w/scratch/spool", v, cfg.Spool)
			}
			continue
		}
		gotEnv = append(gotEnv, kv)
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
		"gated posture": func(r *contract.ProvisionRequest) { r.Spec.PermissionPosture = contract.PostureGated },
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
