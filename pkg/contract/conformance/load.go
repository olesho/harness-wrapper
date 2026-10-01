package conformance

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Saved is what an archive keeps of an agent's Session: every file beneath
// its Provision result's history roots, less its secret paths, and the source
// a request that loads it names.
type Saved struct {
	Source contract.LoadSource
	Files  []SavedFile
}

// SavedFile is one saved file, named the way the source's roots named it.
type SavedFile struct {
	At      contract.RootPath
	Mode    fs.FileMode
	Content []byte
}

// Save reads an agent's history out of its layout the way a Supervisor does
// for an archive: the regular files beneath r's history roots, outside its
// secret paths, none reached through a symbolic link. The harness has stopped.
func Save(d contract.Descriptor, format int, l contract.Layout, r contract.ProvisionResult) (Saved, error) {
	workspace, err := filepath.EvalSymlinks(l.Workspace)
	if err != nil {
		return Saved{}, err
	}
	s := Saved{Source: contract.LoadSource{Format: format, Harness: d.Harness, Layout: l, Workspace: workspace}}
	for _, hr := range r.HistoryRoots {
		base := l.Path(hr.Root)
		top := filepath.Join(base, filepath.FromSlash(hr.Path))
		err := filepath.WalkDir(top, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && p == top {
					return fs.SkipAll // a history root the harness never wrote
				}
				return err
			}
			if !e.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			at := contract.RootPath{Root: hr.Root, Path: filepath.ToSlash(rel)}
			if !r.Archived(at) {
				return nil
			}
			fi, err := e.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			s.Files = append(s.Files, SavedFile{At: at, Mode: fi.Mode().Perm(), Content: b})
			return nil
		})
		if err != nil {
			return Saved{}, err
		}
	}
	return s, nil
}

// Restore writes saved files under a layout the way a Supervisor does before
// the harness is first opened there: each at the place r's relocations send
// it, which must be beneath one of r's history roots and outside its secret
// paths, and never through a symbolic link.
func Restore(l contract.Layout, r contract.ProvisionResult, s Saved) error {
	if err := r.Validate(); err != nil {
		return err
	}
	seen := map[contract.RootPath]bool{}
	for _, f := range s.Files {
		to := contract.Relocate(r.HistoryRelocations, f.At)
		if !r.Archived(to) {
			return fmt.Errorf("saved file %s lands at %s, which is not history here", f.At, to)
		}
		if seen[to] {
			return fmt.Errorf("two saved files land at %s", to)
		}
		seen[to] = true
		root, err := os.OpenRoot(l.Path(to.Root))
		if err != nil {
			return err
		}
		if dir := filepath.Dir(filepath.FromSlash(to.Path)); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				_ = root.Close()
				return err
			}
		}
		err = root.WriteFile(filepath.FromSlash(to.Path), f.Content, f.Mode)
		_ = root.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// save closes nothing: it reads a's history, whose harness has stopped, and
// then removes a's roots, so that nothing the loaded Session does can reach
// the source.
func (c *check) save(a *agent, rule string) Saved {
	if c.desc.Load == nil || len(c.desc.Load.Formats) == 0 {
		c.stop("describe.load", "session_load, and no archive format named")
	}
	saved, err := Save(c.desc, c.desc.Load.Formats[0], a.layout, a.result)
	if err != nil {
		c.stop(rule, "saving the history: %v", err)
	}
	if len(saved.Files) == 0 {
		c.stop(rule, "the history roots hold no file after a turn")
	}
	if err := os.RemoveAll(a.base); err != nil {
		c.stop(rule, "removing the source: %v", err)
	}
	return saved
}

// loadAgent provisions a new agent, in roots of its own, that loads saved;
// with restore, it writes the saved files there.
func (c *check) loadAgent(saved Saved, restore bool, rule string) *agent {
	a := c.roots()
	src := saved.Source
	req := c.request(a, &src)
	res, err := c.f.Adapter.Provision(req)
	again, err2 := c.f.Adapter.Provision(req)
	if err != nil || err2 != nil {
		c.stop(rule, "Provision of an agent that loads: %v / %v", err, err2)
	}
	if !reflect.DeepEqual(res, again) {
		c.fail("load.provision", "the same request that loads rendered two different results")
	}
	if c.f.Provisioned != nil {
		c.f.Provisioned(c.t, a.layout, &res)
	}
	// The history first, then the configuration rendered for the new agent:
	// what the new agent is given wins where both name a file.
	if restore {
		if err := Restore(a.layout, res, saved); err != nil {
			c.stop("load.relocations", "restoring the history: %v", err)
		}
	}
	if err := Apply(a.layout, res); err != nil {
		c.stop("load.provision", "applying the result: %v", err)
	}
	a.result = res
	return a
}

// seed reads the loaded Session's record to its end with no harness running,
// as a Supervisor does to find where new history will begin: the items it
// holds, and the checkpoint after the last.
func (c *check) seed(a *agent, sessionID, rule string) ([]contract.Observation, *contract.Checkpoint) {
	ctx, cancel := c.ctx()
	defer cancel()
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: sessionID, OpenConfig: a.result.OpenConfig, Layout: a.layout})
	if err != nil {
		c.stop(rule, "OpenRecord of the loaded Session: %v", err)
	}
	defer func() { _ = r.Close() }()
	var items []contract.Observation
	var cp *contract.Checkpoint
	deadline := time.Now().Add(c.f.timeout())
	for {
		b, err := r.Observe(ctx, 200*time.Millisecond, contract.MaxObserveBytes)
		if err != nil {
			c.stop(rule, "reading the loaded record: %v", err)
		}
		items = append(items, b.Items...)
		if b.Checkpoint != nil {
			cp = b.Checkpoint
		}
		if b.NeedsAck() {
			if err := r.Ack(b.BatchID); err != nil {
				c.stop(rule, "Ack(%s): %v", b.BatchID, err)
			}
		}
		if b.EndOfRecord {
			return items, cp
		}
		if time.Now().After(deadline) {
			c.stop(rule, "the loaded record never ended")
		}
	}
}

// heard is what the Session's harness last sent its model; "" and false when
// the fixture cannot tell.
func (c *check) heard(s contract.Session) (string, bool) {
	if c.f.Heard == nil {
		return "", false
	}
	return c.f.Heard(c.t, s), true
}

// load: a Session saved in one environment continues in another — a fresh
// layout, with no access to the source — from a seeded checkpoint, delivering
// only what is new, and again after a reopen.
func load(c *check) {
	if !c.has(contract.CapSessionLoad) {
		c.t.Logf("no session_load: skipped")
		return
	}
	src := c.newAgent()
	s, h := c.openSession(src)
	in := c.send(s, "PING 21")
	if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted {
		c.stop("load.record", "the turn to save: %+v", end)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	if res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.stop("load.record", "Close = %+v %v, want drained", res, err)
	}
	h.halt()
	saved := c.save(src, "load.record")
	dst := c.loadAgent(saved, true, "load.provision")
	if dst.layout.Workspace == src.layout.Workspace {
		c.stop("setup", "the new agent has the source's workspace")
	}
	c.continues(dst, SavedSession{Saved: saved, SessionID: h.id, Remembers: []string{"PING 21", "PONG 21"}})
}

// continues checks that a saved Session, restored into a's roots, goes on
// there: its record is where the harness looks and reads to its end with no
// harness; it opens under its saved id, from the checkpoint that read gave,
// delivering only what is new; its model is given the saved conversation;
// and it reopens.
func (c *check) continues(a *agent, saved SavedSession) {
	ctx, cancel := c.ctx()
	defer cancel()
	// The seed: the record is where the harness looks, and it is the saved
	// one. Nothing of it is delivered again once the Session opens.
	items, cp := c.seed(a, saved.SessionID, "load.record")
	said, seeded := "", map[string]bool{}
	for _, o := range items {
		seeded[o.ID] = true
		var d contract.AssistantTextData
		if o.Kind == contract.KindAssistantText && o.Decode(&d) == nil {
			said += d.Text + "\n"
		}
	}
	if len(items) == 0 {
		c.stop("load.record", "the loaded record, read with no harness, is empty: the history is not where the harness looks")
	}
	for _, want := range saved.Remembers {
		if strings.HasPrefix(want, "PONG") && !strings.Contains(said, want) {
			c.stop("load.record", "the loaded record, read with no harness, holds %d items and not the saved reply %q", len(items), want)
		}
	}
	if cp == nil {
		c.stop("load.record", "the loaded record, read to its end, gave no checkpoint")
	}

	open := func(cp *contract.Checkpoint) (contract.Session, *host) {
		req := c.openRequest(a, contract.OpenReopen, saved.SessionID, cp)
		req.Loaded = true
		s, err := c.f.Adapter.NewSession(req)
		if err != nil {
			c.stop("load.open", "NewSession of a loaded Session: %v", err)
		}
		s, hh := c.openWatch(s, "load.open")
		if hh.id != saved.SessionID {
			c.fail("load.open", "the loaded Session opened as %q, want the saved %q", hh.id, saved.SessionID)
		}
		return s, hh
	}
	turn := func(s contract.Session, hh *host, n int, remembers ...string) {
		in := c.send(s, fmt.Sprintf("PING %d", n))
		if end, ok := hh.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted || !strings.Contains(end.Text, fmt.Sprintf("PONG %d", n)) {
			c.fail("load.continues", "a turn of the loaded Session: %+v", end)
		}
		if o, ok := hh.awaitKind(contract.KindAssistantText, in); !ok || o.Origin != contract.OriginRecord {
			c.fail("load.continues", "the new turn's reply is not delivered from the record")
		}
		if text, ok := c.heard(s); ok {
			for _, want := range remembers {
				if !strings.Contains(text, want) {
					c.fail("load.history", "the model was not given %q: the loaded Session does not carry the saved conversation", want)
				}
			}
		}
		c.awaitPhase(s, contract.PhaseIdle)
		time.Sleep(200 * time.Millisecond)
	}

	s2, h2 := open(cp)
	turn(s2, h2, 22, saved.Remembers...)
	for id, o := range h2.ids() {
		if seeded[id] && o.Origin == contract.OriginRecord {
			c.fail("load.no-redelivery", "saved record item %s, behind the seeded checkpoint, was delivered by the loaded Session", id)
		}
	}

	// It reopens as any Session does, and still remembers.
	if res, err := s2.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.fail("load.reopen", "Close of the loaded Session = %+v %v, want drained", res, err)
	}
	h2.halt()
	acked := h2.ids()
	s3, h3 := open(h2.checkpoint())
	turn(s3, h3, 23, append(append([]string(nil), saved.Remembers...), "PONG 22")...)
	for id, o := range h3.ids() {
		if (seeded[id] || hasID(acked, id)) && o.Origin == contract.OriginRecord {
			c.fail("load.reopen", "record item %s, acknowledged before, delivered again after the loaded Session reopened", id)
		}
	}
}

// SavedSession is a Session saved earlier, kept as files: what a load of it
// needs, and what the loaded Session must still know. It is how a harness
// version's saved Sessions are kept as fixtures, which a later version of the
// adapter must load before it may name that version a source (LoadSaved).
type SavedSession struct {
	Saved
	// SessionID is the saved Session's id.
	SessionID string
	// Remembers are texts of the saved conversation — its inputs, and its
	// replies, which begin "PONG" — that a model request of the loaded
	// Session carries.
	Remembers []string
}

// savedIndex is a SavedSession's saved.json: everything but the files'
// contents, which lie beside it under files/<root>/<path>.
type savedIndex struct {
	Source    contract.LoadSource `json:"source"`
	SessionID string              `json:"session_id"`
	Remembers []string            `json:"remembers"`
	Files     []savedEntry        `json:"files"`
}

type savedEntry struct {
	Root contract.Root `json:"root"`
	Path string        `json:"path"`
	Mode string        `json:"mode"`
}

// WriteSavedSession writes s beneath dir: saved.json, and each file under
// files/<root>/<path>.
func WriteSavedSession(dir string, s SavedSession) error {
	idx := savedIndex{Source: s.Source, SessionID: s.SessionID, Remembers: s.Remembers}
	for _, f := range s.Files {
		if err := contract.CheckRelPath(f.At.Path); err != nil {
			return err
		}
		p := filepath.Join(dir, "files", string(f.At.Root), filepath.FromSlash(f.At.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.Content, 0o644); err != nil { //nolint:gosec // a fixture in a source tree
			return err
		}
		idx.Files = append(idx.Files, savedEntry{Root: f.At.Root, Path: f.At.Path, Mode: fmt.Sprintf("%04o", f.Mode.Perm())})
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "saved.json"), append(b, '\n'), 0o644) //nolint:gosec // a fixture in a source tree
}

// ReadSavedSession reads what WriteSavedSession wrote beneath dir.
func ReadSavedSession(dir string) (SavedSession, error) {
	b, err := os.ReadFile(filepath.Join(dir, "saved.json"))
	if err != nil {
		return SavedSession{}, err
	}
	var idx savedIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return SavedSession{}, fmt.Errorf("%s: %w", filepath.Join(dir, "saved.json"), err)
	}
	s := SavedSession{Saved: Saved{Source: idx.Source}, SessionID: idx.SessionID, Remembers: idx.Remembers}
	for _, e := range idx.Files {
		if err := contract.CheckRelPath(e.Path); err != nil {
			return SavedSession{}, err
		}
		mode, err := strconv.ParseUint(e.Mode, 8, 32)
		if err != nil {
			return SavedSession{}, fmt.Errorf("%s: mode %q", e.Path, e.Mode)
		}
		content, err := os.ReadFile(filepath.Join(dir, "files", string(e.Root), filepath.FromSlash(e.Path)))
		if err != nil {
			return SavedSession{}, err
		}
		s.Files = append(s.Files, SavedFile{At: contract.RootPath{Root: e.Root, Path: e.Path}, Mode: fs.FileMode(mode), Content: content})
	}
	return s, nil
}

// RecordSavedSession makes a saved Session with the fixture's adapter, for
// LoadSaved to load later: in roots beneath base — a path the saved files
// will name, so one that says nothing of the machine — it opens a Session,
// has two turns with it, lets prepare give it whatever else the harness
// keeps of a Session, parks it and saves it. base must not exist; it is
// removed again.
func RecordSavedSession(t T, f Fixture, base string, prepare func(contract.Session)) (saved SavedSession, ok bool) {
	t.Helper()
	c := &check{t: t, f: f, desc: f.Adapter.Describe(), sent: map[string]bool{}, base: base}
	defer c.cleanup()
	defer func() {
		if r := recover(); r != nil && r != errStop {
			c.fail("scenario", "panic: %v", r)
		}
	}()
	if _, err := os.Stat(base); err == nil {
		c.stop("setup", "%s exists", base)
	}
	defer func() { _ = os.RemoveAll(base) }()
	a := c.newAgent()
	s, h := c.openSession(a)
	remembers := []string{"PING 1", "PONG 1", "TOOL echo saved"}
	for _, text := range []string{"PING 1", "TOOL echo saved"} {
		in := c.send(s, text)
		if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted {
			c.stop("load.record", "the turn %q to save: %+v", text, end)
		}
		c.awaitPhase(s, contract.PhaseIdle)
	}
	if prepare != nil {
		prepare(s)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	if res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.stop("load.record", "Close = %+v %v, want drained", res, err)
	}
	h.halt()
	return SavedSession{Saved: c.save(a, "load.record"), SessionID: h.id, Remembers: remembers}, true
}

// LoadSaved loads a Session saved earlier — by another version of the
// harness, say — into a fresh environment and runs the load scenario's checks
// on it: the record read with no harness, the strict open under the saved id,
// only new items, the saved conversation given to the model, and a reopen.
func LoadSaved(t T, f Fixture, saved SavedSession) {
	t.Helper()
	c := &check{t: t, f: f, desc: f.Adapter.Describe(), sent: map[string]bool{}}
	defer c.cleanup()
	defer func() {
		if r := recover(); r != nil && r != errStop {
			c.fail("scenario", "panic: %v", r)
		}
	}()
	c.continues(c.loadAgent(saved.Saved, true, "load.provision"), saved)
}

func hasID(m map[string]contract.Observation, id string) bool {
	_, ok := m[id]
	return ok
}

// loadMissing: a loaded Session whose record is not where the harness looks
// does not open: never as a fresh conversation.
func loadMissing(c *check) {
	if !c.has(contract.CapSessionLoad) {
		c.t.Logf("no session_load: skipped")
		return
	}
	src := c.newAgent()
	s, h := c.openSession(src)
	in := c.send(s, "PING 24")
	if _, ok := h.turnEnded(in); !ok {
		c.stop("load.strict", "no turn_ended")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	_, _ = s.Close(ctx, contract.ClosePark, contract.DefaultDrain)
	h.halt()
	saved := c.save(src, "load.strict")
	dst := c.loadAgent(saved, false, "load.provision") // nothing restored

	req := c.openRequest(dst, contract.OpenFresh, "", nil)
	req.Loaded = true
	if _, err := c.f.Adapter.NewSession(req); codeOf(err) != contract.CodeProtocol {
		c.fail("load.strict", "a fresh open of a loaded Session: %v, want protocol", err)
	}
	req = c.openRequest(dst, contract.OpenReopen, h.id, nil)
	req.Loaded = true
	s2, err := c.f.Adapter.NewSession(req)
	if err != nil {
		c.stop("load.strict", "NewSession: %v", err)
	}
	res, err := s2.Open(ctx)
	defer func() { _, _ = s2.Close(ctx, contract.ClosePark, 0) }()
	var e *contract.Error
	switch {
	case err == nil:
		c.fail("load.strict", "a loaded Session with no record opened, as %q: a fresh conversation in place of the saved one", res.SessionID)
	case !asErr(err, &e) || e.Code != contract.CodeOpenFailed || e.Reason != contract.OpenSessionNotFound:
		c.fail("load.strict", "opening a loaded Session with no record: %v, want open_failed{session_not_found}", err)
	}
}

// loadRefused: Provision refuses a source the Descriptor does not say it
// loads, naming what it refused.
func loadRefused(c *check) {
	a := c.roots()
	src := contract.LoadSource{Format: 2, Harness: c.desc.Harness, Layout: a.layout, Workspace: a.layout.Workspace}
	refused := func(what, field string, src contract.LoadSource) {
		_, err := c.provision(a, &src)
		var e *contract.Error
		if !asErr(err, &e) || e.Code != contract.CodeUnsupported || e.Field != field {
			c.fail("load.unsupported", "loading %s: %v, want unsupported naming %s", what, err, field)
		}
	}
	if !c.has(contract.CapSessionLoad) {
		refused("with no session_load", "load", src)
		return
	}
	if c.desc.Load == nil || len(c.desc.Load.Formats) == 0 {
		c.stop("describe.load", "session_load, and no archive format named")
	}
	src.Format = c.desc.Load.Formats[0]
	if _, err := c.provision(a, &src); err != nil {
		c.fail("load.unsupported", "loading the adapter's own version: %v", err)
	}
	other := src
	other.Harness.Version = "0.0.0-never-released"
	refused("a version the Descriptor does not name", "load.harness.version", other)
	other = src
	other.Format = 1 << 20
	refused("an archive format the Descriptor does not name", "load.format", other)
	other = src
	other.Harness.Name = "not-" + src.Harness.Name
	refused("another harness's Session", "load.harness.name", other)
}
