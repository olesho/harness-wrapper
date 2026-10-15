// Package workdir is Step 1.0 of agentd's plan "agentd: agents working
// together" (https://coplan.olehluchkiv.com/d/agentd-messaging-primitives-for-agents-p):
// whether hw can start a Session in a directory of its own beneath the
// agent's workspace — a git worktree of each repository, at
// <workspace>/wt/<name>/ — for the pinned Claude Code, Codex and Pi, and keep
// it there through concurrent edits, a reopen, a load into another
// environment, and a Session's end with its worktree removed. It also asks
// what a harness started there reads of the instructions file hw writes at
// the workspace's root. FINDINGS.md has the answers, evidence/ the runs.
//
// It drives each harness through the Harness Adapter Interface with
// OpenRequest.WorkDir (session_workdir, 1.9), against its real model API,
// with a real account: the tests skip without the binary and the credential
// they name (README.md).
package workdir

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// What the probe reads from its environment, beside each harness's binary
// and credential.
const (
	// envEvidence names a directory each run's report is written to, as
	// <harness>.json and <harness>.md.
	envEvidence = "HW_WORKDIR_EVIDENCE"
	// envWork names a directory the runs' environments are built in and
	// left in; a temporary one otherwise. Not for a shared machine: they
	// hold the credential.
	envWork = "HW_WORKDIR_WORK"
)

// fixture is one harness under the probe.
type fixture struct {
	name    string
	adapter contract.Adapter
	root    string // its distribution
	spec    contract.AgentSpec
	// credential stages the account's credential in a layout's secrets.
	credential func(t *testing.T, l contract.Layout) *contract.CredentialFile
}

// report is what one run found, step by step.
type report struct {
	Harness string `json:"harness"`
	Version string `json:"version"`
	Model   string `json:"model,omitempty"`
	Git     string `json:"git"`
	// SideBySide says the worktree Sessions were open at once.
	SideBySide bool   `json:"side_by_side"`
	Steps      []step `json:"steps"`
	Failed     int    `json:"failed"`
}

type step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Took   string `json:"took,omitempty"`
}

func (r *report) add(t *testing.T, name string, ok bool, took time.Duration, format string, args ...any) {
	t.Helper()
	s := step{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)}
	if took > 0 {
		s.Took = took.Round(100 * time.Millisecond).String()
	}
	r.Steps = append(r.Steps, s)
	if !ok {
		r.Failed++
		t.Errorf("%s: %s", name, s.Detail)
		return
	}
	t.Logf("%s: ok: %s", name, s.Detail)
}

func (r *report) write(t *testing.T) {
	t.Helper()
	dir := os.Getenv(envEvidence)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, r.Harness+".json"), append(b, '\n'), 0o644); err != nil { //nolint:gosec // evidence in a source tree
		t.Fatal(err)
	}
	model := r.Model
	if model == "" {
		model = "the harness's default"
	}
	var md bytes.Buffer
	fmt.Fprintf(&md, "# %s %s, model %s\n\n%s; worktree Sessions side by side: %v. %d of %d steps failed.\n\n| Step | OK | Took | Detail |\n|---|---|---|---|\n",
		r.Harness, r.Version, model, r.Git, r.SideBySide, r.Failed, len(r.Steps))
	for _, s := range r.Steps {
		ok := "yes"
		if !s.OK {
			ok = "**no**"
		}
		fmt.Fprintf(&md, "| %s | %s | %s | %s |\n", s.Name, ok, s.Took, strings.ReplaceAll(strings.ReplaceAll(s.Detail, "|", "\\|"), "\n", " "))
	}
	if err := os.WriteFile(filepath.Join(dir, r.Harness+".md"), md.Bytes(), 0o644); err != nil { //nolint:gosec // evidence in a source tree
		t.Fatal(err)
	}
}

// ---- the environment

// workDir is where a run's environments are built: under envWork when set,
// and left there, else a temporary directory, its symlinks resolved — a
// harness names what it keeps for the resolved path of its working
// directory.
func workDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if base := os.Getenv(envWork); base != "" {
		dir = filepath.Join(base, name+"-"+time.Now().UTC().Format("20060102-150405"))
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

func layoutUnder(t *testing.T, dir string) contract.Layout {
	t.Helper()
	l := contract.Layout{
		Home: filepath.Join(dir, "home"), Config: filepath.Join(dir, "config"), Workspace: filepath.Join(dir, "workspace"),
		Secrets: filepath.Join(dir, "secrets"), Scratch: filepath.Join(dir, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// git runs git in dir, with an identity of its own and none of the caller's
// GIT_* variables, and returns its output.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_AUTHOR_NAME=probe", "GIT_AUTHOR_EMAIL=probe@example.invalid",
		"GIT_COMMITTER_NAME=probe", "GIT_COMMITTER_EMAIL=probe@example.invalid", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return out
}

// repository makes the workspace's one repository, app, with a commit, and a
// worktree of it per Session name at wt/<name>/app on a branch of that name.
func repository(t *testing.T, ws string, names ...string) {
	t.Helper()
	app := filepath.Join(ws, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, app, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(app, "README.md"), []byte("# app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, app, "add", "README.md")
	mustGit(t, app, "commit", "-q", "-m", "init")
	for _, n := range names {
		mustGit(t, app, "worktree", "add", "-q", "-b", n, filepath.Join(ws, "wt", n, "app"))
	}
}

// ---- one Session, as its Host drives it

// session is one Session of the probe's agent: a Host that commits and
// acknowledges every batch at once, keeping each turn's end.
type session struct {
	name string // its working directory's name, or "root"
	dir  string // its OpenRequest.WorkDir
	s    contract.Session
	id   string

	mu     sync.Mutex
	ended  map[string]contract.TurnEndedData // by input id
	wake   chan struct{}
	cp     *contract.Checkpoint
	faults []string
	stop   chan struct{}
	done   chan struct{}
}

// open opens the Session req names, and observes it until closed.
func open(ctx context.Context, f fixture, req contract.OpenRequest, name string) (*session, error) {
	s, err := f.adapter.NewSession(req)
	if err != nil {
		return nil, err
	}
	res, err := s.Open(ctx)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = s.Close(cctx, contract.ClosePark, 0)
		cancel()
		return nil, err
	}
	x := &session{
		name: name, dir: req.WorkDir, s: s, id: res.SessionID, cp: req.Checkpoint,
		ended: map[string]contract.TurnEndedData{}, wake: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go x.observe()
	return x, nil
}

func (x *session) observe() {
	defer close(x.done)
	for {
		select {
		case <-x.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		b, err := x.s.Observe(ctx, 500*time.Millisecond, 4<<20)
		cancel()
		if err != nil {
			if x.s.State().Phase == contract.PhaseExited {
				<-x.stop
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		x.mu.Lock()
		for _, f := range b.Faults {
			x.faults = append(x.faults, f.Kind+": "+f.Detail)
		}
		for _, o := range b.Items {
			if o.Kind != contract.KindTurnEnded || o.InputID == "" {
				continue
			}
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			if prev, ok := x.ended[o.InputID]; ok && d.Text == "" {
				d.Text = prev.Text // claude's record repeats the end without its text
			}
			x.ended[o.InputID] = d
		}
		if b.Checkpoint != nil {
			cp := *b.Checkpoint
			x.cp = &cp
		}
		close(x.wake)
		x.wake = make(chan struct{})
		x.mu.Unlock()
		if b.NeedsAck() {
			_ = x.s.Ack(b.BatchID)
		}
	}
}

// turn sends text and waits for its turn's end, at most within.
func (x *session) turn(text string, within time.Duration) (contract.TurnEndedData, error) {
	in := "in_" + randomHex(8)
	ctx, cancel := context.WithTimeout(context.Background(), contract.SendDeadline)
	_, err := x.s.Send(ctx, contract.Text(in, text))
	cancel()
	if err != nil {
		return contract.TurnEndedData{}, fmt.Errorf("send: %w", err)
	}
	deadline := time.After(within)
	for {
		x.mu.Lock()
		d, ok := x.ended[in]
		wake := x.wake
		x.mu.Unlock()
		if ok {
			// Its record's batch, and the idle that follows, settle.
			for i := 0; i < 100 && x.s.State().Phase == contract.PhaseBusy; i++ {
				time.Sleep(100 * time.Millisecond)
			}
			return d, nil
		}
		select {
		case <-wake:
		case <-deadline:
			return contract.TurnEndedData{}, fmt.Errorf("no turn_ended within %s", within)
		}
	}
}

// close closes the Session for reason, and stops observing it.
func (x *session) close(reason contract.CloseReason) (contract.CloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), contract.DefaultDrain+contract.CloseSlack)
	defer cancel()
	res, err := x.s.Close(ctx, reason, contract.DefaultDrain)
	close(x.stop)
	<-x.done
	return res, err
}

func (x *session) checkpoint() *contract.Checkpoint {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.cp
}

// ---- the run

// turnWait bounds a turn against a real model.
const turnWait = 5 * time.Minute

// run asks the probe's questions of one harness.
func run(t *testing.T, f fixture, rep *report) {
	desc := f.adapter.Describe()
	rep.Harness, rep.Version, rep.Model = f.name, desc.Harness.Version, f.spec.Model
	rep.Git, _ = git(".", "--version")
	defer rep.write(t)
	if !desc.Has(contract.CapSessionWorkDir) {
		t.Fatalf("%s declares no %s", f.name, contract.CapSessionWorkDir)
	}
	base := workDir(t, f.name)
	l := layoutUnder(t, filepath.Join(base, "env1"))
	names := []string{"a", "b"}
	repository(t, l.Workspace, names...)
	secret := "MANGO-" + strings.ToUpper(randomHex(3))
	spec := f.spec
	spec.Instructions = contract.Instructions{
		Persona:   "You are a careful assistant. Do exactly what you are asked, and nothing else.",
		Workspace: "# Workspace\n\nThis workspace holds one repository, `app/`. The workspace's secret word is " + secret + ".\n",
	}
	spec.PermissionPosture = contract.PostureBypass
	cred := f.credential(t, l)
	if cred != nil {
		spec.Credential = &contract.CredentialRef{Kind: cred.Kind}
	}
	res, err := f.adapter.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: f.root, Layout: l, Spec: spec})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := conformance.Apply(l, res); err != nil {
		t.Fatalf("applying the result: %v", err)
	}
	openIn := func(lay contract.Layout, r contract.ProvisionResult, c *contract.CredentialFile, dir, name string, mode contract.OpenMode, id string, cp *contract.Checkpoint, loaded bool) (*session, error) {
		ctx, cancel := context.WithTimeout(context.Background(), contract.OpenDeadline)
		defer cancel()
		return open(ctx, f, contract.OpenRequest{
			Mode: mode, SessionID: id, OpenConfig: r.OpenConfig, Layout: lay, Credential: c, Checkpoint: cp, Loaded: loaded, WorkDir: dir,
		}, name)
	}

	// 1. The root: a Session in the workspace itself, whose instructions
	// file is in its working directory.
	root, err := openIn(l, res, cred, "", "root", contract.OpenFresh, "", nil, false)
	if err != nil {
		t.Fatalf("opening a Session in the workspace: %v", err)
	}
	ask := "Without running any command: what is this workspace's secret word, as your instructions or context files state it? Reply with just the word, or UNKNOWN if you were told none."
	start := time.Now()
	end, err := root.turn(ask, turnWait)
	rep.add(t, "root: reads the workspace's instructions", err == nil && strings.Contains(end.Text, secret), time.Since(start), "%v %q", err, cut(end.Text))
	if _, err := root.close(contract.ClosePark); err != nil {
		t.Logf("closing the root Session: %v", err)
	}

	// 2. Sessions in wt/a and wt/b: side by side when the harness runs
	// several, each told the same.
	side := desc.Sessions() > 1
	rep.SideBySide = side
	type parked struct {
		id string
		cp *contract.Checkpoint
	}
	opened, rested := map[string]*session{}, map[string]parked{}
	openOne := func(n string) {
		start := time.Now()
		x, err := openIn(l, res, cred, "wt/"+n, n, contract.OpenFresh, "", nil, false)
		rep.add(t, "wt/"+n+": opens", err == nil, time.Since(start), "%v", err)
		if err == nil {
			opened[n] = x
		}
	}
	perSession := func(n string, x *session, r *report) {
		start := time.Now()
		end, err := x.turn(ask, turnWait)
		r.add(t, "wt/"+n+": reads the workspace's instructions", err == nil && strings.Contains(end.Text, secret), time.Since(start),
			"%v %q (the instructions file is at the workspace's root, two directories above)", err, cut(end.Text))
		edit := fmt.Sprintf("Run exactly these commands, in the directory you start in, and nothing else:\n"+
			"pwd\nprintf '%%s\\n' '%s was here' > app/%s.txt\ngit -C app add %s.txt\ngit -C app commit -q -m '%s: add file'\ngit -C app log --oneline -1\n"+
			"Then reply with the output of pwd, on a line of its own, and nothing else.", n, n, n, n)
		start = time.Now()
		end, err = x.turn(edit, turnWait)
		want := filepath.Join(l.Workspace, "wt", n)
		r.add(t, "wt/"+n+": works there", err == nil && strings.Contains(end.Text, want), time.Since(start), "%v pwd %q, want %s", err, cut(end.Text), want)
	}
	if side {
		for _, n := range names {
			openOne(n)
		}
		var wg sync.WaitGroup
		subs := make([]*report, len(names))
		for i, n := range names {
			subs[i] = &report{}
			if x := opened[n]; x != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					perSession(n, x, subs[i])
				}()
			}
		}
		wg.Wait()
		for _, sub := range subs {
			rep.Steps, rep.Failed = append(rep.Steps, sub.Steps...), rep.Failed+sub.Failed
		}
	} else {
		for _, n := range names {
			openOne(n)
			if x := opened[n]; x != nil {
				perSession(n, x, rep)
				if _, err := x.close(contract.ClosePark); err != nil {
					t.Logf("parking %s: %v", n, err)
				}
				rested[n] = parked{x.id, x.checkpoint()}
				delete(opened, n)
			}
		}
	}
	// What each worktree holds: its own file, committed on its own branch;
	// the repository's own checkout neither.
	app := filepath.Join(l.Workspace, "app")
	for _, n := range names {
		wt := filepath.Join(l.Workspace, "wt", n, "app")
		log, _ := git(wt, "log", "--oneline", "-1", n)
		files, _ := git(wt, "ls-files")
		other := map[string]string{"a": "b", "b": "a"}[n]
		ok := strings.Contains(log, n+": add file") && strings.Contains(files, n+".txt") && !strings.Contains(files, other+".txt")
		rep.add(t, "wt/"+n+": its commit on its branch, alone", ok, 0, "log %q, files %q", log, strings.ReplaceAll(files, "\n", " "))
	}
	mainFiles, _ := git(app, "ls-files")
	rep.add(t, "app: the repository's own checkout untouched", !strings.Contains(mainFiles, "a.txt") && !strings.Contains(mainFiles, "b.txt"), 0, "files %q", strings.ReplaceAll(mainFiles, "\n", " "))

	// 3. wt/b's Session ends, and its worktree goes; wt/a's, open beside it
	// where the harness runs several, goes on.
	if b := opened["b"]; b != nil {
		if r, err := b.close(contract.CloseDelete); err != nil {
			rep.add(t, "wt/b: ends", false, 0, "%+v %v", r, err)
		}
		delete(opened, "b")
	}
	out, err := git(app, "worktree", "remove", filepath.Join(l.Workspace, "wt", "b", "app"))
	branch, _ := git(app, "branch", "--list", "b")
	_, gone := os.Stat(filepath.Join(l.Workspace, "wt", "b", "app"))
	rep.add(t, "wt/b: its worktree removed once it ended, its branch kept", err == nil && os.IsNotExist(gone) && strings.Contains(branch, "b"), 0, "%v %q, branch %q", err, out, branch)
	status := "Run `git -C app status --short --branch` where you are, and reply with its output only."
	if a := opened["a"]; a != nil {
		start := time.Now()
		end, err := a.turn(status, turnWait)
		rep.add(t, "wt/a: goes on beside the removal", err == nil && strings.Contains(end.Text, "## a"), time.Since(start), "%v %q", err, cut(end.Text))
		if _, err := a.close(contract.ClosePark); err != nil {
			t.Logf("parking a: %v", err)
		}
		rested["a"] = parked{a.id, a.checkpoint()}
	}

	// 4. A restart: wt/a's Session, parked, reopened where it works.
	pa := rested["a"]
	if pa.id == "" {
		t.Fatal("no Session of wt/a to reopen")
	}
	start = time.Now()
	a, err := openIn(l, res, cred, "wt/a", "a", contract.OpenReopen, pa.id, pa.cp, false)
	rep.add(t, "wt/a: reopens", err == nil, time.Since(start), "%v", err)
	if err != nil {
		t.FailNow()
	}
	aID := a.id
	recall := "What file did you create in this conversation? Reply with just its file name."
	start = time.Now()
	end, err = a.turn(recall, turnWait)
	rep.add(t, "wt/a: remembers after the reopen", err == nil && strings.Contains(end.Text, "a.txt"), time.Since(start), "%v %q", err, cut(end.Text))
	if !side {
		start = time.Now()
		end, err = a.turn(status, turnWait)
		rep.add(t, "wt/a: goes on after wt/b's removal", err == nil && strings.Contains(end.Text, "## a"), time.Since(start), "%v %q", err, cut(end.Text))
	}
	if _, err := a.close(contract.ClosePark); err != nil {
		t.Logf("parking a: %v", err)
	}

	// 5. A load into another environment, at another path: the workspace
	// copied with its worktrees and their links repaired, as an export and a
	// load would carry them; the history saved and restored where the load's
	// relocations send it, naming both working directories.
	saved, err := conformance.Save(desc, adapter.ArchiveFormat, l, res)
	if err != nil {
		t.Fatalf("saving: %v", err)
	}
	saved.Source.WorkDirs = []string{"wt/a", "wt/b"}
	l2 := layoutUnder(t, filepath.Join(base, "env2-elsewhere"))
	if err := os.Remove(l2.Workspace); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-Rp", l.Workspace, l2.Workspace).CombinedOutput(); err != nil {
		t.Fatalf("copying the workspace: %v: %s", err, out)
	}
	// The source is gone, as a load's is: nothing of it can be reached.
	if err := os.RemoveAll(filepath.Join(base, "env1")); err != nil {
		t.Fatal(err)
	}
	app2 := filepath.Join(l2.Workspace, "app")
	broken, berr := git(filepath.Join(l2.Workspace, "wt", "a", "app"), "status", "--short")
	rep.add(t, "load: a copied worktree's links name the source", berr != nil, 0, "git status before a repair: %v %q", berr, cut(broken))
	repair, err := git(app2, "worktree", "repair", filepath.Join(l2.Workspace, "wt", "a", "app"))
	after, aerr := git(filepath.Join(l2.Workspace, "wt", "a", "app"), "log", "--oneline", "-1")
	rep.add(t, "load: git worktree repair links it again", err == nil && aerr == nil && strings.Contains(after, "a: add file"), 0, "repair: %v %q; log %v %q", err, repair, aerr, after)
	cred2 := f.credential(t, l2)
	src := saved.Source
	res2, err := f.adapter.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: f.root, Layout: l2, Spec: spec, Load: &src})
	if err != nil {
		t.Fatalf("Provision of the load: %v", err)
	}
	if err := conformance.Restore(l2, res2, saved); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if err := conformance.Apply(l2, res2); err != nil {
		t.Fatalf("applying the load: %v", err)
	}
	cp, n, err := seed(f, l2, res2, aID, "wt/a")
	rep.add(t, "load: wt/a's record read where it was moved", err == nil && n > 0 && cp != nil, 0, "%d items, %v", n, err)
	start = time.Now()
	a2, err := openIn(l2, res2, cred2, "wt/a", "a", contract.OpenReopen, aID, cp, true)
	rep.add(t, "load: wt/a's Session opens loaded, in the new wt/a", err == nil, time.Since(start), "%v", err)
	if err != nil {
		return
	}
	start = time.Now()
	end, err = a2.turn("What file did you create in this conversation? Then run `pwd` and `git -C app log --oneline -1` where you are. Reply with the file name, then pwd's output, then the log line.", turnWait)
	ok := err == nil && strings.Contains(end.Text, "a.txt") && strings.Contains(end.Text, filepath.Join(l2.Workspace, "wt", "a")) && strings.Contains(end.Text, "a: add file")
	rep.add(t, "load: remembers, works in the new wt/a, and git works there", ok, time.Since(start), "%v %q", err, cut(end.Text))
	if _, err := a2.close(contract.ClosePark); err != nil {
		t.Logf("parking the loaded a: %v", err)
	}
}

// seed reads Session id's record, working in dir, to its end with no
// harness, as a Supervisor does before a loaded Session opens: the
// checkpoint after its last item, and how many items it held.
func seed(f fixture, l contract.Layout, res contract.ProvisionResult, id, dir string) (*contract.Checkpoint, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := f.adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: id, OpenConfig: res.OpenConfig, Layout: l, WorkDir: dir})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = r.Close() }()
	var cp *contract.Checkpoint
	n := 0
	for {
		b, err := r.Observe(ctx, 200*time.Millisecond, contract.MaxObserveBytes)
		if err != nil {
			return cp, n, err
		}
		n += len(b.Items)
		if b.Checkpoint != nil {
			cp = b.Checkpoint
		}
		if b.NeedsAck() {
			if err := r.Ack(b.BatchID); err != nil {
				return cp, n, err
			}
		}
		if b.EndOfRecord {
			return cp, n, nil
		}
	}
}

// cut is s on one line, at most 300 bytes.
func cut(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// A worktree made with relative paths (git 2.48 and later) travels with its
// repository: copied elsewhere beside it, it needs no repair. Without them its
// links name where it was made.
func TestRelativeWorktreesTravel(t *testing.T) {
	out, err := git(".", "worktree", "add", "-h")
	if !strings.Contains(out, "relative-paths") {
		t.Skipf("this git has no worktree add --relative-paths (%v)", err)
	}
	base := workDir(t, "relative")
	ws := filepath.Join(base, "ws")
	app := filepath.Join(ws, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, app, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(app, "README.md"), []byte("# app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, app, "add", "README.md")
	mustGit(t, app, "commit", "-q", "-m", "init")
	mustGit(t, app, "worktree", "add", "-q", "--relative-paths", "-b", "rel", filepath.Join(ws, "wt", "rel", "app"))
	mustGit(t, app, "worktree", "add", "-q", "-b", "abs", filepath.Join(ws, "wt", "abs", "app"))
	moved := filepath.Join(base, "moved")
	if out, err := exec.Command("cp", "-Rp", ws, moved).CombinedOutput(); err != nil {
		t.Fatalf("copying: %v: %s", err, out)
	}
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	if out, err := git(filepath.Join(moved, "wt", "rel", "app"), "status", "--short", "--branch"); err != nil || !strings.Contains(out, "## rel") {
		t.Errorf("a relative worktree, moved: %v %q", err, out)
	}
	if out, err := git(filepath.Join(moved, "wt", "abs", "app"), "status", "--short", "--branch"); err == nil {
		t.Errorf("an absolute worktree, moved, still works: %q", out)
	}
	if out, err := git(filepath.Join(moved, "app"), "worktree", "list"); err != nil || !strings.Contains(out, filepath.Join(moved, "wt", "rel", "app")) {
		t.Errorf("the moved repository's worktrees: %v %q", err, out)
	}
}

// pinned is the version hw pins harness to.
func pinned(harness string) string {
	v, _ := versions.Pinned(harness)
	return v
}
