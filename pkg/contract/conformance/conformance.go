// Package conformance is the Harness Adapter Interface's conformance kit: a
// fake Agent Adapter — Supervisor and Host both — and scenarios that drive an
// adapter through the interface and check it keeps the contract.
//
// A scenario speaks the prompt language of agentd's P11 mock Messages API
// (PING, SLOW, STALL, TOOL, ERR, BIG, ASK, LIMIT for a usage wall, and MKGOAL
// and GOAL for work the harness does by itself; see fakeadapter), so it runs
// the same against the fake adapter as against a real harness that talks to
// such a mock. Each check is a rule, named in its failure ("[turn.one-outcome] …"),
// and every rule has a deliberately broken adapter that fails it.
//
// The kit plays the Supervisor's part too where a scenario needs one: Apply
// writes a Provision result, and Save and Restore move a Session's history
// from one agent's roots to another's, as an archive does.
//
//	func TestConformance(t *testing.T) {
//	    conformance.Run(conformance.Testing(t), conformance.Fixture{...})
//	}
package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// T is what the kit reports through: *testing.T, through Testing, or a
// recorder in the kit's own negative tests.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Run(name string, f func(T)) bool
	TempDir() string
}

// Testing adapts a *testing.T.
func Testing(t *testing.T) T { return testingT{t} }

type testingT struct{ *testing.T }

func (t testingT) Run(name string, f func(T)) bool {
	return t.T.Run(name, func(t *testing.T) { f(testingT{t}) })
}

// Fixture is the adapter under test and what the kit needs to drive it.
type Fixture struct {
	// Adapter is the adapter under test.
	Adapter contract.Adapter
	// HarnessRoot is its distribution's root.
	HarnessRoot string
	// Spec is an Agent Spec it provisions; the kit sets the credential kind.
	Spec contract.AgentSpec
	// Credential stages a valid credential for a layout and returns it; nil
	// for a harness that needs none.
	Credential func(t T, l contract.Layout) *contract.CredentialFile
	// Provisioned adjusts a rendered result before it is applied: a real
	// harness's fixture points it at the mock model API here.
	Provisioned func(t T, l contract.Layout, r *contract.ProvisionResult)
	// Kill crashes a Session's harness process without Close.
	Kill func(t T, s contract.Session)
	// HideBinary makes the harness binary unavailable until restore is
	// called; nil skips the scenario that needs it.
	HideBinary func(t T) (restore func())
	// Heard is what the Session's harness last sent its model, as text: the
	// conversation it carries. A real harness's fixture reads it off the mock
	// model API's last request; nil skips the checks that a loaded Session
	// remembers.
	Heard func(t T, s contract.Session) string
	// Timeout bounds each wait; 20s when zero.
	Timeout time.Duration
	// Quiet is how long a harness that rests is watched for a turn it must
	// not start; 1500ms when zero.
	Quiet time.Duration
	// Skip names scenarios the harness cannot run, with the reason.
	Skip map[string]string
	// Approve approves a keeper's device-code sign-in (capability
	// login_keeper), as its user would; nil skips the scenario that needs
	// it.
	Approve func(t T, k contract.Keeper, dc contract.DeviceCode)
}

func (f Fixture) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return 20 * time.Second
}

func (f Fixture) quiet() time.Duration {
	if f.Quiet > 0 {
		return f.Quiet
	}
	return 1500 * time.Millisecond
}

// scenario is one named scenario.
type scenario struct {
	name string
	run  func(c *check)
}

// Scenarios lists the kit's scenarios, in the order Run runs them.
func Scenarios() []string {
	var out []string
	for _, s := range scenarios {
		out = append(out, s.name)
	}
	return out
}

// Run runs every scenario against the fixture, each as a subtest.
func Run(t T, f Fixture) {
	t.Helper()
	for _, s := range scenarios {
		t.Run(s.name, func(t T) {
			if why, skip := f.Skip[s.name]; skip {
				t.Logf("skipped: %s", why)
				return
			}
			c := &check{t: t, f: f, desc: f.Adapter.Describe(), sent: map[string]bool{}}
			defer c.cleanup()
			func() {
				defer func() {
					if r := recover(); r != nil && r != errStop {
						c.fail("scenario", "panic: %v", r)
					}
				}()
				s.run(c)
			}()
		})
	}
}

// errStop ends a scenario whose next steps depend on a failed one.
var errStop = errors.New("conformance: scenario stopped")

// check is one scenario's state.
type check struct {
	t        T
	f        Fixture
	desc     contract.Descriptor
	cleanups []func()
	// sent is every input the scenario sent: a turn of none of them is one
	// the harness started itself.
	sent map[string]bool
	// base, when set, is where the scenario's one agent has its roots; its
	// agents each take a temporary directory otherwise.
	base string
}

func (c *check) cleanup() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

// fail reports a broken rule.
func (c *check) fail(rule, format string, args ...any) {
	c.t.Helper()
	c.t.Errorf("[%s] %s", rule, fmt.Sprintf(format, args...))
}

// stop reports a broken rule and ends the scenario.
func (c *check) stop(rule, format string, args ...any) {
	c.t.Helper()
	c.fail(rule, format, args...)
	panic(errStop)
}

func (c *check) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.f.timeout())
}

func (c *check) has(cap contract.Capability) bool { return c.desc.Has(cap) }

// ---- the Supervisor's part: layout, provision, apply

// agent is one agent's roots, provisioned.
type agent struct {
	base   string // the directory its roots are in
	layout contract.Layout
	result contract.ProvisionResult
	cred   *contract.CredentialFile
}

// roots makes an agent's roots, empty, with its credential staged. Their
// paths are resolved, as a Supervisor's are: Provision is pure and cannot
// resolve them, and a harness may name what it keeps for the resolved path of
// its working directory.
func (c *check) roots() *agent {
	base := c.base
	if base == "" {
		base = c.t.TempDir()
	} else if err := os.MkdirAll(base, 0o700); err != nil {
		c.stop("setup", "layout: %v", err)
	}
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		c.stop("setup", "layout: %v", err)
	}
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"),
		Workspace: filepath.Join(base, "workspace"), Secrets: filepath.Join(base, "secrets"),
		Scratch: filepath.Join(base, "scratch"),
	}
	for _, r := range []contract.Root{contract.RootHome, contract.RootConfig, contract.RootWorkspace, contract.RootSecrets, contract.RootScratch} {
		if err := os.MkdirAll(l.Path(r), 0o700); err != nil {
			c.stop("setup", "layout: %v", err)
		}
	}
	a := &agent{base: base, layout: l}
	if c.f.Credential != nil {
		a.cred = c.f.Credential(c.t, l)
	}
	return a
}

// request is the Provision request for a's roots; load names a Session to
// load into them.
func (c *check) request(a *agent, load *contract.LoadSource) contract.ProvisionRequest {
	spec := c.f.Spec
	if a.cred != nil {
		spec.Credential = &contract.CredentialRef{Kind: a.cred.Kind}
	}
	return contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: c.f.HarnessRoot, Layout: a.layout, Spec: spec, Load: load}
}

// provision renders a's configuration, adjusted as the fixture wants it.
func (c *check) provision(a *agent, load *contract.LoadSource) (contract.ProvisionResult, error) {
	res, err := c.f.Adapter.Provision(c.request(a, load))
	if err != nil {
		return contract.ProvisionResult{}, err
	}
	if c.f.Provisioned != nil {
		c.f.Provisioned(c.t, a.layout, &res)
	}
	return res, nil
}

func (c *check) newAgent() *agent {
	a := c.roots()
	res, err := c.provision(a, nil)
	if err != nil {
		c.stop("provision.valid", "Provision: %v", err)
	}
	if err := Apply(a.layout, res); err != nil {
		c.stop("provision.valid", "applying the result: %v", err)
	}
	a.result = res
	return a
}

// Apply writes a ProvisionResult under a layout the way a Supervisor does:
// each file beneath its root, never through a symlink, with its mode. It
// validates the result first.
func Apply(l contract.Layout, r contract.ProvisionResult) error {
	if err := r.Validate(); err != nil {
		return err
	}
	for _, f := range r.Files {
		root, err := os.OpenRoot(l.Path(f.Root))
		if err != nil {
			return err
		}
		mode, _ := f.FileMode()
		if dir := filepath.Dir(f.Path); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				_ = root.Close()
				return err
			}
		}
		err = root.WriteFile(f.Path, f.Content(), mode)
		_ = root.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *check) openRequest(a *agent, mode contract.OpenMode, sessionID string, cp *contract.Checkpoint) contract.OpenRequest {
	return contract.OpenRequest{Mode: mode, SessionID: sessionID, OpenConfig: a.result.OpenConfig, Layout: a.layout, Credential: a.cred, Checkpoint: cp}
}

// ---- the Host's part: a session, its observations, their acknowledgement

// host drives one Session or record handle: it observes in the background,
// commits every batch at once and acknowledges it, unless paused.
type host struct {
	c    *check
	s    contract.Session
	obs  observer
	id   string // the Session's id
	stop chan struct{}
	done chan struct{}

	mu        sync.Mutex
	wake      chan struct{}
	all       []contract.Observation // every delivery, duplicates included
	byID      map[string]contract.Observation
	batches   []contract.Batch
	committed *contract.Checkpoint
	paused    bool
	errs      []error
}

// observer is what a Session and a record handle share.
type observer interface {
	Observe(ctx context.Context, wait time.Duration, maxBytes int) (contract.Batch, error)
	Ack(batchID string) error
}

func (c *check) watch(o observer) *host {
	h := &host{c: c, obs: o, stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}), byID: map[string]contract.Observation{}}
	go h.pump()
	c.cleanups = append(c.cleanups, h.halt)
	return h
}

func (h *host) halt() {
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	<-h.done
}

func (h *host) pump() {
	defer close(h.done)
	for {
		select {
		case <-h.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		b, err := h.obs.Observe(ctx, 100*time.Millisecond, contract.MaxObserveBytes)
		cancel()
		if err != nil {
			var e *contract.Error
			if errors.As(err, &e) && e.Code == contract.CodeUnexpected {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			h.mu.Lock()
			h.errs = append(h.errs, err)
			h.mu.Unlock()
			select {
			case <-h.stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}
		if !b.NeedsAck() {
			continue
		}
		h.mu.Lock()
		h.batches = append(h.batches, b)
		for _, o := range b.Items {
			h.all = append(h.all, o)
			if _, dup := h.byID[o.ID]; !dup {
				h.byID[o.ID] = o
			}
		}
		paused := h.paused
		if !paused && b.Checkpoint != nil {
			cp := *b.Checkpoint
			h.committed = &cp
		}
		close(h.wake)
		h.wake = make(chan struct{})
		h.mu.Unlock()
		if paused {
			// Not committed, so not acknowledged: the adapter holds this
			// batch, and delivers nothing after it, until it is acked.
			<-h.stop
			return
		}
		if err := h.obs.Ack(b.BatchID); err != nil {
			h.mu.Lock()
			h.errs = append(h.errs, fmt.Errorf("ack %s: %w", b.BatchID, err))
			h.mu.Unlock()
		}
	}
}

// pause stops committing and acknowledging.
func (h *host) pause() {
	h.mu.Lock()
	h.paused = true
	h.mu.Unlock()
}

// checkpoint is the last committed checkpoint.
func (h *host) checkpoint() *contract.Checkpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.committed
}

// ids is every stored id.
func (h *host) ids() map[string]contract.Observation {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]contract.Observation, len(h.byID))
	for k, v := range h.byID {
		out[k] = v
	}
	return out
}

// deliveries is every delivery of id, duplicates included.
func (h *host) deliveries(pred func(contract.Observation) bool) []contract.Observation {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []contract.Observation
	for _, o := range h.all {
		if pred(o) {
			out = append(out, o)
		}
	}
	return out
}

// await waits until pred holds of a stored observation.
func (h *host) await(what string, pred func(contract.Observation) bool) (contract.Observation, bool) {
	deadline := time.After(h.c.f.timeout())
	for {
		h.mu.Lock()
		for _, o := range h.all {
			if pred(o) {
				h.mu.Unlock()
				return o, true
			}
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-wake:
		case <-deadline:
			return contract.Observation{}, false
		}
	}
}

func (h *host) awaitKind(kind contract.Kind, inputID string) (contract.Observation, bool) {
	return h.await(string(kind)+" of "+inputID, func(o contract.Observation) bool {
		return o.Kind == kind && (inputID == "" || o.InputID == inputID)
	})
}

// turnEnded waits for the input's turn to end and decodes the end.
func (h *host) turnEnded(inputID string) (contract.TurnEndedData, bool) {
	o, ok := h.awaitKind(contract.KindTurnEnded, inputID)
	if !ok {
		return contract.TurnEndedData{}, false
	}
	var d contract.TurnEndedData
	if err := o.Decode(&d); err != nil {
		return contract.TurnEndedData{}, false
	}
	return d, true
}

// ---- opening Sessions

var inputSeq struct {
	sync.Mutex
	n int
}

// newInputID is an input id unique for the process's life.
func newInputID() string {
	inputSeq.Lock()
	defer inputSeq.Unlock()
	inputSeq.n++
	return fmt.Sprintf("in_%d_%d", time.Now().UnixNano()%1e9, inputSeq.n)
}

// openSession opens a fresh Session for a and watches it.
func (c *check) openSession(a *agent) (contract.Session, *host) {
	s, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenFresh, "", nil))
	if err != nil {
		c.stop("open.idle", "NewSession: %v", err)
	}
	return c.openWatch(s, "open.idle")
}

func (c *check) openWatch(s contract.Session, rule string) (contract.Session, *host) {
	ctx, cancel := c.ctx()
	defer cancel()
	res, err := s.Open(ctx)
	if err != nil {
		c.stop(rule, "Open: %v", err)
	}
	c.cleanups = append(c.cleanups, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = s.Close(ctx, contract.ClosePark, 0)
	})
	h := c.watch(s)
	h.s, h.id = s, res.SessionID
	return s, h
}

// send submits text and requires the receipt.
func (c *check) send(s contract.Session, text string) string {
	in := newInputID()
	c.sent[in] = true
	ctx, cancel := c.ctx()
	defer cancel()
	res, err := s.Send(ctx, contract.Text(in, text))
	if err != nil {
		c.stop("turn.receipt", "Send(%q): %v", text, err)
	}
	if res.Receipt != contract.ReceiptSubmitted || res.TurnID == "" {
		c.fail("turn.receipt", "Send(%q) = %+v, want submitted with a turn id", text, res)
	}
	return in
}

// awaitIdle waits for the Session to leave busy.
func (c *check) awaitPhase(s contract.Session, want ...contract.Phase) contract.Phase {
	deadline := time.Now().Add(c.f.timeout())
	for {
		p := s.State().Phase
		for _, w := range want {
			if p == w {
				return p
			}
		}
		if time.Now().After(deadline) {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func codeOf(err error) contract.Code {
	if err == nil {
		return ""
	}
	return contract.CodeOf(err)
}

func certaintyOf(err error) contract.Certainty {
	var e *contract.Error
	if errors.As(err, &e) {
		return e.Certainty
	}
	return ""
}

// stacks returns a goroutine dump, for a failure that hung.
func stacks() string {
	b := make([]byte, 1<<16)
	return string(b[:runtime.Stack(b, true)])
}

// removeFunc removes a file; a test may replace it.
var removeFunc = os.Remove
