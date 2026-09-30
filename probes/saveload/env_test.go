package saveload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// dirs names an environment's directories beneath its base.
type dirs struct{ home, config, workspace, secrets, scratch, harness string }

// sourceDirs are the names agentd gives an environment's roots: its config
// root is profile, its scratch root spool. restoredDirs share none of them, so
// a restored environment differs from its source in every root's path, not
// only in the base they sit under.
var (
	sourceDirs   = dirs{"home", "profile", "workspace", "secrets", "spool", "harness"}
	restoredDirs = dirs{"h", "cfg", "ws", "sec", "scr", "dist"}
)

// environment is one agent's environment: its five roots, a harness
// distribution of its own, and what was provisioned into it. The probe is its
// Supervisor and its Host.
type environment struct {
	p      *probe
	label  string // its name in the evidence: source, restored/<variant>, negative
	base   string // everything of it lives beneath
	root   string // the harness distribution: harness_root
	layout contract.Layout
	result contract.ProvisionResult
	cred   *contract.CredentialFile
	// credNote says how the environment got its credential when it is not a
	// credential file.
	credNote string
	inputs   int
}

// newEnvironment makes an empty environment under base: the roots, 0700, and
// the harness distribution.
func (p *probe) newEnvironment(label, base string, d dirs) *environment {
	p.t.Helper()
	e := &environment{p: p, label: label, base: base, root: filepath.Join(base, d.harness)}
	e.layout = contract.Layout{
		Home: filepath.Join(base, d.home), Config: filepath.Join(base, d.config),
		Workspace: filepath.Join(base, d.workspace), Secrets: filepath.Join(base, d.secrets),
		Scratch: filepath.Join(base, d.scratch),
	}
	for _, dir := range []string{e.layout.Home, e.layout.Config, e.layout.Workspace, e.layout.Secrets, e.layout.Scratch, filepath.Join(e.root, "bin")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			p.t.Fatal(err)
		}
	}
	p.h.Distribution(p, e.root)
	// What the harness leaves outside the environment goes when the run ends.
	p.t.Cleanup(func() {
		for _, dir := range p.h.Outside(e) {
			_ = os.RemoveAll(dir)
		}
	})
	return e
}

// provision renders spec for the environment and applies it, as a Supervisor
// does, with a credential staged for it. The harness adjusts the rendered
// result first: in mock mode, to reach the mock model API.
func (e *environment) provision(spec contract.AgentSpec) {
	t := e.p.t
	t.Helper()
	e.cred = e.p.h.Credential(e.p, e.layout, e.label)
	if e.cred != nil {
		spec.Credential = &contract.CredentialRef{Kind: e.cred.Kind}
	}
	res, err := e.p.adapter.Provision(contract.ProvisionRequest{
		Contract: contract.Version, HarnessRoot: e.root, Layout: e.layout, Spec: spec,
	})
	if err != nil {
		t.Fatalf("%s: Provision: %v", e.label, err)
	}
	if e.credNote, err = e.p.h.Adjust(e.p, &res); err != nil {
		t.Fatalf("%s: adjusting the provisioned result: %v", e.label, err)
	}
	if err := conformance.Apply(e.layout, res); err != nil {
		t.Fatalf("%s: applying the provisioned files: %v", e.label, err)
	}
	e.result = res
}

// canonical is path with its symlinks resolved: how a harness that resolves
// its working directory sees it.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// processes are the command lines of the running processes that name marker.
// A ps that fails is reported as a line of its own: a list that could not be
// read is never an empty one.
func processes(marker string) []string {
	out, err := exec.Command("ps", "axww", "-o", "pid=,command=").Output()
	if err != nil {
		return []string{"ps: " + err.Error()}
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" && strings.Contains(l, marker) {
			lines = append(lines, l)
		}
	}
	return lines
}

// quiet waits until no running process names the environment's base — by the
// path it was given or its resolved one — and returns the ones still running
// when the wait ends.
func (e *environment) quiet(wait time.Duration) []string {
	deadline := time.Now().Add(wait)
	for {
		left := processes(e.base)
		if c := canonical(e.base); c != e.base {
			left = append(left, processes(c)...)
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- a Session, observed

// session is one open Session and everything it delivered: the probe observes
// it and acknowledges every batch, as a Host whose Supervisor commits at once.
type session struct {
	e  *environment
	s  contract.Session
	id string // the harness's id for the Session, from Open

	mu     sync.Mutex
	seen   []contract.Observation // every delivery, in order
	cp     *contract.Checkpoint   // of the last acknowledged batch that had one
	resets []contract.Reset
	faults []contract.Fault
	errs   []error
	stop   chan struct{}
	done   chan struct{}
}

// open opens a Session in the environment and starts observing it.
func (e *environment) open(mode contract.OpenMode, sessionID string, cp *contract.Checkpoint) (*session, error) {
	s, err := e.p.adapter.NewSession(contract.OpenRequest{
		Mode: mode, SessionID: sessionID, OpenConfig: e.result.OpenConfig,
		Layout: e.layout, Credential: e.cred, Checkpoint: cp,
	})
	if err != nil {
		return nil, fmt.Errorf("NewSession: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), contract.OpenDeadline)
	defer cancel()
	res, err := s.Open(ctx)
	if err != nil {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, _ = s.Close(cctx, contract.ClosePark, 0)
		ccancel()
		return nil, fmt.Errorf("Open: %w", err)
	}
	ss := &session{e: e, s: s, id: res.SessionID, stop: make(chan struct{}), done: make(chan struct{})}
	go ss.pump()
	return ss, nil
}

func (s *session) pump() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		b, err := s.s.Observe(context.Background(), 200*time.Millisecond, contract.MaxObserveBytes)
		if err != nil {
			s.mu.Lock()
			s.errs = append(s.errs, fmt.Errorf("Observe: %w", err))
			s.mu.Unlock()
			return
		}
		if !b.NeedsAck() {
			continue
		}
		// Commit, then acknowledge: the order a Supervisor keeps.
		s.mu.Lock()
		s.seen = append(s.seen, b.Items...)
		if b.Checkpoint != nil {
			s.cp = b.Checkpoint
		}
		if b.Reset != nil {
			s.resets = append(s.resets, *b.Reset)
		}
		s.faults = append(s.faults, b.Faults...)
		s.mu.Unlock()
		if err := s.s.Ack(b.BatchID); err != nil {
			s.mu.Lock()
			s.errs = append(s.errs, fmt.Errorf("Ack %s: %w", b.BatchID, err))
			s.mu.Unlock()
			return
		}
	}
}

// disturbed counts the resets and faults the Session's batches carried: a
// record that stopped continuing its checkpoint, or lost something.
func (s *session) disturbed() (resets, faults int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.resets), len(s.faults)
}

// snapshot is everything delivered so far.
func (s *session) snapshot() []contract.Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.Observation(nil), s.seen...)
}

// await waits for a delivered observation pred holds of.
func (s *session) await(what string, pred func(contract.Observation) bool) (contract.Observation, error) {
	deadline := time.Now().Add(s.e.p.wait)
	for {
		for _, o := range s.snapshot() {
			if pred(o) {
				return o, nil
			}
		}
		s.mu.Lock()
		errs := errors.Join(s.errs...)
		s.mu.Unlock()
		if errs != nil {
			return contract.Observation{}, fmt.Errorf("no %s: %w", what, errs)
		}
		if time.Now().After(deadline) {
			return contract.Observation{}, fmt.Errorf("no %s within %s", what, s.e.p.wait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// turn is one input's turn, as the Session reported it.
type turn struct {
	Input   string
	Prompt  string
	Outcome contract.TurnOutcome
	// Reply is the turn's final text.
	Reply string
	// Seen is every observation delivered from the send until the turn's
	// record settled: the new input's, and anything else the record gave.
	Seen []contract.Observation
}

// record is the turn's record-origin observations.
func (tn turn) record() []contract.Observation {
	var out []contract.Observation
	for _, o := range tn.Seen {
		if o.Origin == contract.OriginRecord {
			out = append(out, o)
		}
	}
	return out
}

// tools is what the turn's tools were given and what they printed, from the
// record.
func (tn turn) tools() (uses int, output string) {
	for _, o := range tn.record() {
		switch o.Kind {
		case contract.KindToolUse:
			uses++
		case contract.KindToolResult:
			var d contract.ToolResultData
			if o.Decode(&d) == nil {
				output += d.Output + "\n"
			}
		}
	}
	return uses, output
}

// turn sends prompt as the environment's next input and waits for its turn to
// end and its record to settle: the live end, the end the record proves, the
// Session idle again, and then nothing new for a moment.
func (s *session) turn(prompt string) (turn, error) {
	p := s.e.p
	deadline := time.Now().Add(p.wait)
	for s.s.State().Phase != contract.PhaseIdle {
		if time.Now().After(deadline) {
			return turn{}, fmt.Errorf("the Session is %s, not idle", s.s.State().Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.e.inputs++
	tn := turn{Input: strings.NewReplacer("/", "-", "+", "-").Replace(s.e.label) + "-" + strconv.Itoa(s.e.inputs), Prompt: prompt}
	from := len(s.snapshot())
	ctx, cancel := context.WithTimeout(context.Background(), contract.SendDeadline)
	_, err := s.s.Send(ctx, contract.Text(tn.Input, prompt))
	cancel()
	if err != nil {
		return tn, fmt.Errorf("Send %q: %w", prompt, err)
	}
	ended := func(origin contract.Origin) func(contract.Observation) bool {
		return func(o contract.Observation) bool {
			return o.Kind == contract.KindTurnEnded && o.Origin == origin && o.InputID == tn.Input
		}
	}
	end, err := s.await("turn_ended of "+tn.Input, ended(contract.OriginLive))
	if err != nil {
		return tn, err
	}
	var d contract.TurnEndedData
	if err := end.Decode(&d); err != nil {
		return tn, err
	}
	tn.Outcome, tn.Reply = d.Outcome, d.Text
	if _, err := s.await("record turn_ended of "+tn.Input, ended(contract.OriginRecord)); err != nil {
		return tn, err
	}
	for s.s.State().Phase != contract.PhaseIdle && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// Settled: nothing new for half a second.
	for n, since := len(s.snapshot()), time.Now(); time.Since(since) < 500*time.Millisecond; {
		time.Sleep(50 * time.Millisecond)
		if m := len(s.snapshot()); m != n {
			n, since = m, time.Now()
		}
	}
	tn.Seen = s.snapshot()[from:]
	return tn, nil
}

// close parks the Session, as agentd does an idle agent's, and returns what
// Close established and the last committed checkpoint.
func (s *session) close() (contract.CloseResult, *contract.Checkpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), contract.DefaultDrain+contract.CloseSlack+30*time.Second)
	res, err := s.s.Close(ctx, contract.ClosePark, contract.DefaultDrain)
	cancel()
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return res, s.cp, errors.Join(append([]error{err}, s.errs...)...)
}

// ---- the record, read with no harness running

// recordRead is one reading of a Session's record through a record handle.
type recordRead struct {
	Items   []contract.Observation
	CP      *contract.Checkpoint // of the last batch that had one
	Batches int
	Resets  []contract.Reset
	Rescans []contract.Rescan
	Faults  []contract.Fault
	// Ended: the handle reported the record's end.
	Ended bool
}

// ids is the set of the ids read.
func (r recordRead) ids() map[string]bool {
	out := make(map[string]bool, len(r.Items))
	for _, o := range r.Items {
		out[o.ID] = true
	}
	return out
}

// readRecord reads the Session's record from a checkpoint to its end with the
// adapter's record handle: no harness process, no credential, nothing sent. It
// acknowledges every batch, as a Supervisor that committed it would — what it
// does with the items is its caller's business.
func (e *environment) readRecord(sessionID string, from *contract.Checkpoint) (recordRead, error) {
	var out recordRead
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := e.p.adapter.OpenRecord(ctx, contract.RecordRequest{
		SessionID: sessionID, OpenConfig: e.result.OpenConfig, Layout: e.layout, Checkpoint: from,
	})
	if err != nil {
		return out, fmt.Errorf("OpenRecord: %w", err)
	}
	defer func() { _ = r.Close() }()
	for ctx.Err() == nil {
		b, err := r.Observe(ctx, time.Second, contract.MaxObserveBytes)
		if err != nil {
			return out, fmt.Errorf("Observe: %w", err)
		}
		if b.EndOfRecord {
			out.Ended = true
			return out, nil
		}
		if !b.NeedsAck() {
			continue
		}
		out.Batches++
		out.Items = append(out.Items, b.Items...)
		if b.Checkpoint != nil {
			out.CP = b.Checkpoint
		}
		if b.Reset != nil {
			out.Resets = append(out.Resets, *b.Reset)
		}
		if b.Rescan != nil {
			out.Rescans = append(out.Rescans, *b.Rescan)
		}
		out.Faults = append(out.Faults, b.Faults...)
		if err := r.Ack(b.BatchID); err != nil {
			return out, fmt.Errorf("Ack %s: %w", b.BatchID, err)
		}
	}
	return out, ctx.Err()
}

// fatalIf stops the probe when a step everything after depends on failed.
func fatalIf(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}
