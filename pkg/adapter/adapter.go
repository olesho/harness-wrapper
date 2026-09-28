// Package adapter is harness-wrapper's Harness Adapter: one implementation of
// the Harness Adapter Interface (pkg/contract), with a profile per harness.
// The Claude Code Adapter and the Codex Adapter are this adapter with its
// Claude Code and Codex profiles.
//
// This package is the shared part, and names no harness:
//
//   - the Session: its states, the admission gate, one outstanding send, an
//     interrupt that names its input, answers, and Close with stopped and
//     drained;
//   - Observe and Ack: one cursor over the live events and the profile's
//     record, with the checkpoint on the batch that covers a chunk's last
//     item;
//   - submission markers, written and synced before an input reaches the
//     harness, and OpenRecord and Recover built on them and the profile's
//     record.
//
// A profile supplies what is its harness's own (Profile): the Descriptor,
// Provision, a Transport to a running harness, and a Reader of its record.
// Each profile registers itself under its harness's name with Register, in
// its package's init, so a runtime's harness list links one profile per
// harness it selects.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Profile is what one harness supplies.
type Profile interface {
	// Describe is the harness's Descriptor. It is pure.
	Describe() contract.Descriptor
	// Provision renders the harness's configuration for a spec. It is pure:
	// no I/O, and the same request always gives the same result.
	Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error)
	// Start launches the harness for a Session and returns its transport.
	// It fails with a *contract.Error of code open_failed and its reason.
	// ctx bounds the launch; cancelling it abandons it.
	Start(ctx context.Context, req Start) (Transport, error)
	// Record returns a reader of one Session's durable record, from the
	// checkpoint the source names. It needs no harness binary and no
	// credential, and starts no turn.
	Record(src RecordSource) (Reader, error)
}

// Start is what Profile.Start launches a harness from.
type Start struct {
	Mode contract.OpenMode
	// SessionID is the Session to reopen, or the id a fresh open asked for;
	// "" lets the harness choose.
	SessionID  string
	OpenConfig []byte
	Layout     contract.Layout
	Credential *contract.CredentialFile
	// Report receives the harness's events, in the order it reports them,
	// from one goroutine at a time. Exited is the last.
	Report func(Event)
}

// Transport is a running harness.
type Transport interface {
	// SessionID is the harness's own id for the Session.
	SessionID() string
	// Submit hands an input to the harness, and returns once the harness has
	// it. An error wrapping ErrNotSubmitted guarantees nothing reached the
	// harness; any other means the input may have.
	Submit(ctx context.Context, s Submission) error
	// Interrupt asks the harness to stop its current turn, and returns once
	// it has the request. The turn's end comes as an Ended event.
	Interrupt(ctx context.Context) error
	// Answer answers a prompt the harness raised.
	Answer(ctx context.Context, promptID string, c contract.Choice) error
	// Stop ends the harness's process group, TERM first and KILL once grace
	// ends, and reports whether every process of it is gone. It returns once
	// they are, or ctx ends.
	Stop(ctx context.Context, grace time.Duration) (stopped bool)
}

// ErrNotSubmitted marks a Submit failure that reached nothing.
var ErrNotSubmitted = errors.New("adapter: input not submitted")

// Submission is one input for the harness.
type Submission struct {
	InputID string
	// Native is the input's id in the harness's own terms, from its
	// submission marker: the id the harness's record will carry.
	Native string
	Text   string
}

// EventKind is what a harness reported.
type EventKind int

// Event kinds.
const (
	// Started: the input's turn began.
	Started EventKind = iota + 1
	// Ended: the input's turn ended: Outcome, and Text or Error.
	Ended
	// Retrying: the harness retries a model call of the turn (Retry).
	Retrying
	// RateLimited: the harness reported the account's usage (RateLimit).
	// ResumeAt is when a refused account can work again, when known.
	RateLimited
	// PromptRaised: the turn waits on a prompt (Prompt).
	PromptRaised
	// PromptResolved: the prompt PromptID is gone, by "answer" or "harness".
	PromptResolved
	// Exited: the harness process ended (Exit). It is the last event.
	Exited
)

// Event is one thing a harness reported.
type Event struct {
	Kind EventKind
	// Native is the input the event concerns, in the harness's terms.
	Native string
	Time   time.Time

	Outcome contract.TurnOutcome
	Text    string
	Error   *contract.TurnError

	Retry     contract.RetryingData
	RateLimit contract.RateLimitData
	ResumeAt  *time.Time

	Prompt   *contract.PromptInfo
	PromptID string
	By       string

	Exit contract.SessionExitedData
}

// RecordSource names one Session's record for Profile.Record.
type RecordSource struct {
	SessionID  string
	OpenConfig []byte
	Layout     contract.Layout
	// Checkpoint is the last one the Supervisor committed; nil reads from
	// the start. One the profile cannot read makes it read from the start,
	// reporting a rescan.
	Checkpoint *contract.Checkpoint
	// Markers maps the harness's native input ids back to input ids.
	Markers *Markers
}

// Reader reads one Session's record: the items, in record order, each an
// observation of origin record whose id is stable across reads.
type Reader interface {
	// Read returns the record's next chunk after the committed position,
	// holding roughly max bytes of items at most, without moving: until
	// Commit, every Read returns the same items. An empty chunk means
	// nothing new, for now.
	Read(ctx context.Context, max int) (Chunk, error)
	// Commit moves past a chunk once the Supervisor has committed it, and
	// may drop what it covered.
	Commit(c Chunk) error
	// Recover reads what became of an input the marker says was handed to
	// the harness: its outcome when the record proves one, unknown when it
	// cannot.
	Recover(ctx context.Context, m Marker) (contract.Recovered, error)
	Close() error
}

// Chunk is one Read of a record.
type Chunk struct {
	Items []contract.Observation
	// Checkpoint covers every item up to this chunk's last; nil when the
	// chunk leaves the position where it was.
	Checkpoint *contract.Checkpoint
	Reset      *contract.Reset
	Rescan     *contract.Rescan
	Faults     []contract.Fault
	// Token is the reader's own handle on the chunk, for Commit.
	Token any
}

func (c Chunk) empty() bool {
	return len(c.Items) == 0 && c.Checkpoint == nil && c.Reset == nil && c.Rescan == nil && len(c.Faults) == 0
}

// Register registers a profile under its harness's name, as a Harness Adapter
// (contract.Register). A profile package calls it from its init.
func Register(name string, p Profile) { contract.Register(name, New(p)) }

// New is the Harness Adapter with profile p.
func New(p Profile) contract.Adapter { return &harnessAdapter{p: p, desc: p.Describe()} }

type harnessAdapter struct {
	p    Profile
	desc contract.Descriptor
}

func (a *harnessAdapter) Describe() contract.Descriptor { return a.p.Describe() }

func (a *harnessAdapter) Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error) {
	if err := checkVersion(req.Contract); err != nil {
		return contract.ProvisionResult{}, err
	}
	if err := req.Layout.Validate(); err != nil {
		return contract.ProvisionResult{}, err
	}
	if req.HarnessRoot == "" || !filepath.IsAbs(req.HarnessRoot) || filepath.Clean(req.HarnessRoot) != req.HarnessRoot {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "harness_root", Message: "not a clean absolute path"}
	}
	if err := contract.CheckSpec(a.desc, req.Spec); err != nil {
		return contract.ProvisionResult{}, err
	}
	res, err := a.p.Provision(req)
	if err != nil {
		return contract.ProvisionResult{}, err
	}
	if err := res.Validate(); err != nil {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInternal, Message: "the profile rendered an invalid result: " + err.Error()}
	}
	return res, nil
}

func (a *harnessAdapter) NewSession(req contract.OpenRequest) (contract.Session, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := req.Layout.Validate(); err != nil {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "layout", Message: err.Error()}
	}
	if req.Mode == contract.OpenReopen && !a.desc.Has(contract.CapResume) {
		return nil, contract.Errorf(contract.CodeUnsupported, "reopen needs %s", contract.CapResume)
	}
	if req.Mode == contract.OpenFresh && req.SessionID != "" && !a.desc.Has(contract.CapAssignSessionID) {
		return nil, contract.Errorf(contract.CodeUnsupported, "choosing a fresh session's id needs %s", contract.CapAssignSessionID)
	}
	return newSession(a, req), nil
}

func (a *harnessAdapter) OpenRecord(_ context.Context, req contract.RecordRequest) (contract.Record, error) {
	if !contract.ValidID(req.SessionID) {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "session_id", Message: fmt.Sprintf("%q is not an id", req.SessionID)}
	}
	if err := req.Layout.Validate(); err != nil {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "layout", Message: err.Error()}
	}
	if len(req.OpenConfig) > contract.MaxOpenConfigBytes {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "open_config", Message: "too large"}
	}
	if c := req.Checkpoint; c != nil && len(c.Data) > contract.MaxCheckpointBytes {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "checkpoint.data", Message: "too large"}
	}
	m, err := OpenMarkers(req.Layout.Scratch)
	if err != nil {
		return nil, &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
	r, err := a.p.Record(RecordSource{
		SessionID: req.SessionID, OpenConfig: req.OpenConfig, Layout: req.Layout,
		Checkpoint: req.Checkpoint, Markers: m,
	})
	if err != nil {
		return nil, err
	}
	return newRecord(r, m, req.SessionID), nil
}

// checkVersion refuses a request written in another major version, or in a
// newer minor one than this package implements.
func checkVersion(v string) error {
	major, minor, err := contract.ParseVersion(v)
	if err != nil || major != contract.Major || minor > contract.Minor {
		return &contract.Error{Code: contract.CodeProtocol, Field: "contract", Message: fmt.Sprintf("%q, want %s or an earlier minor", v, contract.Version)}
	}
	return nil
}
