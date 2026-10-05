// Package fakeadapter is a Harness Adapter for a harness that exists only in
// this process: the reference the conformance kit is proved against, and a
// stand-in for a harness in a runtime's tests.
//
// Its harness runs one goroutine per Session and answers the prompt language
// of agentd's P11 mock Messages API, so a scenario reads the same against it
// as against a real harness talking to that mock:
//
//	PING <n>       reply "PONG <n>"
//	SLOW <n>       reply "slow" in <n> chunks, one per tick (default 40)
//	STALL <s>      nothing for <s> seconds (default 60), then "ok"
//	TOOL <command> a tool call running <command>, then "TOOL DONE"
//	ERR <code> <k> the model call fails with <code> <k> times, retried up to
//	               MaxRetries times: "RECOVERED" once it passes, else the turn
//	               errors (529: overloaded, 401: auth, 402: billing, else api);
//	               an auth or billing error blocks the Session
//	LIMIT          the account's usage limit refuses the model call: the turn
//	               errors (usage_limit, resuming in an hour), and the Session
//	               blocks until then
//	BIG <kib>      reply with <kib> KiB of text
//	ASK            raise a prompt (yes/no); on its answer, "ANSWERED <choice>"
//	CRASH          the harness process dies mid-turn
//	MKGOAL <text>  give the Session a goal whose objective is <text>, and reply
//	               "TOOL DONE". From then on the harness works on the goal by
//	               itself: once a turn completes, it starts a turn of its own
//	               on the objective, which no input asked for
//	GOAL <n> <k>   a goal's objective: each of the first <n> turns the harness
//	               starts for it works <k> ticks and replies "goal step <i>";
//	               the next one completes the goal, and the harness rests
//	anything else  reply "ok"
//
// A turn the harness started itself is stopped by an input — it ends
// interrupted, and the input's turn follows — or by an interrupt that names
// it, after which the harness starts none until an input's turn ends or the
// Session is reopened.
//
// It keeps its record — every record-origin observation, one JSON line each —
// its submission markers and its goal under the layout's scratch root, in a
// directory named for the workspace, so a record handle, or a Session reopened
// by another Adapter value, reads what an earlier one left, as it would after
// a crash. A Session saved in one environment is therefore loaded into another
// by moving that directory to the new workspace's name: the relocation
// Provision answers a request that loads with.
package fakeadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Name is the harness name the fake registers under (Register).
const Name = "fake"

// CredentialKind is the credential kind the fake takes.
const CredentialKind = "fake_token"

// Host is where the fake presents its token, as a bearer token in
// Authorization, behind an egress broker (capability brokered_credentials).
const Host = "model.fake.example"

// CheckpointFormat is the checkpoint format the fake writes.
const CheckpointFormat = 1

// MaxRetries bounds the retries of a failing model call.
const MaxRetries = 3

// DefaultTick is the fake model's pacing unit.
const DefaultTick = 20 * time.Millisecond

// Options configure an Adapter.
type Options struct {
	// Tick is the model's pacing unit; DefaultTick when zero.
	Tick time.Duration
	// StartDelay is how long Open takes.
	StartDelay time.Duration
	// Break names one rule of the interface the adapter breaks, for the
	// conformance kit's negative tests; "" breaks none. Breaks lists them.
	Break string
}

// Breaks are the rules an Adapter can be told to break.
var Breaks = []string{
	"busy-maybe-submitted", "unstable-ids", "replay-new-batch", "interrupt-other-turn",
	"two-outcomes", "recover-not-found", "drained-without-acks", "no-rescan", "open-gate",
	"empty-poll-batch", "impure-provision", "fresh-checkpoint", "ack-unknown", "no-turn-started",
	"drop-oversize", "interrupt-twice-differs", "answer-after-taken", "no-truncate",
	"no-record-turn-end", "unsent-unknown", "no-session-exited", "close-differs",
	"block-on-overloaded", "ignore-checkpoint", "no-retrying", "prompt-pending-maybe",
	"idle-too-late", "send-after-close",
	"load-starts-fresh", "load-keeps-path", "load-any-source", "load-forgets", "load-drops-goal",
	"auto-unreported", "auto-send-busy", "auto-input-id", "auto-interrupt-ignored", "auto-restarts",
	"auto-no-record-end",
	"egress-without-capability", "impure-placeholder", "placeholder-ignores-nonce", "placeholder-holds-secret",
	"placeholder-off-route", "keeper-forgets", "keeper-lends-unbrokerable", "keeper-signout-keeps",
}

// Adapter is the fake harness's adapter.
type Adapter struct {
	opts Options
	// placeholders counts Placeholder's calls, for a break that makes it impure.
	placeholders atomic.Int64
}

// New returns an Adapter.
func New(opts Options) *Adapter {
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	return &Adapter{opts: opts}
}

// Register registers a fake Adapter under Name.
func Register() { contract.Register(Name, New(Options{})) }

func (a *Adapter) breaks(rule string) bool { return a.opts.Break == rule }

// Version is the fake harness's version.
const Version = "1.0.0"

// ArchiveFormat is the archive format the fake's load recipe is written for.
const ArchiveFormat = 2

// Describe describes the fake: every capability, every spec field.
func (a *Adapter) Describe() contract.Descriptor {
	return contract.Descriptor{
		Contract:     contract.Version,
		Harness:      contract.HarnessInfo{Name: Name, Version: Version, Adapter: "harness-wrapper fakeadapter"},
		Capabilities: a.capabilities(),
		Load:         &contract.LoadSupport{Formats: []int{ArchiveFormat}, Sources: []string{Version}},
		Egress: &contract.Egress{
			Hosts:       []string{Host},
			Credentials: []contract.CredentialRoute{{Kind: CredentialKind, Hosts: []string{Host}, Headers: []string{"Authorization"}}},
		},
		Keeper:           &contract.KeeperSupport{Kind: CredentialKind},
		CheckpointFormat: CheckpointFormat,
		CredentialKinds:  []string{CredentialKind},
		Spec: contract.SpecSupport{
			Models:             contract.Models{Any: true},
			Efforts:            []string{"low", "medium", "high"},
			Instructions:       []string{contract.InstructionPersona, contract.InstructionWorkspace},
			Skills:             true,
			Memory:             true,
			Connectors:         []string{contract.ConnectorStdio, contract.ConnectorHTTP},
			PermissionPostures: []string{contract.PostureBypass, contract.PostureGated},
			InputContent:       []string{contract.ContentText},
		},
		Limits: contract.Limits{MaxInputBytes: contract.MaxInputBytes},
	}
}

func (a *Adapter) capabilities() []contract.Capability {
	caps := []contract.Capability{
		contract.CapResume, contract.CapAssignSessionID, contract.CapPrompts, contract.CapStreamingText,
		contract.CapToolsObserved, contract.CapRetryVisible, contract.CapSessionLoad, contract.CapAutonomousTurns,
	}
	if !a.breaks("egress-without-capability") {
		caps = append(caps, contract.CapBrokeredCredentials)
	}
	return append(caps, contract.CapLoginKeeper)
}

// Placeholder renders the fake token's placeholder: "fake-" and 32 hex digits
// drawn from the nonce, staged as the file and swapped at Host.
func (a *Adapter) Placeholder(req contract.PlaceholderRequest) (contract.PlaceholderResult, error) {
	if !contract.Compatible(req.Contract) {
		return contract.PlaceholderResult{}, contract.Errorf(contract.CodeProtocol, "contract %q", req.Contract)
	}
	if req.Kind != CredentialKind {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeUnsupported, Field: "kind", Message: "no route for " + req.Kind}
	}
	if len(req.Nonce) < contract.MinNonceBytes {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "nonce", Message: "too short"}
	}
	tok := strings.TrimSpace(string(req.Credential))
	if tok == "" || strings.ContainsAny(tok, "\n\r\x00") {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "credential", Message: "not a token"}
	}
	seed := req.Nonce
	if a.breaks("placeholder-ignores-nonce") {
		seed = nil
	}
	sum := sha256.Sum256(append([]byte("fake placeholder\x00"), seed...))
	ph := "fake-" + hex.EncodeToString(sum[:16])
	if a.breaks("impure-placeholder") {
		ph += strconv.FormatInt(a.placeholders.Add(1), 10)
	}
	file := []byte(ph)
	if a.breaks("placeholder-holds-secret") {
		file = []byte(ph + "\n" + tok)
	}
	hosts := []string{Host}
	if a.breaks("placeholder-off-route") {
		hosts = []string{"elsewhere.fake.example"}
	}
	return contract.PlaceholderResult{
		File:  file,
		Swaps: []contract.Swap{{Placeholder: ph, Secret: tok, Hosts: hosts, Headers: []string{"Authorization"}}},
	}, nil
}

// openConfig is the fake's opaque open_config.
type openConfig struct {
	Binary string `json:"binary"`
	Model  string `json:"model,omitempty"`
}

// BinaryPath is where Provision tells the fake harness's binary to be, under
// the harness root: Open refuses with binary_not_found when it is absent.
func BinaryPath(harnessRoot string) string { return filepath.Join(harnessRoot, "bin", "fake-harness") }

// Provision renders the fake's configuration.
func (a *Adapter) Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error) {
	if !contract.Compatible(req.Contract) {
		return contract.ProvisionResult{}, contract.Errorf(contract.CodeProtocol, "contract %q", req.Contract)
	}
	if err := req.Layout.Validate(); err != nil {
		return contract.ProvisionResult{}, err
	}
	if !filepath.IsAbs(req.HarnessRoot) {
		return contract.ProvisionResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "harness_root", Message: "not absolute"}
	}
	if err := contract.CheckSpec(a.Describe(), req.Spec); err != nil {
		return contract.ProvisionResult{}, err
	}
	var moves []contract.Relocation
	if req.Load != nil {
		l := *req.Load
		if a.breaks("load-any-source") {
			l.Harness.Version = Version
		}
		if err := contract.CheckLoad(a.Describe(), l); err != nil {
			return contract.ProvisionResult{}, err
		}
		// The record sits in a directory named for the workspace it was made
		// in: it moves to the new workspace's.
		from, to := historyDir+"/"+workspaceKey(l.Workspace), historyDir+"/"+workspaceKey(req.Layout.Workspace)
		if from != to && !a.breaks("load-keeps-path") {
			moves = []contract.Relocation{{
				From: contract.RootPath{Root: contract.RootScratch, Path: from}, To: contract.RootPath{Root: contract.RootScratch, Path: to},
			}}
		}
	}
	spec, _ := json.MarshalIndent(req.Spec, "", "  ")
	files := []contract.File{contract.TextFile(contract.RootConfig, "fake.json", "0600", string(spec)+"\n")}
	if a.breaks("impure-provision") {
		files = append(files, contract.TextFile(contract.RootConfig, "rendered-at", "0600", time.Now().String()))
	}
	if p := req.Spec.Instructions.Persona; p != "" {
		files = append(files, contract.TextFile(contract.RootConfig, "persona.md", "0600", p))
	}
	if w := req.Spec.Instructions.Workspace; w != "" {
		files = append(files, contract.TextFile(contract.RootWorkspace, "FAKE.md", "0644", w))
	}
	for _, sk := range req.Spec.Skills {
		for _, f := range sk.Files {
			files = append(files, contract.TextFile(contract.RootConfig, filepath.ToSlash(filepath.Join("skills", sk.Name, f.Path)), "0600", f.Content))
		}
	}
	if req.Spec.Memory != nil {
		for _, f := range req.Spec.Memory.Files {
			files = append(files, contract.TextFile(contract.RootConfig, filepath.ToSlash(filepath.Join("memory", f.Path)), "0600", f.Content))
		}
	}
	cfg, _ := json.Marshal(openConfig{Binary: BinaryPath(req.HarnessRoot), Model: req.Spec.Model})
	return contract.ProvisionResult{
		Files:              files,
		OpenConfig:         cfg,
		HistoryRoots:       []contract.RootPath{{Root: contract.RootScratch, Path: historyDir}},
		SecretPaths:        []contract.RootPath{{Root: contract.RootConfig, Path: "fake.json"}},
		HistoryRelocations: moves,
	}, nil
}

// historyDir is the fake's one history root, beneath the scratch root.
const historyDir = "fake"

// workspaceKey names the directory a workspace's Sessions are kept in: the
// fake, like a harness that names its record for its working directory, keeps
// them by the workspace's path.
func workspaceKey(workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	return "w" + hex.EncodeToString(sum[:8])
}

// NewSession returns an unopened Session.
func (a *Adapter) NewSession(req contract.OpenRequest) (contract.Session, error) {
	if err := req.Validate(); err != nil && (!a.breaks("fresh-checkpoint") || req.Mode != contract.OpenFresh) {
		return nil, err
	}
	if err := req.Layout.Validate(); err != nil {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "layout", Message: err.Error()}
	}
	var cfg openConfig
	if err := json.Unmarshal(req.OpenConfig, &cfg); err != nil {
		return nil, &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenConfigInvalid, Message: err.Error()}
	}
	return newSession(a, req, cfg), nil
}

// OpenRecord opens a read-only handle on a Session's record.
func (a *Adapter) OpenRecord(_ context.Context, req contract.RecordRequest) (contract.Record, error) {
	if !contract.ValidID(req.SessionID) {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "session_id"}
	}
	if err := req.Layout.Validate(); err != nil {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "layout", Message: err.Error()}
	}
	st := store{dir: sessionDir(req.Layout, req.SessionID), adapter: a}
	if _, err := os.Stat(st.dir); err != nil {
		return nil, &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenSessionNotFound, Message: err.Error()}
	}
	r := &record{adapter: a, store: st, cursor: newCursor(a)}
	if err := r.cursor.load(st, req.Checkpoint); err != nil {
		return nil, err
	}
	return r, nil
}

// ---- the record on disk

// sessionDir is a Session's directory: its record, its markers and its goal.
func sessionDir(l contract.Layout, sessionID string) string {
	return filepath.Join(l.Scratch, historyDir, workspaceKey(l.Workspace), sessionID)
}

// store is a Session's durable files.
type store struct {
	dir     string
	adapter *Adapter
}

func (s store) recordPath() string          { return filepath.Join(s.dir, "record.jsonl") }
func (s store) markerPath(in string) string { return filepath.Join(s.dir, "markers", in) }
func (s store) goalPath() string            { return filepath.Join(s.dir, "goal.json") }

// goal is a Session's goal: what the harness works on by itself while it is
// active.
type goal struct {
	Objective string `json:"objective"`
	Active    bool   `json:"active"`
	// Turns counts the turns the harness started for it; Seq, every turn
	// the harness ever started by itself in the Session, which names them.
	Turns int `json:"turns"`
	Seq   int `json:"seq"`
}

// readGoal reads the Session's goal; nil when it has none.
func (s store) readGoal() *goal {
	b, err := os.ReadFile(s.goalPath())
	if err != nil {
		return nil
	}
	var g goal
	if json.Unmarshal(b, &g) != nil {
		return nil
	}
	return &g
}

func (s store) writeGoal(g goal) {
	b, _ := json.Marshal(g)
	_ = os.WriteFile(s.goalPath(), b, 0o600)
}

func (s store) create() error {
	if err := os.MkdirAll(filepath.Join(s.dir, "markers"), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.recordPath(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// mark writes, and fsyncs, the submission marker for an input before it is
// handed to the harness.
func (s store) mark(inputID string) error {
	f, err := os.OpenFile(s.markerPath(inputID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.markerPath(inputID)))
}

// marked reports whether the marker store holds inputID's marker; an error
// when the store cannot be read, which proves nothing.
func (s store) marked(inputID string) (bool, error) {
	if _, err := os.Stat(filepath.Join(s.dir, "markers")); err != nil {
		return false, err
	}
	_, err := os.Stat(s.markerPath(inputID))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// appendRecord appends one observation to the record and fsyncs it, and
// returns the record's length after it.
func (s store) appendRecord(o contract.Observation) (int64, error) {
	line, err := json.Marshal(o)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(s.recordPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// readRecord reads the record's complete lines from offset: each observation
// and the offset after it. A trailing partial line is not read.
func (s store) readRecord(offset int64) ([]recordLine, []byte, error) {
	b, err := os.ReadFile(s.recordPath())
	if err != nil {
		return nil, nil, err
	}
	if offset > int64(len(b)) {
		return nil, b, nil
	}
	var out []recordLine
	pos := offset
	for pos < int64(len(b)) {
		i := int64(-1)
		for j := pos; j < int64(len(b)); j++ {
			if b[j] == '\n' {
				i = j
				break
			}
		}
		if i < 0 {
			break // a partial line: not yet a fact
		}
		var o contract.Observation
		if err := json.Unmarshal(b[pos:i], &o); err != nil {
			return nil, b, fmt.Errorf("record line at %d: %w", pos, err)
		}
		out = append(out, recordLine{obs: o, end: i + 1})
		pos = i + 1
	}
	return out, b, nil
}

type recordLine struct {
	obs contract.Observation
	end int64
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// ---- checkpoints

// checkpointData is format 1: how far the record was committed, and a digest
// of that prefix, so a record rewritten since is recognized.
type checkpointData struct {
	Offset int64  `json:"offset"`
	Prefix string `json:"prefix_sha256"`
}

func encodeCheckpoint(offset int64, content []byte) *contract.Checkpoint {
	sum := sha256.Sum256(content[:offset])
	data, _ := json.Marshal(checkpointData{Offset: offset, Prefix: hex.EncodeToString(sum[:])})
	return &contract.Checkpoint{Format: CheckpointFormat, Data: data}
}

// ---- the cursor: observations delivered in batches, acknowledged, replayed

// item is one observation to deliver; end is the record offset after it, for
// a record item, and 0 for a live one.
type item struct {
	obs contract.Observation
	end int64
}

// cursor is the observe/ack state both a Session and a record handle keep.
type cursor struct {
	adapter *Adapter

	mu      sync.Mutex
	cond    chan struct{} // closed and replaced whenever pending grows
	pending []item
	// committed is the record offset the last acknowledged batch covered.
	committed int64
	// outstanding is the delivered, unacknowledged batch, and how many
	// pending items it covers.
	outstanding *contract.Batch
	outCount    int
	outEnd      int64
	lastAcked   string
	batchSeq    int
	// reset and rescan ride on the next batch.
	reset  *contract.Reset
	rescan *contract.Rescan
	// observing guards against two concurrent Observes.
	observing bool
	// recordRead is how far the record has been read into pending.
	recordRead int64
	nonce      string
}

func newCursor(a *Adapter) *cursor {
	return &cursor{adapter: a, cond: make(chan struct{}), nonce: fmt.Sprintf("%d", time.Now().UnixNano())}
}

// load positions the cursor at cp in s's record and queues every record item
// after it.
func (c *cursor) load(s store, cp *contract.Checkpoint) error {
	lines, content, err := s.readRecord(0)
	if err != nil {
		return err
	}
	start := int64(0)
	if cp != nil && !c.adapter.breaks("ignore-checkpoint") {
		var d checkpointData
		switch {
		case cp.Format != CheckpointFormat || json.Unmarshal(cp.Data, &d) != nil:
			if c.adapter.breaks("no-rescan") {
				return contract.Errorf(contract.CodeProtocol, "checkpoint format %d", cp.Format)
			}
			c.rescan = &contract.Rescan{Reason: fmt.Sprintf("checkpoint format %d is not one this adapter reads", cp.Format)}
		case d.Offset > int64(len(content)) || hexSum(content[:d.Offset]) != d.Prefix:
			c.reset = &contract.Reset{Reason: "the record no longer continues the checkpoint", Previous: cp}
		default:
			start = d.Offset
		}
	}
	c.committed = start
	c.recordRead = start
	for _, l := range lines {
		if l.end <= start {
			continue
		}
		c.pending = append(c.pending, item{obs: c.stamp(l.obs), end: l.end})
		c.recordRead = l.end
	}
	return nil
}

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stamp applies the "unstable-ids" break: an id that changes per process.
func (c *cursor) stamp(o contract.Observation) contract.Observation {
	if c.adapter.breaks("unstable-ids") && o.Origin == contract.OriginRecord {
		o.ID += "#" + c.nonce
	}
	return o
}

// push queues observations and wakes a waiting Observe.
func (c *cursor) push(items ...item) {
	c.mu.Lock()
	for i := range items {
		items[i].obs = c.stamp(items[i].obs)
	}
	c.pending = append(c.pending, items...)
	for _, it := range items {
		if it.end > c.recordRead {
			c.recordRead = it.end
		}
	}
	close(c.cond)
	c.cond = make(chan struct{})
	c.mu.Unlock()
}

// observe returns the outstanding batch, or builds the next one, waiting up
// to wait for anything to deliver.
func (c *cursor) observe(ctx context.Context, wait time.Duration, maxBytes int, s store, endOfRecord bool, alive func() bool) (contract.Batch, error) {
	if wait < 0 || wait > contract.MaxObserveWait {
		return contract.Batch{}, contract.Errorf(contract.CodeProtocol, "wait %s", wait)
	}
	if maxBytes < contract.MinObserveBytes || maxBytes > contract.MaxObserveBytes {
		return contract.Batch{}, contract.Errorf(contract.CodeProtocol, "max_bytes %d", maxBytes)
	}
	c.mu.Lock()
	if c.observing {
		c.mu.Unlock()
		return contract.Batch{}, contract.Errorf(contract.CodeUnexpected, "an observe is already pending")
	}
	c.observing = true
	defer func() {
		c.mu.Lock()
		c.observing = false
		c.mu.Unlock()
	}()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		if c.outstanding != nil {
			b := *c.outstanding
			if c.adapter.breaks("replay-new-batch") {
				c.batchSeq++
				b.BatchID = fmt.Sprintf("b%d-%s", c.batchSeq, c.nonce)
				c.outstanding.BatchID = b.BatchID
			}
			c.mu.Unlock()
			return b, nil
		}
		if len(c.pending) > 0 || c.reset != nil || c.rescan != nil {
			b, err := c.buildLocked(maxBytes, s)
			c.mu.Unlock()
			return b, err
		}
		wake := c.cond
		c.mu.Unlock()
		if endOfRecord && (alive == nil || !alive()) {
			return contract.Batch{Items: []contract.Observation{}, EndOfRecord: true}, nil
		}
		select {
		case <-ctx.Done():
			return contract.Batch{}, ctx.Err()
		case <-deadline.C:
			b := contract.Batch{Items: []contract.Observation{}}
			if c.adapter.breaks("empty-poll-batch") {
				b.BatchID = "empty"
			}
			return b, nil
		case <-wake:
		}
		c.mu.Lock()
	}
}

// buildLocked makes the next batch from pending, within maxBytes.
func (c *cursor) buildLocked(maxBytes int, s store) (contract.Batch, error) {
	b := contract.Batch{Items: []contract.Observation{}, Reset: c.reset, Rescan: c.rescan}
	size := 256 // the envelope
	n, end := 0, c.committed
	for _, it := range c.pending {
		enc, _ := json.Marshal(it.obs)
		if size+len(enc)+1 > maxBytes {
			if n == 0 {
				if c.adapter.breaks("drop-oversize") {
					c.pending = c.pending[1:]
					return contract.Batch{Items: []contract.Observation{}}, nil
				}
				return contract.Batch{}, &contract.Error{Code: contract.CodeBatchTooLarge, RequiredBytes: size + len(enc) + 1, Message: "the next observation does not fit"}
			}
			break
		}
		size += len(enc) + 1
		b.Items = append(b.Items, it.obs)
		if it.end > end {
			end = it.end
		}
		n++
	}
	if end > c.committed || c.reset != nil || c.rescan != nil {
		_, content, err := s.readRecord(0)
		if err == nil && end <= int64(len(content)) {
			b.Checkpoint = encodeCheckpoint(end, content)
		}
	}
	c.batchSeq++
	b.BatchID = fmt.Sprintf("b%d", c.batchSeq)
	c.outstanding = &b
	c.outCount = n
	c.outEnd = end
	c.reset, c.rescan = nil, nil
	return b, nil
}

// ack acknowledges the outstanding batch.
func (c *cursor) ack(batchID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outstanding != nil && c.outstanding.BatchID == batchID {
		c.pending = c.pending[c.outCount:]
		c.committed = c.outEnd
		c.lastAcked = batchID
		c.outstanding = nil
		close(c.cond)
		c.cond = make(chan struct{})
		return nil
	}
	if batchID != "" && batchID == c.lastAcked {
		return nil
	}
	if c.adapter.breaks("ack-unknown") {
		return nil
	}
	return contract.Errorf(contract.CodeUnexpected, "batch %q was not delivered, or is older than the last acknowledged", batchID)
}

// waitDrained waits until drained or ctx ends.
func (c *cursor) waitDrained(ctx context.Context) bool {
	for {
		c.mu.Lock()
		done := len(c.pending) == 0 && c.outstanding == nil
		wake := c.cond
		c.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-wake:
		case <-time.After(5 * time.Millisecond):
		}
	}
}
