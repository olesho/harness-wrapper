package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/codex"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
	tcodex "github.com/olesho/harness-wrapper/pkg/transcript/codex"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// envCodex names the pinned codex binary — the native one — as the profile's
// own tests do.
const envCodex = "HW_REAL_CODEX"

// TestCodexSessions runs N Codex Sessions side by side in one environment
// against the mock model API, each in a `codex app-server` of its own, as
// the profile runs them. It starts them twice: all at once on a fresh
// CODEX_HOME, and behind a start lock — one Session first, alone, to
// initialize the home, then the rest at once — which is the change the plan
// asks of the profile, made here by ordering the opens. The second set then
// takes turns for the run's duration.
func TestCodexSessions(t *testing.T) {
	bin := os.Getenv(envCodex)
	if bin == "" {
		t.Skipf("%s does not name a codex binary", envCodex)
	}
	version := codexVersion(t, bin)
	n, dur := sessionCount(t), runFor(t)
	started := time.Now()
	work := workDir(t)
	root := filepath.Join(work, "dist")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, codex.BinaryPath(root)); err != nil {
		t.Fatal(err)
	}
	mock := mockapi.Start()
	mock.KeepRequests = 64 // each request is kept with its whole conversation: codex resends it every time
	defer mock.Close()
	ad := adapter.New(codex.Profile{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	procs := &procSampler{match: func(cmd string) bool {
		return strings.Contains(cmd, "codex") && strings.Contains(cmd, "app-server")
	}}
	go procs.run(ctx)
	rep := &codexReport{
		Harness: codex.Name, Version: version, Mode: modeMock, Platform: runtime.GOOS + "-" + runtime.GOARCH,
		Sessions: n, Duration: dur.String(),
	}

	// 1. No lock: every Session's app-server starts at once on a fresh home.
	fresh := newCodexEnv(t, ad, root, mock, filepath.Join(work, "fresh"))
	all := fresh.openAll(ctx, n, "fresh")
	for _, s := range all {
		if s.openErr != "" {
			rep.FreshFailures = append(rep.FreshFailures, s.name+": "+clip(s.openErr, 300))
		}
	}
	closeAll(ctx, all)

	// 2. Behind a start lock: one Session first, alone, then the rest at once.
	env := newCodexEnv(t, ad, root, mock, filepath.Join(work, "env"))
	control := env.open(ctx, "control")
	if control.openErr != "" {
		t.Fatalf("the control Session: %s", control.openErr)
	}
	sessions := env.openAll(ctx, n, "s")
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	for _, s := range append([]*session{control}, sessions...) {
		if s.openErr != "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			takeTurns(ctx, s, codexSteps, deadline, 2*time.Minute)
		}()
	}
	wg.Wait()
	everyone := append([]*session{control}, sessions...)
	procs.sample()
	heapProfile(t)
	closeAll(ctx, everyone)
	cancel()

	judgeCodex(t, rep, env.l, everyone, procs)
	rep.Took = time.Since(started).Round(time.Second).String()
	rep.ModelCalls = mock.Count()
	rep.HostPeak = selfPeak()
	name := fmt.Sprintf("codex-mock-n%d", n)
	md := rep.markdown()
	writeEvidence(t, name, rep, md)
	t.Log("\n" + md)
	for _, c := range rep.Criteria {
		if !c.Pass {
			t.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
}

var codexSteps = []step{
	{scenario: "PING", text: "PING %[1]s", want: "PONG %[1]s"},
	{scenario: "TOOL", text: "TOOL echo %[1]s | tee -a probe-%[1]s.txt", want: "TOOL DONE"},
	{scenario: "SLOW", text: "SLOW 8", want: "slow7"},
	// A goal: codex records it in its goals database and works it in two
	// turns of its own, then marks it complete. The tag keeps each
	// Session's goal apart in the mock's count.
	{scenario: "GOAL", text: "MKGOAL GOAL 2 4 %[1]s", own: true},
}

func codexVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	pin, _ := versions.Pinned(codex.Name)
	f := strings.Fields(string(out))
	if len(f) == 0 || f[len(f)-1] != pin {
		t.Fatalf("%s is codex %q; the profile is pinned to %s", bin, strings.TrimSpace(string(out)), pin)
	}
	return f[len(f)-1]
}

// codexEnv is one agent's environment: its roots, its configuration, its key.
type codexEnv struct {
	t    *testing.T
	ad   contract.Adapter
	l    contract.Layout
	res  contract.ProvisionResult
	cred *contract.CredentialFile
}

func newCodexEnv(t *testing.T, ad contract.Adapter, root string, mock *mockapi.Server, dir string) *codexEnv {
	t.Helper()
	l := layoutUnder(t, dir)
	spec := contract.AgentSpec{
		Instructions:      contract.Instructions{Persona: persona, Workspace: "# Workspace\n"},
		PermissionPosture: contract.PostureBypass,
		// codex's default model speaks a form of the Responses API that
		// sends no tools: against the mock the model is named "mock".
		Model: "mock",
	}
	res, err := ad.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: root, Layout: l, Spec: spec})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	pointCodexAt(t, mock, &res)
	if err := conformance.Apply(l, res); err != nil {
		t.Fatalf("applying the result: %v", err)
	}
	file := filepath.Join(l.Secrets, "openai-api-key")
	if err := os.WriteFile(file, []byte("sk-mock-placeholder-not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &codexEnv{t: t, ad: ad, l: l, res: res, cred: &contract.CredentialFile{Kind: codex.CredentialAPIKey, File: file}}
}

// pointCodexAt gives the rendered config.toml a model provider of its own at
// the mock, as the profile's own tests do.
func pointCodexAt(t *testing.T, mock *mockapi.Server, r *contract.ProvisionResult) {
	t.Helper()
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
		return
	}
	t.Fatal("no config.toml rendered")
}

// open opens a fresh Session; codex chooses its thread's id.
func (e *codexEnv) open(ctx context.Context, name string) *session {
	s := newSession(name, "", "")
	cs, err := e.ad.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, OpenConfig: e.res.OpenConfig, Layout: e.l, Credential: e.cred})
	if err != nil {
		s.openErr = "NewSession: " + err.Error()
		return s
	}
	s.s = cs
	go s.observe(ctx)
	octx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	t0 := time.Now()
	res, err := cs.Open(octx)
	s.openTook = time.Since(t0)
	if err != nil {
		s.openErr = "Open: " + err.Error()
		return s
	}
	s.id = res.SessionID
	return s
}

// openAll opens n Sessions at once.
func (e *codexEnv) openAll(ctx context.Context, n int, prefix string) []*session {
	out := make([]*session, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			out[i] = e.open(ctx, fmt.Sprintf("%s%d", prefix, i))
		}()
	}
	close(gate)
	wg.Wait()
	return out
}

// procSampler reads /proc every 2 s for the process trees whose top matches:
// each one's resident memory, and all of theirs together.
type procSampler struct {
	match func(cmdline string) bool

	mu      sync.Mutex
	peak    map[int]int64
	PeakAll int64 `json:"peak_all_bytes"`
	Samples int   `json:"samples"`
}

func (p *procSampler) run(ctx context.Context) {
	if runtime.GOOS != "linux" {
		return
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		p.sample()
	}
}

func (p *procSampler) sample() {
	if runtime.GOOS != "linux" {
		return
	}
	procs := readProcs()
	children := map[int][]int{}
	for pid, pr := range procs {
		children[pr.ppid] = append(children[pr.ppid], pid)
	}
	var tree func(pid int) int64
	tree = func(pid int) int64 {
		sum := procs[pid].rss
		for _, c := range children[pid] {
			sum += tree(c)
		}
		return sum
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.peak == nil {
		p.peak = map[int]int64{}
	}
	p.Samples++
	var all int64
	for pid, pr := range procs {
		if p.match(pr.cmdline) && !p.match(procs[pr.ppid].cmdline) {
			rss := tree(pid)
			all += rss
			p.peak[pid] = max(p.peak[pid], rss)
		}
	}
	p.PeakAll = max(p.PeakAll, all)
}

// peaks is each tree's peak, largest first.
func (p *procSampler) peaks() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, 0, len(p.peak))
	for _, v := range p.peak {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

type codexReport struct {
	Harness       string            `json:"harness"`
	Version       string            `json:"version"`
	Mode          mode              `json:"mode"`
	Platform      string            `json:"platform"`
	Sessions      int               `json:"sessions"`
	Duration      string            `json:"duration"`
	Took          string            `json:"took"`
	Criteria      []criterion       `json:"criteria"`
	FreshFailures []string          `json:"fresh_home_open_failures,omitempty"`
	PerSession    []sessionReport   `json:"per_session"`
	Index         string            `json:"session_index"`
	Rollouts      map[string]string `json:"rollouts"`
	Databases     map[string]string `json:"databases"`
	PeakAllRSS    string            `json:"peak_all_rss"`
	PeakEachRSS   []string          `json:"peak_each_rss"`
	HostPeak      string            `json:"host_peak_rss"`
	ModelCalls    int               `json:"model_requests"`
}

func judgeCodex(t *testing.T, rep *codexReport, l contract.Layout, everyone []*session, procs *procSampler) {
	t.Helper()
	// Starts.
	var lockFail []string
	var slowest time.Duration
	for _, s := range everyone {
		if s.openErr != "" {
			lockFail = append(lockFail, s.name+": "+clip(s.openErr, 300))
		}
		slowest = max(slowest, s.openTook)
	}
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "every app-server starts behind the start lock, and the failure of #50290 shows without it",
		Pass: len(lockFail) == 0,
		Detail: fmt.Sprintf("behind the lock: %d of %d failed to start (the slowest open took %s); on a fresh home, all at once: %d of %d failed. %s",
			len(lockFail), len(everyone), slowest.Round(time.Millisecond), len(rep.FreshFailures), rep.Sessions, strings.Join(lockFail, "; ")),
	})

	// Turns, and what a busy database would fail.
	busy := 0
	for _, s := range everyone {
		for _, st := range s.turns {
			for _, f := range st.Failures {
				if strings.Contains(strings.ToLower(f), "database") || strings.Contains(f, "SQLITE") {
					busy++
				}
			}
		}
		rep.PerSession = append(rep.PerSession, s.report())
	}
	sent, failed, missed, waited := turnTotals(everyone)
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "no turn fails on a busy database", Pass: busy == 0 && failed == 0 && sent > 0,
		Detail: fmt.Sprintf("%d input turns, %d failed, %d of them on a database; after %d goals codex worked in turns of its own within the wait, but for %d", sent, failed, busy, waited, missed),
	})

	// session_index.jsonl.
	threads := map[string]string{}
	for _, s := range everyone {
		if s.id != "" {
			threads[s.id] = s.name
		}
	}
	idxProblems, idx := indexCheck(filepath.Join(l.Config, "session_index.jsonl"), threads)
	rep.Index = idx
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "session_index.jsonl, when codex writes it, parses with an entry per thread", Pass: len(idxProblems) == 0,
		Detail: idx + ". " + strings.Join(idxProblems, "; "),
	})

	// Rollouts.
	rep.Rollouts = map[string]string{}
	var rollProblems []string
	for id, name := range threads {
		path, err := tcodex.Rollout(l.Config, id)
		if err != nil {
			rollProblems = append(rollProblems, name+": "+err.Error())
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			rollProblems = append(rollProblems, name+": "+err.Error())
			continue
		}
		lines, bad, foreign := 0, 0, 0
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			lines++
			if !json.Valid([]byte(line)) {
				bad++
			}
			for other := range threads {
				if other != id && strings.Contains(line, other) {
					foreign++
				}
			}
		}
		rep.Rollouts[name] = fmt.Sprintf("%d lines, %d not JSON, %d naming another thread", lines, bad, foreign)
		if bad > 0 || foreign > 0 {
			rollProblems = append(rollProblems, name+": "+rep.Rollouts[name])
		}
	}
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "every rollout is whole", Pass: len(rollProblems) == 0,
		Detail: fmt.Sprintf("%d rollouts read. %s", len(threads), strings.Join(rollProblems, "; ")),
	})

	// The SQLite databases in CODEX_HOME, once every app-server stopped.
	rep.Databases = map[string]string{}
	dbs, _ := filepath.Glob(filepath.Join(l.Config, "*.sqlite"))
	var dbProblems []string
	for _, db := range dbs {
		out, err := exec.Command("python3", "-c", `import sqlite3,sys
c=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro",uri=True)
print(c.execute("pragma integrity_check").fetchone()[0])`, db).CombinedOutput()
		res := strings.TrimSpace(string(out))
		if err != nil {
			res = fmt.Sprintf("%v: %s", err, res)
		}
		size := func(p string) string {
			if st, err := os.Stat(p); err == nil {
				return fmt.Sprintf("%.1f MiB", float64(st.Size())/(1<<20))
			}
			return "none"
		}
		res = fmt.Sprintf("%s (%s, wal %s)", res, size(db), size(db+"-wal"))
		rep.Databases[filepath.Base(db)] = res
		if !strings.HasPrefix(res, "ok ") {
			dbProblems = append(dbProblems, filepath.Base(db)+": "+res)
		}
	}
	rep.Criteria = append(rep.Criteria, criterion{
		Name: "codex's databases pass SQLite's integrity check", Pass: len(dbs) > 0 && len(dbProblems) == 0,
		Detail: fmt.Sprintf("%d databases. %s", len(dbs), strings.Join(dbProblems, "; ")),
	})

	procs.mu.Lock()
	rep.PeakAllRSS = mib(procs.PeakAll)
	procs.mu.Unlock()
	for _, v := range procs.peaks() {
		rep.PeakEachRSS = append(rep.PeakEachRSS, mib(v))
	}
}

// indexCheck reads session_index.jsonl: every line JSON, every thread of the
// run in it. codex writes the file when a thread is named, and agentd names
// none: a run that leaves no file says so, and fails nothing.
func indexCheck(path string, threads map[string]string) ([]string, string) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "codex wrote none: it writes the file when a thread is named, and no Session's thread was"
	}
	if err != nil {
		return []string{err.Error()}, "unread"
	}
	per := map[string]int{}
	lines, bad := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		lines++
		if !json.Valid([]byte(line)) {
			bad++
			continue
		}
		for id := range threads {
			if strings.Contains(line, id) {
				per[id]++
			}
		}
	}
	var problems []string
	if bad > 0 {
		problems = append(problems, fmt.Sprintf("%d lines not JSON", bad))
	}
	most := 0
	for id, name := range threads {
		if per[id] == 0 {
			problems = append(problems, name+"'s thread has no entry")
		}
		most = max(most, per[id])
	}
	return problems, fmt.Sprintf("%d lines for %d threads, at most %d for one", lines, len(threads), most)
}

func (rep *codexReport) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s %s: %d Sessions side by side (%s)\n\n", rep.Harness, rep.Version, rep.Sessions, rep.Mode)
	fmt.Fprintf(&b, "%s, turns for %s; the run took %s. Peak memory of every app-server's tree together: %s; each at its peak: %s; of the probe's own process, which hosts every Session's adapter and the mock: %s.\n\n",
		rep.Platform, rep.Duration, rep.Took, rep.PeakAllRSS, strings.Join(rep.PeakEachRSS, ", "), rep.HostPeak)
	b.WriteString("| Criterion | Result | Detail |\n|---|---|---|\n")
	for _, c := range rep.Criteria {
		res := "pass"
		if !c.Pass {
			res = "**fail**"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.Name, res, strings.ReplaceAll(c.Detail, "|", "/"))
	}
	b.WriteString("\n| Session | Open | Turns completed / sent | Tool uses | Batches (peak a second) |\n|---|---|---|---|---|\n")
	for _, s := range rep.PerSession {
		var sent, done int
		for _, st := range s.Turns {
			sent += st.Sent
			done += st.Completed
		}
		fmt.Fprintf(&b, "| %s | %d ms | %d / %d | %d | %d (%d) |\n", s.Name, s.OpenMS, done, sent, s.ToolUses, s.Batches, s.PeakBatch)
	}
	if len(rep.FreshFailures) > 0 {
		b.WriteString("\nOn a fresh home, all at once:\n")
		for _, f := range rep.FreshFailures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	dbs := make([]string, 0, len(rep.Databases))
	for k, v := range rep.Databases {
		dbs = append(dbs, k+" "+v)
	}
	sort.Strings(dbs)
	fmt.Fprintf(&b, "\nsession_index.jsonl: %s. Databases: %s.\n", rep.Index, strings.Join(dbs, ", "))
	for _, s := range rep.PerSession {
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
