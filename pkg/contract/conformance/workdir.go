package conformance

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A Session's own working directory (session_workdir, 1.9): a directory
// beneath the workspace — a git worktree, say — that the Session's harness
// works in rather than the workspace. Its harness runs there, it reopens
// there, its record is read from there, and a load moves its history to the
// same directory beneath the new workspace.

// mkWorkDir makes the working directory dir beneath a's workspace, as the
// Supervisor would before the Session opens, and returns its path.
func (c *check) mkWorkDir(a *agent, dir string) string {
	p := contract.WorkDirPath(a.layout, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		c.stop("setup", "the working directory %s: %v", dir, err)
	}
	return p
}

// openSessionIn opens a fresh Session for a that works in dir, and watches
// it.
func (c *check) openSessionIn(a *agent, dir string) (contract.Session, *host) {
	req := c.openRequest(a, contract.OpenFresh, "", nil)
	req.WorkDir = dir
	s, err := c.f.Adapter.NewSession(req)
	if err != nil {
		c.stop("workdir.open", "NewSession in %s: %v", dir, err)
	}
	return c.openWatch(s, "workdir.open")
}

// workdirMarker is the file workdirTurn reads where the harness works.
const workdirMarker = ".hw-workdir"

// workdirTurn has a Session show where its harness works: a mark of path's
// own, written there, which a TOOL turn reads by a path relative to the
// harness's working directory — a reply too short for the mock model API
// to cut, as a pwd's could be.
func (c *check) workdirTurn(s contract.Session, h *host, path string) {
	mark := "wd-" + randomHex(8)
	if err := os.WriteFile(filepath.Join(path, workdirMarker), []byte(mark+"\n"), 0o644); err != nil {
		c.stop("setup", "marking %s: %v", path, err)
	}
	in := c.send(s, "TOOL cat "+workdirMarker)
	end, ok := h.turnEnded(in)
	switch {
	case !ok:
		c.stop("workdir.runs-there", "no turn_ended for TOOL cat %s", workdirMarker)
	case end.Outcome != contract.TurnCompleted:
		c.stop("workdir.runs-there", "TOOL cat %s ended %s: %+v", workdirMarker, end.Outcome, end)
	case !strings.Contains(end.Text, mark):
		c.fail("workdir.runs-there", "TOOL cat %s answered %q, not %s's mark %s: the harness does not work there", workdirMarker, end.Text, path, mark)
	}
	c.awaitPhase(s, contract.PhaseIdle)
}

// randomHex is n random bytes, hex encoded.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// workdir: Sessions each work in a directory of their own — side by side
// when the adapter runs several at once — and each reopens there, its
// record read from there; one that names no directory works in the
// workspace. A directory that is not one is refused before any harness
// starts, a path that leaves the workspace before NewSession returns, and an
// adapter without session_workdir refuses one.
func workdir(c *check) {
	a := c.newAgent()
	if !c.has(contract.CapSessionWorkDir) {
		req := c.openRequest(a, contract.OpenFresh, "", nil)
		req.WorkDir = "wt/a"
		if _, err := c.f.Adapter.NewSession(req); codeOf(err) != contract.CodeUnsupported {
			c.fail("workdir.unsupported", "a Session's own working directory with no %s: %v, want unsupported", contract.CapSessionWorkDir, err)
		}
		return
	}
	for _, bad := range []string{"/" + strings.TrimPrefix(a.layout.Workspace, "/"), "../out", "wt/../../out", "."} {
		req := c.openRequest(a, contract.OpenFresh, "", nil)
		req.WorkDir = bad
		if _, err := c.f.Adapter.NewSession(req); codeOf(err) != contract.CodeProtocol {
			c.fail("workdir.path", "NewSession with work_dir %q: %v, want protocol", bad, err)
		}
	}
	missing := c.openRequest(a, contract.OpenFresh, "", nil)
	missing.WorkDir = "wt/missing"
	if s, err := c.f.Adapter.NewSession(missing); err != nil {
		c.fail("workdir.missing", "NewSession with a work_dir not made yet: %v; it is checked when the Session opens", err)
	} else {
		ctx, cancel := c.ctx()
		_, err := s.Open(ctx)
		var e *contract.Error
		if !asErr(err, &e) || e.Code != contract.CodeOpenFailed || e.Reason != contract.OpenConfigInvalid {
			c.fail("workdir.missing", "Open in a work_dir that is no directory: %v, want open_failed{config_invalid}", err)
		}
		_, _ = s.Close(ctx, contract.ClosePark, 0)
		cancel()
	}

	type opened struct {
		dir, path string
		s         contract.Session
		h         *host
	}
	open := func(dir string) *opened {
		o := &opened{dir: dir, path: c.mkWorkDir(a, dir)}
		o.s, o.h = c.openSessionIn(a, dir)
		return o
	}
	dirs := []string{"wt/a", "wt/b", ""}
	if n := c.desc.Sessions(); n > 1 {
		// Side by side: every Session open before any turn, as an agent's
		// Sessions are.
		var all []*opened
		for _, dir := range dirs[:min(len(dirs), n)] {
			all = append(all, open(dir))
		}
		for _, o := range all {
			c.workdirTurn(o.s, o.h, o.path)
		}
		for _, o := range all {
			if o.dir != "" {
				c.workdirLife(a, o.dir, o.path, o.s, o.h)
			}
		}
		return
	}
	for _, dir := range dirs {
		o := open(dir)
		c.workdirTurn(o.s, o.h, o.path)
		if dir != "" {
			c.workdirLife(a, o.dir, o.path, o.s, o.h)
			continue
		}
		ctx, cancel := c.ctx()
		if res, err := o.s.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
			c.fail("workdir.close", "Close in the workspace = %+v %v", res, err)
		}
		cancel()
		o.h.halt()
	}
}

// workdirLife takes Session s, open in its working directory dir at path,
// through a turn and a close; then reads its record naming dir, with no
// harness, and reopens it there, where it goes on with its conversation and
// still works in path. It leaves the reopened Session closed.
func (c *check) workdirLife(a *agent, dir, path string, s contract.Session, h *host) {
	in := c.send(s, "PING 41")
	if end, ok := h.turnEnded(in); !ok || !strings.Contains(end.Text, "PONG 41") {
		c.fail("workdir.turn", "a turn in %s: %+v", dir, end)
	}
	c.awaitPhase(s, contract.PhaseIdle)
	time.Sleep(200 * time.Millisecond)
	ctx, cancel := c.ctx()
	defer cancel()
	if res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.fail("workdir.close", "Close in %s = %+v %v", dir, res, err)
	}
	h.halt()

	// The record is read from the Session's directory, with no harness.
	said := ""
	items := c.readRecord(a, h.id, dir)
	for _, it := range items {
		var d contract.AssistantTextData
		if it.Kind == contract.KindAssistantText && it.Decode(&d) == nil {
			said += d.Text + "\n"
		}
	}
	if !strings.Contains(said, "PONG 41") {
		c.fail("workdir.record", "the record of the Session in %s, read naming it, holds %d items and not its reply PONG 41", dir, len(items))
	}

	// It reopens there, and goes on.
	req := c.openRequest(a, contract.OpenReopen, h.id, h.checkpoint())
	req.WorkDir = dir
	s2, err := c.f.Adapter.NewSession(req)
	if err != nil {
		c.stop("workdir.reopen", "NewSession reopening in %s: %v", dir, err)
	}
	s2, h2 := c.openWatch(s2, "workdir.reopen")
	if h2.id != h.id {
		c.fail("workdir.reopen", "reopened in %s as %q, want %q", dir, h2.id, h.id)
	}
	in = c.send(s2, "PING 42")
	if end, ok := h2.turnEnded(in); !ok || !strings.Contains(end.Text, "PONG 42") {
		c.fail("workdir.reopen", "a turn after the reopen in %s: %+v", dir, end)
	}
	if text, ok := c.heard(s2); ok && !strings.Contains(text, "PING 41") {
		c.fail("workdir.reopen", "the reopened Session in %s does not carry its conversation: the model was not given PING 41", dir)
	}
	c.awaitPhase(s2, contract.PhaseIdle)
	c.workdirTurn(s2, h2, path)
	if res, err := s2.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.fail("workdir.close", "Close after the reopen in %s = %+v %v", dir, res, err)
	}
	h2.halt()
}

// readRecord reads the record of Session id, which works in dir, to its end
// with no harness, acknowledging every batch, and returns its items.
func (c *check) readRecord(a *agent, id, dir string) []contract.Observation {
	ctx, cancel := c.ctx()
	defer cancel()
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: id, OpenConfig: a.result.OpenConfig, Layout: a.layout, WorkDir: dir})
	if err != nil {
		c.stop("workdir.record", "OpenRecord of the Session in %s: %v", dir, err)
	}
	defer func() { _ = r.Close() }()
	var items []contract.Observation
	deadline := time.Now().Add(c.f.timeout())
	for {
		b, err := r.Observe(ctx, 200*time.Millisecond, contract.MaxObserveBytes)
		if err != nil {
			c.stop("workdir.record", "reading the record of the Session in %s: %v", dir, err)
		}
		items = append(items, b.Items...)
		if b.NeedsAck() {
			if err := r.Ack(b.BatchID); err != nil {
				c.stop("workdir.record", "Ack(%s): %v", b.BatchID, err)
			}
		}
		if b.EndOfRecord {
			return items
		}
		if time.Now().After(deadline) {
			c.stop("workdir.record", "the record of the Session in %s never ended", dir)
		}
	}
}

// workdirLoad: a Session that worked in a directory of its own continues in a
// new environment — in the same directory beneath the new workspace — once
// the load names that directory (LoadSource.WorkDirs).
func workdirLoad(c *check) {
	if !c.has(contract.CapSessionLoad) || !c.has(contract.CapSessionWorkDir) {
		c.t.Logf("no session_load, or no session_workdir: skipped")
		return
	}
	const dir = "wt/loaded"
	src := c.newAgent()
	c.mkWorkDir(src, dir)
	s, h := c.openSessionIn(src, dir)
	in := c.send(s, "PING 31")
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
	saved.Source.WorkDirs = []string{dir}
	dst := c.loadAgent(saved, true, "load.provision")
	c.mkWorkDir(dst, dir)
	// continues has the loaded Session say, once reopened, that it works in
	// the new directory.
	c.continues(dst, SavedSession{Saved: saved, SessionID: h.id, Remembers: []string{"PING 31", "PONG 31"}, WorkDir: dir})
}
