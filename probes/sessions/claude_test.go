package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
	tclaude "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

const (
	// envClaude names the pinned claude binary, as the profile's own tests do.
	envClaude = "HW_REAL_CLAUDE"
	// envClaudeHook names a built claude-code-hook, for a host without the go
	// tool; otherwise it is built from this tree.
	envClaudeHook = "HW_CLAUDE_CODE_HOOK"
	// envClaudeToken names a file holding a `claude setup-token` token: the
	// live run's credential.
	envClaudeToken = "HW_SESSIONS_CLAUDE_TOKEN_FILE" //nolint:gosec // an environment variable's name
	// envClaudeModel names the live run's model; haiku otherwise.
	envClaudeModel = "HW_SESSIONS_CLAUDE_MODEL"
)

const persona = "You are the agent of a probe that runs several conversations at once. Do exactly what each message asks, and be brief."

// TestClaudeSessions runs N Claude Code Sessions side by side in one
// environment against the mock model API.
func TestClaudeSessions(t *testing.T) { runClaude(t, modeMock) }

// TestClaudeSessionsLive does the same against Anthropic's API, with the
// token envClaudeToken names; it spends a few hundred short turns of haiku.
func TestClaudeSessionsLive(t *testing.T) { runClaude(t, modeLive) }

// claudeRun is one run: one agent's environment, and its Sessions.
type claudeRun struct {
	t      *testing.T
	mode   mode
	ad     contract.Adapter
	l      contract.Layout
	res    contract.ProvisionResult
	cred   *contract.CredentialFile
	rss    *rssSampler
	ctx    context.Context
	turnIn time.Duration // how long a turn may take
}

func mockSteps() []step {
	return []step{
		{scenario: "PING", text: "PING %[1]s", want: "PONG %[1]s"},
		{scenario: "TOOL", text: "TOOL echo %[1]s | tee -a probe-%[1]s.txt", want: "TOOL DONE"},
		{scenario: "AGENT", text: "AGENT PING sub-%[1]s", want: "TOOL DONE"},
		{scenario: "BG", text: "BG sleep 1; echo bg-%[1]s", want: "TOOL DONE", own: true},
		{scenario: "SLOW", text: "SLOW 8", want: "slow7"},
	}
}

func liveSteps() []step {
	return []step{
		{scenario: "PING", text: "Reply with exactly PONG-%[1]s and nothing else.", want: "PONG-%[1]s"},
		{scenario: "TOOL", text: "Use the Bash tool to run exactly this command: echo %[1]s | tee -a probe-%[1]s.txt — then reply with the command's output and nothing else.", want: "%[1]s"},
		{scenario: "AGENT", text: "Use the Agent tool, subagent_type general-purpose, to ask a subagent to reply with exactly SUB-%[1]s and nothing else. Then reply with exactly what it answered.", want: "SUB-%[1]s"},
		{scenario: "BG", text: "Use the Bash tool with run_in_background set to true to run exactly this command: sleep 2; echo bg-%[1]s — then reply with exactly STARTED and nothing else.", want: "STARTED", own: true},
		{scenario: "SLOW", text: "Count from 1 to 20, one number per line, and nothing else.", want: "20"},
	}
}

func runClaude(t *testing.T, m mode) {
	bin := os.Getenv(envClaude)
	if bin == "" {
		t.Skipf("%s does not name a claude binary", envClaude)
	}
	if m == modeLive && os.Getenv(envClaudeToken) == "" {
		t.Skipf("%s names no token file: the live run is pending", envClaudeToken)
	}
	version := claudeVersion(t, bin)
	n, dur := sessionCount(t), runFor(t)
	started := time.Now()
	work := workDir(t)
	root := filepath.Join(work, "dist")
	audit := filepath.Join(work, "hook-audit")
	distribution(t, root, bin, audit)
	l := layoutUnder(t, filepath.Join(work, "env"))

	var mock *mockapi.Server
	if m == modeMock {
		mock = mockapi.Start()
		mock.KeepRequests = 1000 // each request is kept with its system prompt
		defer mock.Close()
	}
	mcp := newMCPServer()
	defer mcp.close()
	header := "Bearer " + newID("hdr")
	headerFile := filepath.Join(l.Secrets, "mcp-authorization")
	if err := os.WriteFile(headerFile, []byte(header+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	spec := contract.AgentSpec{
		Instructions:      contract.Instructions{Persona: persona, Workspace: "# Workspace\n"},
		PermissionPosture: contract.PostureBypass,
		Connectors: []contract.Connector{{Name: "probe", HTTP: &contract.HTTPConnector{
			URL: mcp.url(), HeadersFile: map[string]string{"Authorization": headerFile},
		}}},
	}
	steps, turnIn := mockSteps(), 2*time.Minute
	if m == modeLive {
		spec.Model = "haiku"
		if v := os.Getenv(envClaudeModel); v != "" {
			spec.Model = v
		}
		steps, turnIn = liveSteps(), 5*time.Minute
	}
	ad := adapter.New(claudecode.Profile{})
	res, err := ad.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: root, Layout: l, Spec: spec})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if mock != nil {
		res.OpenConfig = editOpenConfig(t, res.OpenConfig, func(c *claudeOpenConfig) {
			c.Env = append(c.Env, "ANTHROPIC_BASE_URL="+mock.URL(), "CLAUDE_CODE_MAX_RETRIES=2")
		})
	}
	if err := conformance.Apply(l, res); err != nil {
		t.Fatalf("applying the result: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &claudeRun{t: t, mode: m, ad: ad, l: l, res: res, cred: credential(t, m, l), rss: &rssSampler{}, ctx: ctx, turnIn: turnIn}
	go r.rss.run(ctx)

	// What the profile rendered in .claude.json must stay. But claude
	// rewrites its own file, and numStartups is its count: a control Session,
	// alone, shows what claude changes with no other Session there, and that
	// is not held against the run.
	cj := filepath.Join(l.Config, ".claude.json")
	rendered := readJSON(t, cj)
	control := r.open("control")
	if control.openErr != "" {
		t.Fatalf("the control Session: %s", control.openErr)
	}
	d, err := control.turn(ctx, firstStep(steps, "PING", "control"), turnIn)
	control.count("PING", d, err, "")
	closeAll(ctx, []*session{control})
	alone := readJSON(t, cj)
	var keep []entry
	var dropped []string
	for _, e := range renderedEntries(rendered, map[string]bool{"numStartups": true}) {
		if got, ok := lookup(alone, e.path); ok && fmt.Sprint(got) == fmt.Sprint(e.want) {
			keep = append(keep, e)
		} else {
			dropped = append(dropped, fmt.Sprintf("%s: rendered %v, then %v", e, e.want, got))
		}
	}
	watch := &fileWatch{path: cj, keep: keep}
	go watch.run(ctx)

	// Every Session opens at once.
	sessions := make([]*session, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			sessions[i] = r.open(fmt.Sprintf("s%d", i))
		}()
	}
	close(gate)
	wg.Wait()

	// Each takes turns until the run's time is up.
	deadline := time.Now().Add(dur)
	for _, s := range sessions {
		if s.openErr != "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			takeTurns(r.ctx, s, steps, deadline, r.turnIn)
		}()
	}
	wg.Wait()

	// At the end, a Session opened now must still get its connector's header.
	before := mcp.count()
	final := r.open("final")
	headerOK := false
	if final.openErr == "" {
		d, err := final.turn(ctx, firstStep(steps, "PING", "final"), turnIn)
		final.count("PING", d, err, "")
		for wait := time.Now().Add(30 * time.Second); time.Now().Before(wait); time.Sleep(200 * time.Millisecond) {
			if with, _ := mcp.heard(before, "Authorization", header); with > 0 {
				headerOK = true
				break
			}
		}
	}
	initialHeaders, mcpAll := mcp.heard(0, "Authorization", header)

	r.rss.sample()
	closeAll(ctx, append(append([]*session(nil), sessions...), final))
	time.Sleep(time.Second) // the watcher's last look at .claude.json
	cancel()

	everyone := append(append([]*session{control}, sessions...), final)
	rep := judgeClaude(r, everyone, watch.snapshot(), audit, headerOK, initialHeaders, mcpAll, mock)
	rep.Version, rep.Took = version, time.Since(started).Round(time.Second).String()
	rep.Duration, rep.Sessions, rep.ClaudeDrops, rep.HostPeak = dur.String(), n, dropped, selfPeak()
	name := fmt.Sprintf("claude-%s-n%d", m, n)
	writeEvidence(t, name, rep, rep.markdown())
	t.Log("\n" + rep.markdown())
	for _, c := range rep.Criteria {
		if !c.Pass {
			t.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
}

func firstStep(steps []step, scenario, tag string) string {
	for _, s := range steps {
		if s.scenario == scenario {
			return fmt.Sprintf(s.text, tag)
		}
	}
	return "PING " + tag
}

// open opens a fresh Session under a new id, with a hook spool of its own.
func (r *claudeRun) open(name string) *session {
	spool := filepath.Join(r.l.Scratch, "spool", name)
	s := newSession(name, sessionid.NewUUID(), spool)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		s.openErr = err.Error()
		return s
	}
	cs, err := r.ad.NewSession(contract.OpenRequest{
		Mode: contract.OpenFresh, SessionID: s.id, Layout: r.l, Credential: r.cred,
		OpenConfig: editOpenConfig(r.t, r.res.OpenConfig, func(c *claudeOpenConfig) {
			for i, e := range c.Env {
				if strings.HasPrefix(e, "HW_EVENT_SPOOL=") {
					c.Env[i] = "HW_EVENT_SPOOL=" + spool
				}
			}
			c.Spool = spool
		}),
	})
	if err != nil {
		s.openErr = "NewSession: " + err.Error()
		return s
	}
	s.s = cs
	go s.observe(r.ctx)
	r.rss.add(s)
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	defer cancel()
	t0 := time.Now()
	if _, err := cs.Open(ctx); err != nil {
		s.openErr = "Open: " + err.Error()
	}
	s.openTook = time.Since(t0)
	return s
}

func claudeVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	pin, _ := versions.Pinned(claudecode.Name)
	got := strings.Fields(string(out))
	if len(got) == 0 || got[0] != pin {
		t.Fatalf("%s is claude %q; the profile is pinned to %s", bin, strings.TrimSpace(string(out)), pin)
	}
	return got[0]
}

// distribution lays agentd's claude-code distribution out under root: the
// claude binary, and as bin/claude-code-hook a wrapper that keeps a copy of
// each hook payload in audit — named for the spool it goes to — before it
// runs the real hook helper on it.
func distribution(t *testing.T, root, bin, audit string) {
	t.Helper()
	for _, d := range []string{filepath.Join(root, "bin"), audit} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(bin, claudecode.BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	hook := os.Getenv(envClaudeHook)
	if hook == "" {
		hook = filepath.Join(filepath.Dir(root), "claude-code-hook")
		build := exec.Command("go", "build", "-o", hook, "github.com/olesho/harness-wrapper/cmd/claude-code-hook")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("building the hook helper: %v\n%s", err, out)
		}
	}
	wrapper := fmt.Sprintf(`#!/bin/sh
# The sessions probe's hook wrapper: a copy of each payload, named for the
# spool HW_EVENT_SPOOL names, then the hook helper on it.
f=$(mktemp) || exit 0
cat >"$f"
dst=$(mktemp %q/"$(basename "${HW_EVENT_SPOOL:-none}")".XXXXXXXX) && cp "$f" "$dst"
exec <"$f"
rm -f "$f"
exec %q "$@"
`, audit, hook)
	if err := os.WriteFile(claudecode.HookPath(root), []byte(wrapper), 0o755); err != nil { //nolint:gosec // an executable the harness runs
		t.Fatal(err)
	}
}

// credential stages the token where agentd's launcher does: a file named for
// its kind in the secrets root. The mock takes any placeholder.
func credential(t *testing.T, m mode, l contract.Layout) *contract.CredentialFile {
	t.Helper()
	token := "mock-placeholder-not-a-credential"
	if m == modeLive {
		b, err := os.ReadFile(os.Getenv(envClaudeToken))
		if err != nil {
			t.Fatalf("the live credential: %v", err)
		}
		token = strings.TrimSpace(string(b))
	}
	file := filepath.Join(l.Secrets, claudecode.CredentialKind)
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &contract.CredentialFile{Kind: claudecode.CredentialKind, File: file}
}

// claudeOpenConfig is the Claude Code profile's open_config, which the probe
// rewrites: the mock's address, and each Session's own spool.
type claudeOpenConfig struct {
	Binary     string   `json:"binary"`
	Args       []string `json:"args"`
	Env        []string `json:"env"`
	WorkingDir string   `json:"working_dir"`
	Spool      string   `json:"spool"`
}

// readJSON reads a JSON object from a file.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// closeAll closes the Sessions at once, as their Hosts would, and keeps what
// each Close established.
func closeAll(ctx context.Context, sessions []*session) {
	var wg sync.WaitGroup
	for _, s := range sessions {
		if s.s == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			res, err := s.s.Close(cctx, contract.ClosePark, 5*time.Second)
			if err != nil {
				s.note("close: %v", err)
			}
			s.closed = &res
		}()
	}
	wg.Wait()
}

func editOpenConfig(t *testing.T, raw []byte, edit func(*claudeOpenConfig)) []byte {
	t.Helper()
	var c claudeOpenConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("open_config: %v", err)
	}
	c.Env = append([]string(nil), c.Env...)
	edit(&c)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// count is how many requests the MCP server has had.
func (m *mcpServer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.seen)
}

// ---- judging the run

type claudeReport struct {
	Harness     string            `json:"harness"`
	Version     string            `json:"version"`
	Mode        mode              `json:"mode"`
	Platform    string            `json:"platform"`
	Sessions    int               `json:"sessions"`
	Duration    string            `json:"duration"`
	Took        string            `json:"took"`
	Criteria    []criterion       `json:"criteria"`
	PerSession  []sessionReport   `json:"per_session"`
	ClaudeJSON  fileStats         `json:"claude_json"`
	ClaudeDrops []string          `json:"claude_changes_alone,omitempty"`
	HostPeak    string            `json:"host_peak_rss"`
	PeakAllRSS  string            `json:"peak_all_rss"`
	RSSSamples  int               `json:"rss_samples"`
	HookEvents  map[string]int    `json:"hook_events_by_spool"`
	HookKinds   map[string]int    `json:"hook_events_by_kind"`
	Misrouted   []string          `json:"misrouted,omitempty"`
	Transcripts map[string]string `json:"transcripts"`
	MCPHeaders  int               `json:"mcp_requests_with_header"`
	MCPRequests int               `json:"mcp_requests"`
	ModelCalls  int               `json:"model_requests,omitempty"`
}

type sessionReport struct {
	Name      string                `json:"name"`
	ID        string                `json:"id"`
	OpenMS    int64                 `json:"open_ms"`
	OpenErr   string                `json:"open_error,omitempty"`
	Turns     map[string]*turnStats `json:"turns"`
	OwnTurns  int                   `json:"own_turns"`
	ToolUses  int                   `json:"tool_uses"`
	Started   int                   `json:"tool_started"`
	Finished  int                   `json:"tool_finished"`
	Foreign   []string              `json:"foreign_tool_events,omitempty"`
	Unheard   []string              `json:"tool_uses_without_hooks,omitempty"`
	Subagents int                   `json:"subagents_started"`
	SubsEnded int                   `json:"subagents_stopped"`
	PeakRSS   string                `json:"peak_rss"`
	Batches   int                   `json:"batches"`
	Items     int                   `json:"items"`
	PeakBatch int                   `json:"peak_batches_per_second"`
	Faults    []string              `json:"faults,omitempty"`
	Notes     []string              `json:"notes,omitempty"`
	Closed    *contract.CloseResult `json:"closed,omitempty"`
}

func judgeClaude(r *claudeRun, everyone []*session, cj fileStats, audit string, headerOK bool, headers, mcpAll int, mock *mockapi.Server) *claudeReport {
	rep := &claudeReport{
		Harness: claudecode.Name, Mode: r.mode, Platform: runtime.GOOS + "-" + runtime.GOARCH,
		ClaudeJSON: cj, MCPHeaders: headers, MCPRequests: mcpAll,
		Transcripts: map[string]string{},
	}
	r.rss.mu.Lock()
	rep.PeakAllRSS, rep.RSSSamples = mib(r.rss.PeakAll), r.rss.Samples
	r.rss.mu.Unlock()
	if mock != nil {
		rep.ModelCalls = mock.Count()
	}

	// Starts.
	var openFail []string
	var slowest time.Duration
	for _, s := range everyone {
		if s.openErr != "" {
			openFail = append(openFail, s.name+": "+s.openErr)
		}
		slowest = max(slowest, s.openTook)
	}
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "no start hangs", Pass: len(openFail) == 0,
		Detail: fmt.Sprintf("a control Session alone, %d at once, one at the end; the slowest open took %s. %s", len(everyone)-2, slowest.Round(time.Millisecond), strings.Join(openFail, "; ")),
	})

	// .claude.json.
	rep.Criteria = append(rep.Criteria, criterion{
		Name: ".claude.json parses after every write and keeps what was rendered",
		Pass: cj.Torn == 0 && cj.Corrupt == 0 && cj.Lost == 0 && cj.Missing == 0,
		Detail: fmt.Sprintf("%d states read: %d torn, %d corrupt, %d missing a rendered entry, %d reads found no file",
			cj.Changes, cj.Torn, cj.Corrupt, cj.Lost, cj.Missing),
	})

	// Hook events: each in its own Session's spool, none lost.
	owner := map[string]string{}
	for _, s := range everyone {
		owner[s.name] = s.id
	}
	byKind, bySpool, misrouted := hookAudit(audit, owner)
	rep.HookEvents, rep.HookKinds, rep.Misrouted = bySpool, byKind, misrouted
	var foreign, unheard int
	for _, s := range everyone {
		sr := s.report()
		rep.PerSession = append(rep.PerSession, sr)
		foreign += len(sr.Foreign)
		unheard += len(sr.Unheard)
	}
	events := 0
	for _, v := range bySpool {
		events += v
	}
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "every hook event lands in its own Session's spool",
		Pass: events > 0 && len(misrouted) == 0 && foreign == 0 && unheard == 0,
		Detail: fmt.Sprintf("%d hook payloads: %d in another Session's spool; %d tool events a Session reported for another's tool use; %d tool uses with no hook event",
			events, len(misrouted), foreign, unheard),
	})

	// Transcripts.
	tp := transcriptCheck(r.l, everyone, rep.Transcripts)
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "every transcript parses and holds only its own Session", Pass: len(tp) == 0,
		Detail: fmt.Sprintf("%d transcripts read. %s", len(everyone), strings.Join(tp, "; ")),
	})

	// headersHelper, at the end.
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "a connector's headersHelper still runs at the end", Pass: headerOK,
		Detail: fmt.Sprintf("the Session opened last sent the header from its file: %v; %d of the MCP server's %d requests carried it", headerOK, headers, mcpAll),
	})

	// Turns: not a criterion of the plan's, but a failed turn says why. A
	// turn of claude's own that did not follow a background command's end
	// is no failed turn: it is counted apart.
	sent, failed, missed, waited := turnTotals(everyone)
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "every turn completed", Pass: failed == 0 && sent > 0,
		Detail: fmt.Sprintf("%d input turns, %d failed; after %d background commands claude took a turn of its own within the wait, but for %d", sent, failed, waited, missed),
	})
	return rep
}

func (s *session) report() sessionReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr := sessionReport{
		Name: s.name, ID: s.id, OpenMS: s.openTook.Milliseconds(), OpenErr: s.openErr, Turns: s.turns,
		OwnTurns: len(s.own), ToolUses: len(s.toolUse), Started: len(s.started), Finished: len(s.finished),
		Subagents: len(s.subStart), SubsEnded: len(s.subStop), PeakRSS: mib(s.peakRSS),
		Faults: s.faults, Notes: s.notes, Closed: s.closed,
		Batches: len(s.batches), Items: s.items, PeakBatch: peakPerSecond(s.batches),
	}
	for id := range s.started {
		if _, ok := s.toolUse[id]; !ok {
			sr.Foreign = append(sr.Foreign, "started "+id)
		}
	}
	for id := range s.finished {
		if _, ok := s.toolUse[id]; !ok {
			sr.Foreign = append(sr.Foreign, "finished "+id)
		}
	}
	for id, name := range s.toolUse {
		if !s.started[id] || !s.finished[id] {
			sr.Unheard = append(sr.Unheard, fmt.Sprintf("%s %s (started %v, finished %v)", name, id, s.started[id], s.finished[id]))
		}
	}
	sort.Strings(sr.Foreign)
	sort.Strings(sr.Unheard)
	return sr
}

// hookAudit reads the wrapper's copies of the hook payloads: each must name
// the Session whose spool it went to.
func hookAudit(dir string, owner map[string]string) (byKind, bySpool map[string]int, misrouted []string) {
	byKind, bySpool = map[string]int{}, map[string]int{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		spool, _, _ := strings.Cut(e.Name(), ".")
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p struct {
			SessionID string `json:"session_id"`
			Event     string `json:"hook_event_name"`
		}
		if json.Unmarshal(b, &p) != nil {
			misrouted = append(misrouted, e.Name()+": not JSON")
			continue
		}
		bySpool[spool]++
		byKind[p.Event]++
		if want, ok := owner[spool]; !ok || p.SessionID != want {
			if len(misrouted) < 20 {
				misrouted = append(misrouted, fmt.Sprintf("%s in spool %s names Session %s", p.Event, spool, p.SessionID))
			}
		}
	}
	return byKind, bySpool, misrouted
}

// transcriptCheck reads each Session's transcript: every line JSON, every
// entry of its own Session; and no transcript in the project's directory
// belongs to none of them.
func transcriptCheck(l contract.Layout, everyone []*session, out map[string]string) []string {
	var problems []string
	known := map[string]bool{}
	var dir string
	for _, s := range everyone {
		known[s.id] = true
		path, err := tclaude.Locate(s.id, l.Workspace, []string{"CLAUDE_CONFIG_DIR=" + l.Config})
		if err != nil {
			problems = append(problems, s.name+": "+err.Error())
			continue
		}
		dir = filepath.Dir(path)
		b, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, s.name+": "+err.Error())
			continue
		}
		lines, foreign, bad := 0, 0, 0
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			lines++
			var e struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal([]byte(line), &e) != nil {
				bad++
				continue
			}
			if e.SessionID != "" && e.SessionID != s.id {
				foreign++
			}
		}
		out[s.name] = fmt.Sprintf("%d lines, %d not JSON, %d of another Session", lines, bad, foreign)
		if bad > 0 || foreign > 0 {
			problems = append(problems, s.name+": "+out[s.name])
		}
	}
	if dir != "" {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			id, ok := strings.CutSuffix(e.Name(), ".jsonl")
			if ok && !e.IsDir() && !known[id] {
				problems = append(problems, "a transcript of no Session of the run: "+e.Name())
			}
		}
	}
	return problems
}

func (rep *claudeReport) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s %s: %d Sessions side by side (%s)\n\n", rep.Harness, rep.Version, rep.Sessions, rep.Mode)
	fmt.Fprintf(&b, "%s, turns for %s; the run took %s. Peak memory of every Session's harness tree together: %s (%d samples); of the probe's own process, which hosts every Session's adapter and the mock: %s.\n\n",
		rep.Platform, rep.Duration, rep.Took, rep.PeakAllRSS, rep.RSSSamples, rep.HostPeak)
	b.WriteString("| Criterion | Result | Detail |\n|---|---|---|\n")
	for _, c := range rep.Criteria {
		res := "pass"
		if !c.Pass {
			res = "**fail**"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.Name, res, strings.ReplaceAll(c.Detail, "|", "/"))
	}
	b.WriteString("\n| Session | Open | Turns completed / sent | Own turns | Tool uses / started / finished | Subagents | Peak memory | Batches (peak a second) |\n|---|---|---|---|---|---|---|---|\n")
	for _, s := range rep.PerSession {
		var sent, done int
		for _, st := range s.Turns {
			sent += st.Sent
			done += st.Completed
		}
		fmt.Fprintf(&b, "| %s | %d ms | %d / %d | %d | %d / %d / %d | %d | %s | %d (%d) |\n",
			s.Name, s.OpenMS, done, sent, s.OwnTurns, s.ToolUses, s.Started, s.Finished, s.Subagents, s.PeakRSS, s.Batches, s.PeakBatch)
	}
	fmt.Fprintf(&b, "\n.claude.json: %d states read, %d torn, %d corrupt, %d missing a rendered entry. What claude changed of it alone, in the control Session, is not held against the run:\n", rep.ClaudeJSON.Changes, rep.ClaudeJSON.Torn, rep.ClaudeJSON.Corrupt, rep.ClaudeJSON.Lost)
	for _, d := range rep.ClaudeDrops {
		fmt.Fprintf(&b, "- alone: %s\n", d)
	}
	for _, e := range rep.ClaudeJSON.Examples {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	kinds := make([]string, 0, len(rep.HookKinds))
	for k, v := range rep.HookKinds {
		kinds = append(kinds, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(kinds)
	fmt.Fprintf(&b, "\nHook payloads by event: %s.\n", strings.Join(kinds, ", "))
	for _, m := range rep.Misrouted {
		fmt.Fprintf(&b, "- misrouted: %s\n", m)
	}
	for _, s := range rep.PerSession {
		for _, f := range s.Foreign {
			fmt.Fprintf(&b, "- %s reported another Session's tool event: %s\n", s.Name, f)
		}
		for _, u := range s.Unheard {
			fmt.Fprintf(&b, "- %s: a tool use without its hook events: %s\n", s.Name, u)
		}
		for _, f := range s.Faults {
			fmt.Fprintf(&b, "- %s fault: %s\n", s.Name, f)
		}
		for _, n := range s.Notes {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, n)
		}
		for sc, st := range s.Turns {
			for _, f := range st.Failures {
				fmt.Fprintf(&b, "- %s %s failed: %s\n", s.Name, sc, f)
			}
		}
	}
	return b.String()
}
