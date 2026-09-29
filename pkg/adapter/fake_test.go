package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// fakeProfile is a harness that lives in the test process, behind the shared
// adapter: a goroutine per Session answering the kit's prompt language, and a
// record — one JSON line per entry — under the scratch root, which its Reader
// reads back the way a real profile reads its harness's transcript: entries
// with ids of their own, inputs by their native ids, turn ends as evidence.
type fakeProfile struct {
	tick time.Duration

	mu    sync.Mutex
	procs map[string]*fakeTransport
}

const (
	fakeName        = "fake-profile"
	fakeCheckpoint1 = 1
	fakeMaxRetries  = 3
)

func newFakeProfile() *fakeProfile {
	return &fakeProfile{tick: 10 * time.Millisecond, procs: map[string]*fakeTransport{}}
}

func (p *fakeProfile) Describe() contract.Descriptor {
	return contract.Descriptor{
		Contract: contract.Version,
		Harness:  contract.HarnessInfo{Name: fakeName, Version: "1.0.0", Adapter: "harness-wrapper adapter test"},
		Capabilities: []contract.Capability{
			contract.CapResume, contract.CapAssignSessionID, contract.CapRetryVisible, contract.CapRateLimits,
		},
		CheckpointFormat: fakeCheckpoint1,
		Spec: contract.SpecSupport{
			Models:             contract.Models{Any: true},
			Instructions:       []string{contract.InstructionPersona},
			PermissionPostures: []string{contract.PostureBypass},
			InputContent:       []string{contract.ContentText},
		},
		Limits: contract.Limits{MaxInputBytes: contract.MaxInputBytes},
	}
}

type fakeConfig struct {
	Binary string `json:"binary"`
}

func fakeBinary(root string) string { return filepath.Join(root, "bin", "fake") }

func (p *fakeProfile) Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error) {
	cfg, _ := json.Marshal(fakeConfig{Binary: fakeBinary(req.HarnessRoot)})
	return contract.ProvisionResult{
		Files:        []contract.File{contract.TextFile(contract.RootConfig, "persona.md", "0600", req.Spec.Instructions.Persona)},
		OpenConfig:   cfg,
		HistoryRoots: []contract.RootPath{{Root: contract.RootScratch, Path: "fake"}},
	}, nil
}

func fakeDir(l contract.Layout, session string) string {
	return filepath.Join(l.Scratch, "fake", session)
}

func (p *fakeProfile) Start(ctx context.Context, req Start) (Transport, error) {
	var cfg fakeConfig
	if err := json.Unmarshal(req.OpenConfig, &cfg); err != nil {
		return nil, &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenConfigInvalid, Message: err.Error()}
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenBinaryNotFound, Message: err.Error()}
	}
	id := req.SessionID
	if id == "" {
		id = "s-" + randomHex(8)
	}
	dir := fakeDir(req.Layout, id)
	if req.Mode == contract.OpenReopen {
		if _, err := os.Stat(dir); err != nil {
			return nil, &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenSessionNotFound, Message: err.Error()}
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenConfigInvalid, Message: err.Error()}
	}
	// Launching takes a moment, as a real harness's does.
	select {
	case <-time.After(5 * p.tick):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	t := &fakeTransport{p: p, id: id, dir: dir, report: req.Report, dead: make(chan struct{})}
	p.mu.Lock()
	p.procs[id] = t
	p.mu.Unlock()
	return t, nil
}

func (p *fakeProfile) Record(src RecordSource) (Reader, error) {
	dir := fakeDir(src.Layout, src.SessionID)
	if _, err := os.Stat(dir); err != nil {
		return nil, &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenSessionNotFound, Message: err.Error()}
	}
	r := &fakeReader{path: filepath.Join(dir, "record.jsonl"), markers: src.Markers}
	if cp := src.Checkpoint; cp != nil {
		var d fakeCheckpointData
		if cp.Format != fakeCheckpoint1 || json.Unmarshal(cp.Data, &d) != nil || d.Offset < 0 {
			r.rescan = &contract.Rescan{Reason: fmt.Sprintf("checkpoint format %d is not one this profile reads", cp.Format)}
		} else {
			r.offset = d.Offset
		}
	}
	return r, r.rebuild()
}

// ---- the harness

type fakeTransport struct {
	p      *fakeProfile
	id     string
	dir    string
	report func(Event)

	mu   sync.Mutex
	turn *fakeTurn
	gone bool
	dead chan struct{}
}

type fakeTurn struct {
	native    string
	interrupt chan struct{}
	once      sync.Once
	done      chan struct{}
}

func (t *fakeTransport) SessionID() string { return t.id }

// entry is one line of the fake's record.
type fakeEntry struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Native   string              `json:"native,omitempty"`
	Text     string              `json:"text,omitempty"`
	Message  string              `json:"message,omitempty"`
	Outcome  string              `json:"outcome,omitempty"`
	Error    *contract.TurnError `json:"error,omitempty"`
	Class    string              `json:"class,omitempty"`
	Status   int                 `json:"status,omitempty"`
	Received time.Time           `json:"at"`
}

func (t *fakeTransport) write(e fakeEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gone {
		return
	}
	n, _ := countLines(filepath.Join(t.dir, "record.jsonl"))
	e.ID = "e" + strconv.Itoa(n+1)
	e.Received = time.Now().UTC()
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(filepath.Join(t.dir, "record.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Sync()
	_ = f.Close()
}

func countLines(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return bytes.Count(b, []byte{'\n'}), nil
}

func (t *fakeTransport) Submit(ctx context.Context, s Submission) error {
	t.mu.Lock()
	if t.gone {
		t.mu.Unlock()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "the harness exited"}
	}
	if t.turn != nil {
		t.mu.Unlock()
		return &contract.Error{Code: contract.CodeBusy, Certainty: contract.NotSubmitted, Message: "a turn runs"}
	}
	ft := &fakeTurn{native: s.Native, interrupt: make(chan struct{}), done: make(chan struct{})}
	t.turn = ft
	t.mu.Unlock()
	t.write(fakeEntry{Type: "user", Native: s.Native, Text: s.Text})
	go t.run(ft, s.Text)
	return nil
}

func (t *fakeTransport) Interrupt(context.Context) error {
	t.mu.Lock()
	ft := t.turn
	t.mu.Unlock()
	if ft != nil {
		ft.once.Do(func() { close(ft.interrupt) })
	}
	return nil
}

func (t *fakeTransport) Answer(context.Context, string, contract.Choice) error {
	return contract.Errorf(contract.CodeUnsupported, "no prompts")
}

// Stop ends the harness: a turn in flight is cut, with no end in the record.
func (t *fakeTransport) Stop(context.Context, time.Duration) bool {
	t.die(contract.ExitClean, "stopped")
	return true
}

// kill is a crash: the harness dies with whatever it had not recorded.
func (t *fakeTransport) kill() { t.die(contract.ExitKilled, "killed") }

func (t *fakeTransport) die(class contract.ExitClass, detail string) {
	t.mu.Lock()
	if t.gone {
		t.mu.Unlock()
		return
	}
	t.gone = true
	close(t.dead)
	ft := t.turn
	t.mu.Unlock()
	if ft != nil {
		<-ft.done
	}
	code := 0
	if class != contract.ExitClean {
		code = 137
	}
	t.report(Event{Kind: Exited, Exit: contract.SessionExitedData{Class: class, ExitCode: &code, Detail: detail}})
}

// wait waits d, and reports why it stopped waiting: "" when d passed.
func (t *fakeTransport) wait(ft *fakeTurn, d time.Duration) string {
	select {
	case <-ft.interrupt:
		return "interrupt"
	case <-t.dead:
		return "dead"
	case <-time.After(d):
		return ""
	}
}

func (t *fakeTransport) run(ft *fakeTurn, text string) {
	defer close(ft.done)
	tick := t.p.tick
	words := strings.Fields(text)
	cmd, arg := "", ""
	if len(words) > 0 {
		cmd = words[0]
	}
	if len(words) > 1 {
		arg = strings.Join(words[1:], " ")
	}
	t.report(Event{Kind: Started, Native: ft.native})
	msg := "m-" + ft.native
	end := func(outcome contract.TurnOutcome, reply string, terr *contract.TurnError) {
		t.write(fakeEntry{Type: "end", Native: ft.native, Outcome: string(outcome), Text: reply, Error: terr})
		t.mu.Lock()
		if t.gone {
			t.mu.Unlock()
			return
		}
		t.turn = nil
		t.mu.Unlock()
		t.report(Event{Kind: Ended, Native: ft.native, Outcome: outcome, Text: reply, Error: terr})
	}
	reply := func(s string) {
		t.write(fakeEntry{Type: "assistant", Native: ft.native, Message: msg, Text: s})
		end(contract.TurnCompleted, s, nil)
	}
	stopped := func(why string, partial string) {
		if why == "dead" {
			return
		}
		if partial != "" {
			t.write(fakeEntry{Type: "assistant", Native: ft.native, Message: msg, Text: partial})
		}
		end(contract.TurnInterrupted, "", nil)
	}
	n := func(def int) int {
		if v, err := strconv.Atoi(strings.Fields(arg + " x")[0]); err == nil {
			return v
		}
		return def
	}
	switch cmd {
	case "PING":
		reply(strings.TrimSpace("PONG " + arg))
	case "SLOW":
		var b strings.Builder
		for i := 0; i < n(40); i++ {
			if why := t.wait(ft, tick); why != "" {
				stopped(why, b.String())
				return
			}
			b.WriteString("slow")
		}
		reply(b.String())
	case "STALL":
		if why := t.wait(ft, time.Duration(n(60))*time.Second); why != "" {
			stopped(why, "")
			return
		}
		reply("ok")
	case "ERR":
		f := strings.Fields(arg)
		code, times := 529, 1
		if len(f) > 0 {
			code, _ = strconv.Atoi(f[0])
		}
		if len(f) > 1 {
			times, _ = strconv.Atoi(f[1])
		}
		for attempt := 1; attempt <= times; attempt++ {
			if attempt > fakeMaxRetries {
				class := contract.ErrorAPI
				if code == 529 {
					class = contract.ErrorOverloaded
				}
				t.write(fakeEntry{Type: "api_error", Native: ft.native, Class: string(class), Status: code})
				end(contract.TurnErrored, "", &contract.TurnError{Class: class, HTTPStatus: code})
				return
			}
			t.report(Event{Kind: Retrying, Native: ft.native, Retry: contract.RetryingData{Attempt: attempt, Max: fakeMaxRetries, DelayMS: int(tick / time.Millisecond), HTTPStatus: code}})
			if why := t.wait(ft, tick); why != "" {
				stopped(why, "")
				return
			}
		}
		reply("RECOVERED")
	case "LIMIT":
		at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		t.report(Event{Kind: RateLimited, Native: ft.native, ResumeAt: &at, RateLimit: contract.RateLimitData{Status: "rejected", Windows: []contract.RateLimitWindow{{Name: "five_hour", ResetsAt: &at}}}})
		t.write(fakeEntry{Type: "api_error", Native: ft.native, Class: string(contract.ErrorUsageLimit), Status: 429})
		end(contract.TurnErrored, "", &contract.TurnError{Class: contract.ErrorUsageLimit, HTTPStatus: 429})
	case "BIG":
		reply(strings.Repeat("x", n(64)<<10))
	default:
		reply("ok")
	}
}

// ---- the record

type fakeCheckpointData struct {
	Offset int64 `json:"offset"`
}

type fakeReader struct {
	path    string
	markers *Markers
	offset  int64
	rescan  *contract.Rescan
	// current is the input whose turn the record is in, at offset.
	current string
}

type fakeChunk struct{ next int64 }

// rebuild reads the record up to the checkpoint for the input it was in.
func (r *fakeReader) rebuild() error {
	if r.offset == 0 {
		return nil
	}
	lines, _, err := r.lines(0)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, l := range lines {
		if l.end > r.offset {
			break
		}
		r.follow(l.e)
	}
	return nil
}

// follow moves current past e, and returns the input e belongs to.
func (r *fakeReader) follow(e fakeEntry) string {
	if e.Type == "user" {
		r.current = ""
		if mk, ok := r.markers.ByNative(e.Native); ok {
			r.current = mk.InputID
		}
	}
	in := r.current
	if e.Type == "end" {
		r.current = ""
	}
	return in
}

type fakeLine struct {
	e   fakeEntry
	end int64
}

func (r *fakeReader) lines(from int64) ([]fakeLine, int64, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, from, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, from, err
	}
	var out []fakeLine
	pos := from
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break // a partial line waits for its newline
		}
		pos += int64(len(line))
		var e fakeEntry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, fakeLine{e: e, end: pos})
		}
	}
	return out, pos, nil
}

func (r *fakeReader) Read(_ context.Context, max int) (Chunk, error) {
	var ch Chunk
	if r.rescan != nil {
		ch.Rescan = r.rescan
		r.offset, r.current = 0, ""
	}
	lines, _, err := r.lines(r.offset)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Chunk{}, err
	}
	cur := r.current
	next, size := r.offset, 0
	for _, l := range lines {
		if size > max/2 && len(ch.Items) > 0 {
			break
		}
		in := r.follow(l.e)
		for _, o := range fakeItems(l.e, in) {
			ch.Items = append(ch.Items, o)
			size += encodedSize(o)
		}
		next = l.end
	}
	r.current = cur // Read does not move; Commit does
	if next != r.offset || ch.Rescan != nil {
		d, _ := json.Marshal(fakeCheckpointData{Offset: next})
		ch.Checkpoint = &contract.Checkpoint{Format: fakeCheckpoint1, Data: d}
		ch.Token = fakeChunk{next: next}
	}
	return ch, nil
}

func fakeItems(e fakeEntry, in string) []contract.Observation {
	stamp := func(o contract.Observation) contract.Observation {
		o.Entry = e.ID
		if in != "" {
			o.InputID, o.TurnID = in, TurnID(in)
		}
		return o
	}
	switch e.Type {
	case "user":
		return []contract.Observation{stamp(contract.NewObservation(contract.KindUserInput, e.ID+":0", contract.OriginRecord, e.Received, contract.TextData{Text: e.Text}))}
	case "assistant":
		text, cut := Truncate(e.Text)
		o := stamp(contract.NewObservation(contract.KindAssistantText, e.ID+":0", contract.OriginRecord, e.Received, contract.AssistantTextData{MessageID: e.Message, Text: text}))
		o.Truncated = cut
		return []contract.Observation{o}
	case "api_error":
		return []contract.Observation{stamp(contract.NewObservation(contract.KindAPIError, e.ID, contract.OriginRecord, e.Received, contract.APIErrorData{Class: contract.ErrorClass(e.Class), HTTPStatus: e.Status}))}
	case "end":
		if in == "" {
			return nil
		}
		text, _ := Truncate(e.Text)
		return []contract.Observation{stamp(contract.NewObservation(contract.KindTurnEnded, in, contract.OriginRecord, e.Received, contract.TurnEndedData{Outcome: contract.TurnOutcome(e.Outcome), Text: text, Error: e.Error}))}
	}
	return nil
}

func (r *fakeReader) Commit(c Chunk) error {
	tok, ok := c.Token.(fakeChunk)
	if !ok {
		return nil
	}
	lines, _, err := r.lines(r.offset)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if r.rescan != nil {
		r.rescan = nil
	}
	for _, l := range lines {
		if l.end > tok.next {
			break
		}
		r.follow(l.e)
	}
	r.offset = tok.next
	return nil
}

func (r *fakeReader) Recover(_ context.Context, m Marker) (contract.Recovered, error) {
	lines, _, err := r.lines(0)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return contract.Recovered{}, err
	}
	found := false
	for _, l := range lines {
		switch {
		case l.e.Type == "user" && l.e.Native == m.Native:
			found = true
		case l.e.Type == "user" && found:
			return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
		case l.e.Type == "end" && found:
			return contract.Recovered{Outcome: contract.RecoveredOutcome(l.e.Outcome)}, nil
		}
	}
	return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
}

func (r *fakeReader) Close() error { return nil }
