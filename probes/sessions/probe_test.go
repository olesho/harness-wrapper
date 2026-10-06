// Package sessions is Step 0 of agentd's plan "agentd: parallel Sessions in
// one agent" (https://coplan.olehluchkiv.com/d/agentd-parallel-sessions-in-one-agent):
// whether several Sessions of one harness run side by side in one agent's
// environment — one config dir, one workspace, one staged credential — with a
// Host each, as the plan proposes. It ships nothing; the probe and what it
// found (FINDINGS.md) are the whole of it.
//
// For Claude Code it provisions one environment the way agentd does, opens N
// Sessions in it at once, each with a hook spool of its own — the change the
// plan asks of the profile, made here by rewriting each Session's open
// configuration — and runs turns in all of them for a while: replies, tool
// calls, subagents, background commands and streamed text. It watches the
// files the Sessions share while they run and, at the end, checks:
//
//   - .claude.json parses after every write and keeps what was rendered;
//   - no Session's start hangs;
//   - every transcript parses and holds only its own Session;
//   - every hook event lands in its own Session's spool;
//   - a connector's headersHelper still runs, in a Session opened at the end.
//
// The tests skip without the binary they name:
//
//	HW_REAL_CLAUDE=… go test -count=1 -v ./probes/sessions/
//
// README.md has the rest.
package sessions

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// What the probe reads from its environment.
const (
	// envSessions is how many Sessions run at once; 4 when unset.
	envSessions = "HW_SESSIONS_N"
	// envDuration is how long they take turns, as a Go duration; 2m when
	// unset (the evidence runs take 30m).
	envDuration = "HW_SESSIONS_DURATION"
	// envEvidence names a directory the run's evidence is written to.
	envEvidence = "HW_SESSIONS_EVIDENCE"
	// envWork names a directory the run's environment is built in and left
	// in, for a look afterwards; a temporary one otherwise.
	envWork = "HW_SESSIONS_WORK"
)

type mode string

const (
	modeMock mode = "mock" // the real harness against the mock model API
	modeLive mode = "live" // the real harness against its real model API
)

func sessionCount(t *testing.T) int {
	t.Helper()
	v := os.Getenv(envSessions)
	if v == "" {
		return 4
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 2 {
		t.Fatalf("%s=%q: want a number of Sessions, 2 or more", envSessions, v)
	}
	return n
}

func runFor(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv(envDuration)
	if v == "" {
		return 2 * time.Minute
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q: want a Go duration", envDuration, v)
	}
	return d
}

// workDir is where the run's environment is built: under envWork when set,
// and left there, else a temporary directory. Symlinks are resolved: claude
// keys its projects by the working directory it resolves.
func workDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if base := os.Getenv(envWork); base != "" {
		dir = filepath.Join(base, time.Now().UTC().Format("20060102-150405"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// layoutUnder makes an agent's five roots under dir.
func layoutUnder(t *testing.T, dir string) contract.Layout {
	t.Helper()
	l := contract.Layout{
		Home:      filepath.Join(dir, "home"),
		Config:    filepath.Join(dir, "config"),
		Workspace: filepath.Join(dir, "workspace"),
		Secrets:   filepath.Join(dir, "secrets"),
		Scratch:   filepath.Join(dir, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// ---- one Session, as its Host sees it

// session is one Session of the probe's agent, driven by a Host of its own:
// its observations are committed and acknowledged as they arrive.
type session struct {
	name  string // s0, s1, …, final
	id    string // the id the Session was opened under
	spool string // its hook spool
	s     contract.Session

	mu       sync.Mutex
	changed  chan struct{} // closed and replaced on every batch
	ended    map[string]contract.TurnEndedData
	own      map[string]contract.TurnEndedData // turns the harness started itself, ended
	toolUse  map[string]string                 // tool use id → tool, from the record
	started  map[string]bool                   // tool_started, by tool use id
	finished map[string]bool                   // tool_finished, by tool use id
	subStart map[string]bool                   // subagent_started, by subagent id
	subStop  map[string]bool                   // subagent_stopped, by subagent id
	kinds    map[contract.Kind]int
	faults   []string
	notes    []string

	// batches is when each batch that carried items arrived, and items
	// how many it carried: the rate a Host asks its Supervisor to commit at.
	batches []time.Time
	items   int

	openTook time.Duration
	openErr  string
	turns    map[string]*turnStats // by scenario
	peakRSS  int64                 // bytes: the harness and every process beneath it
	closed   *contract.CloseResult
}

type turnStats struct {
	Sent      int      `json:"sent"`
	Completed int      `json:"completed"`
	Failed    int      `json:"failed"`
	Failures  []string `json:"failures,omitempty"`
}

func newSession(name, id, spool string) *session {
	return &session{
		name: name, id: id, spool: spool,
		changed: make(chan struct{}),
		ended:   map[string]contract.TurnEndedData{}, own: map[string]contract.TurnEndedData{},
		toolUse: map[string]string{}, started: map[string]bool{}, finished: map[string]bool{},
		subStart: map[string]bool{}, subStop: map[string]bool{},
		kinds: map[contract.Kind]int{}, turns: map[string]*turnStats{},
	}
}

func (s *session) note(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.notes) < 50 {
		s.notes = append(s.notes, fmt.Sprintf(format, args...))
	}
}

// observe is the Host's pump: every batch is recorded, as a Supervisor
// commits it, and then acknowledged. It runs until ctx ends.
func (s *session) observe(ctx context.Context) {
	for ctx.Err() == nil {
		b, err := s.s.Observe(ctx, time.Second, 4<<20)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if s.s.State().Phase == contract.PhaseExited {
				return
			}
			s.note("observe: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s.record(b)
		if b.BatchID != "" {
			if err := s.s.Ack(b.BatchID); err != nil {
				s.note("ack %s: %v", b.BatchID, err)
			}
		}
	}
}

func (s *session) record(b contract.Batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range b.Faults {
		s.faults = append(s.faults, f.Kind+": "+f.Detail)
	}
	if len(b.Items) > 0 {
		s.batches = append(s.batches, time.Now())
		s.items += len(b.Items)
	}
	for _, o := range b.Items {
		s.kinds[o.Kind]++
		switch o.Kind {
		case contract.KindTurnEnded:
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			if o.InputID != "" {
				s.ended[o.InputID] = d
			} else {
				s.own[o.TurnID] = d
			}
		case contract.KindToolUse:
			var d contract.ToolUseData
			_ = o.Decode(&d)
			s.toolUse[d.ToolUseID] = d.Name
		case contract.KindToolStarted:
			var d contract.ToolUseData
			_ = o.Decode(&d)
			s.started[d.ToolUseID] = true
		case contract.KindToolFinished:
			var d contract.ToolFinishedData
			_ = o.Decode(&d)
			s.finished[d.ToolUseID] = true
		case contract.KindSubagentStarted:
			var d contract.SubagentData
			_ = o.Decode(&d)
			s.subStart[d.SubagentID] = true
		case contract.KindSubagentStopped:
			var d contract.SubagentData
			_ = o.Decode(&d)
			s.subStop[d.SubagentID] = true
		}
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// ownLabel marks the count of the turns a harness takes itself after a
// step: they are reported apart from the inputs' turns.
const ownLabel = ", then a turn of the harness's own"

// turnTotals sums a Session's input turns, and apart from them the turns
// of its own the harness was waited for and did not take.
func turnTotals(sessions []*session) (sent, failed, ownMissed, ownWaited int) {
	for _, s := range sessions {
		s.mu.Lock()
		for sc, st := range s.turns {
			if strings.HasSuffix(sc, ownLabel) {
				ownWaited += st.Sent
				ownMissed += st.Failed
				continue
			}
			sent += st.Sent
			failed += st.Failed
		}
		s.mu.Unlock()
	}
	return sent, failed, ownMissed, ownWaited
}

// peakPerSecond is the most batches that arrived within any one second.
func peakPerSecond(times []time.Time) int {
	most, j := 0, 0
	for i := range times {
		for times[i].Sub(times[j]) >= time.Second {
			j++
		}
		most = max(most, i-j+1)
	}
	return most
}

var errTurnTimeout = errors.New("the turn did not end in time")

// step is one turn of a Session's script; %[1]s in a text is the turn's tag.
type step struct {
	scenario string
	text     string
	want     string
	own      bool // the harness takes a turn of its own after it
}

// takeTurns takes the script's turns in order, round after round, until
// deadline. After a step the harness follows with a turn of its own, that
// turn runs to its end before the next input, as agentd's node lets it.
func takeTurns(ctx context.Context, s *session, steps []step, deadline time.Time, turnIn time.Duration) {
	for k := 0; time.Now().Before(deadline); k++ {
		st := steps[k%len(steps)]
		tag := fmt.Sprintf("%s-%d", s.name, k)
		own := s.ownTurns()
		d, err := s.turn(ctx, fmt.Sprintf(st.text, tag), turnIn)
		want := st.want
		if strings.Contains(want, "%") {
			want = fmt.Sprintf(want, tag)
		}
		s.count(st.scenario, d, err, want)
		if err != nil {
			if s.s.State().Phase == contract.PhaseExited {
				s.note("the harness exited at turn %d: %v", k, err)
				return
			}
			time.Sleep(time.Second) // a failure never spins
			continue
		}
		if st.own {
			label := st.scenario + ownLabel
			if s.waitOwnTurn(ctx, own, turnIn/2) {
				s.count(label, contract.TurnEndedData{Outcome: contract.TurnCompleted}, nil, "")
			} else {
				s.count(label, contract.TurnEndedData{}, errTurnTimeout, "")
			}
		}
	}
}

// idle waits until the Session takes an input, as agentd's node does: a
// turn the harness started itself runs to its end, never stopped by the
// next input. A wait over a second is noted with what the Session was doing.
func (s *session) idle(ctx context.Context, wait time.Duration) error {
	t0 := time.Now()
	for {
		st := s.s.State()
		switch st.Phase {
		case contract.PhaseIdle:
			if took := time.Since(t0); took > time.Second {
				s.note("waited %s for idle", took.Round(10*time.Millisecond))
			}
			return nil
		case contract.PhaseExited:
			return errors.New("the harness exited")
		case contract.PhaseBlocked:
			return fmt.Errorf("blocked: %+v", st.Block)
		}
		if time.Since(t0) > wait {
			return fmt.Errorf("not idle after %s: phase %s, turn %q, input %q, %d background tasks", wait, st.Phase, st.TurnID, st.InputID, len(st.Background))
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// turn sends text once the Session takes an input, and waits for its turn
// to end.
func (s *session) turn(ctx context.Context, text string, wait time.Duration) (contract.TurnEndedData, error) {
	if err := s.idle(ctx, wait); err != nil {
		return contract.TurnEndedData{}, err
	}
	in := contract.Text(newID("in"), text)
	if _, err := s.s.Send(ctx, in); err != nil {
		return contract.TurnEndedData{}, fmt.Errorf("send: %w", err)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		d, ok := s.ended[in.InputID]
		ch := s.changed
		s.mu.Unlock()
		if ok {
			return d, nil
		}
		select {
		case <-ch:
		case <-deadline.C:
			return contract.TurnEndedData{}, errTurnTimeout
		case <-ctx.Done():
			return contract.TurnEndedData{}, ctx.Err()
		}
	}
}

// ownTurns is how many turns the harness started itself have ended.
func (s *session) ownTurns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.own)
}

// waitOwnTurn waits for a turn the harness starts itself — the one a
// background command's end brings — to end, beyond the first n.
func (s *session) waitOwnTurn(ctx context.Context, n int, wait time.Duration) bool {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		got := len(s.own) > n
		ch := s.changed
		s.mu.Unlock()
		if got {
			return true
		}
		select {
		case <-ch:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// count records a turn's outcome under its scenario.
func (s *session) count(scenario string, d contract.TurnEndedData, err error, want string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.turns[scenario]
	if st == nil {
		st = &turnStats{}
		s.turns[scenario] = st
	}
	st.Sent++
	fail := ""
	switch {
	case err != nil:
		fail = err.Error()
	case d.Outcome != contract.TurnCompleted:
		fail = "outcome " + string(d.Outcome)
		if d.Error != nil {
			fail += " (" + string(d.Error.Class) + ")"
		}
	case want != "" && !strings.Contains(d.Text, want):
		fail = fmt.Sprintf("reply %q, want %q", clip(d.Text, 80), want)
	}
	if fail == "" {
		st.Completed++
		return
	}
	st.Failed++
	if len(st.Failures) < 5 {
		st.Failures = append(st.Failures, fail)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- what the Sessions share: a JSON file every harness process rewrites

// fileWatch reads a file each time it changes, and checks that it parses and
// keeps the entries it was rendered with.
type fileWatch struct {
	path string
	keep []entry // what must stay as rendered

	mu sync.Mutex
	st fileStats
}

// fileStats is what a fileWatch saw.
type fileStats struct {
	Changes  int      `json:"changes"` // states read that differed from the last
	Torn     int      `json:"torn"`    // reads that did not parse, then did
	Corrupt  int      `json:"corrupt"` // states that still did not parse 300 ms later
	Lost     int      `json:"lost"`    // states missing a rendered entry
	Missing  int      `json:"missing"` // reads that found no file
	Examples []string `json:"examples,omitempty"`
}

// entry is a value at a path of a JSON document.
type entry struct {
	path []string
	want any
}

func (e entry) String() string { return strings.Join(e.path, " › ") }

// renderedEntries lists doc's leaves, but those named in skip, which the
// harness changes by design.
func renderedEntries(doc map[string]any, skip map[string]bool) []entry {
	var out []entry
	var walk func(prefix []string, v any)
	walk = func(prefix []string, v any) {
		if m, ok := v.(map[string]any); ok {
			for k, sub := range m {
				walk(append(append([]string(nil), prefix...), k), sub)
			}
			return
		}
		if !skip[strings.Join(prefix, ".")] {
			out = append(out, entry{path: prefix, want: v})
		}
	}
	walk(nil, doc)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func lookup(doc map[string]any, path []string) (any, bool) {
	var v any = doc
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

func (w *fileWatch) example(format string, args ...any) {
	if len(w.st.Examples) < 10 {
		w.st.Examples = append(w.st.Examples, time.Now().UTC().Format("15:04:05.000")+" "+fmt.Sprintf(format, args...))
	}
}

// run polls the file every 20 ms until ctx ends.
func (w *fileWatch) run(ctx context.Context) {
	var last []byte
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		b, err := os.ReadFile(w.path)
		if err != nil {
			w.mu.Lock()
			w.st.Missing++
			w.example("read: %v", err)
			w.mu.Unlock()
			continue
		}
		if string(b) == string(last) {
			continue
		}
		last = b
		w.check(b)
	}
}

// check judges one state of the file. One that does not parse is read again
// 300 ms later: a write caught midway is torn, a state left so is corrupt.
func (w *fileWatch) check(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.st.Changes++
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		w.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		again, rerr := os.ReadFile(w.path)
		w.mu.Lock()
		if rerr == nil && json.Unmarshal(again, &doc) == nil {
			w.st.Torn++
			w.example("torn read (%d bytes): %v", len(b), err)
		} else {
			w.st.Corrupt++
			w.example("corrupt (%d bytes): %v", len(b), err)
			return
		}
	}
	for _, e := range w.keep {
		got, ok := lookup(doc, e.path)
		if !ok || fmt.Sprint(got) != fmt.Sprint(e.want) {
			w.st.Lost++
			w.example("%s: %v (rendered %v)", e, got, e.want)
			return
		}
	}
}

func (w *fileWatch) snapshot() fileStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.st
	st.Examples = append([]string(nil), w.st.Examples...)
	return st
}

// ---- memory: each Session's harness and everything beneath it (Linux)

// rssSampler reads /proc every 2 s: for each Session, the resident memory
// of the harness process whose command line names the Session's id, and of
// every process beneath it — its MCP servers and tools.
type rssSampler struct {
	mu       sync.Mutex
	sessions []*session
	PeakAll  int64 `json:"peak_all_bytes"`
	Samples  int   `json:"samples"`
}

func (r *rssSampler) add(s *session) {
	r.mu.Lock()
	r.sessions = append(r.sessions, s)
	r.mu.Unlock()
}

func (r *rssSampler) run(ctx context.Context) {
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
		r.sample()
	}
}

func (r *rssSampler) sample() {
	procs := readProcs()
	children := map[int][]int{}
	for pid, p := range procs {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	var tree func(pid int) int64
	tree = func(pid int) int64 {
		sum := procs[pid].rss
		for _, c := range children[pid] {
			sum += tree(c)
		}
		return sum
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Samples++
	var all int64
	for _, s := range r.sessions {
		var rss int64
		for pid, p := range procs {
			if strings.Contains(p.cmdline, s.id) && procs[p.ppid].cmdline != "" && !strings.Contains(procs[p.ppid].cmdline, s.id) {
				rss += tree(pid) // the top of the Session's harness tree
			}
		}
		all += rss
		s.mu.Lock()
		s.peakRSS = max(s.peakRSS, rss)
		s.mu.Unlock()
	}
	r.PeakAll = max(r.PeakAll, all)
}

type proc struct {
	ppid    int
	rss     int64
	cmdline string
}

func readProcs() map[int]proc {
	out := map[int]proc{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		p := proc{cmdline: strings.ReplaceAll(string(cmd), "\x00", " ")}
		f, err := os.Open(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "PPid:"); ok {
				p.ppid, _ = strconv.Atoi(strings.TrimSpace(v))
			}
			if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
				p.rss = kb << 10
			}
		}
		_ = f.Close()
		out[pid] = p
	}
	return out
}

// ---- an MCP server that keeps the headers it was sent

// mcpServer is a minimal MCP server over streamable HTTP, as the conformance
// kit's: it answers initialize and tools/list with no tools, and keeps the
// headers of every request.
type mcpServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newMCPServer() *mcpServer {
	m := &mcpServer{}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *mcpServer) url() string { return m.srv.URL + "/mcp" }

func (m *mcpServer) close() { m.srv.Close() }

func (m *mcpServer) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.seen = append(m.seen, r.Header.Clone())
	m.mu.Unlock()
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "not a JSON-RPC message", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any = map[string]any{}
	switch req.Method {
	case "initialize":
		version := req.Params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "sessions-probe", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// heard counts the requests since the first n that carried header with
// value, and how many came in all.
func (m *mcpServer) heard(n int, header, value string) (with, all int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.seen[min(n, len(m.seen)):] {
		if h.Get(header) == value {
			with++
		}
	}
	return with, len(m.seen)
}

// ---- the report

// criterion is one of the plan's acceptance criteria, judged.
type criterion struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// writeEvidence writes the run's report as JSON and as Markdown, when
// envEvidence names a directory.
func writeEvidence(t *testing.T, name string, rep any, md string) {
	t.Helper()
	dir := os.Getenv(envEvidence)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("evidence: %v", err)
		return
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644); err != nil {
		t.Errorf("evidence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(md), 0o644); err != nil {
		t.Errorf("evidence: %v", err)
	}
}

func mib(b int64) string { return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20)) }

// selfPeak is this process's peak resident memory (Linux): it hosts every
// Session's adapter, as each Session's agentd-proxy would host one.
func selfPeak() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "unmeasured on " + runtime.GOOS
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return mib(kb << 10)
		}
	}
	return "unread"
}
