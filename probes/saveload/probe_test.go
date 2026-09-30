// Package saveload is the feasibility probe of Save and Load: whether a
// Session of each pinned harness, saved as files, continues in a fresh
// environment. It is Step 1 of "Save and load an agent's conversation: Claude
// Code and Codex"
// (https://coplan.olehluchkiv.com/d/save-and-load-an-agents-conversation-cla):
// a gate before any archive API is built. It ships nothing; the probe and what
// it found (FINDINGS.md) are the whole of it.
//
// For each harness it runs an agent the way agentd does — the Harness Adapter,
// the pinned binary, a layout of five roots — takes two turns that carry a
// nonce and a tool result, stops the harness, copies the candidate state to a
// standalone archive, takes the source away, and restores the archive into an
// environment whose every root differs. There it reads the restored record to
// its end with the adapter's record handle, reopens the Session from that
// checkpoint, and checks what the model is sent, what the record reports, and
// what a second stop and reopen keep. A negative control does the same with
// nothing restored.
//
// The tests skip without the binaries they name:
//
//	HW_REAL_CLAUDE=… HW_REAL_CODEX=… go test -count=1 -v ./probes/saveload/
//
// README.md has the rest.
package saveload

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// What the probe reads from its environment.
const (
	// envEvidence names a directory the run's evidence is written to.
	envEvidence = "HW_SAVELOAD_EVIDENCE"
	// envWork names a directory the run's environments are built in and left
	// in, for a look at them afterwards; a temporary one otherwise.
	envWork = "HW_SAVELOAD_WORK"
)

type mode string

const (
	// modeMock: the real harness binary against the mock model API, which
	// keeps every request: what a resumed Session sends is read, not inferred.
	modeMock mode = "mock"
	// modeLive: the real harness binary against its real model API, with a
	// credential the caller names. The model's recall is the evidence.
	modeLive mode = "live"
)

// nonces are the run's random words. A enters the conversation in the first
// turn and is written nowhere else. T is what the second turn's tool prints;
// it also lands in the workspace's note. M is in the seeded memory file.
type nonces struct{ A, T, M string }

// script is the run's prompts. None of the three sent after the restore names
// a nonce or summarizes what came before.
type script struct {
	// remember carries A; its reply holds A.
	remember string
	// tool runs a command that prints T and leaves it in noteFile.
	tool string
	// delegate hands a task to a subagent, whose reply is delegateReply; empty
	// when the run has no such turn.
	delegate, delegateReply string
	// recall is the first prompt after the restore; recallReply is what its
	// reply holds when the conversation continued.
	recall, recallReply string
	// again is the first prompt after the second reopen, and againReply what
	// its reply holds.
	again, againReply string
	// where runs a command that prints the working directory and noteFile.
	where string
}

// noteFile is the workspace file the source's tool turn writes: the workspace
// smoke check.
const noteFile = "saveload-note.txt"

// item is one item of the conversation a model request carries.
type item struct {
	Role string `json:"role"`
	// Type is text, tool_use or tool_result.
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
}

// variant is one inventory of state to save: a candidate for what a Load
// needs.
type variant struct {
	Name string `json:"name"`
	Note string `json:"note"`
	// holds reports whether a file of the source belongs to it.
	holds func(root contract.Root, path string) bool
}

// harness is what names a harness in the probe. Everything else is the
// Harness Adapter Interface, the same for both.
type harness interface {
	// Name is the harness's registered name.
	Name() string
	// BinaryEnv names the environment variable that names the real binary.
	BinaryEnv() string
	// Version is the version a binary's --version output names.
	Version(out string) string
	// LiveReady reports whether a live credential was named, and how to name
	// one when not.
	LiveReady() (bool, string)
	// Distribution lays the harness distribution out under root.
	Distribution(p *probe, root string)
	// Spec is the agent's spec, with memory's files seeded.
	Spec(p *probe, memory []contract.MemoryFile) contract.AgentSpec
	// Adjust fits a provisioned result to the run before it is applied: in
	// mock mode it points the harness at the mock model API; in a live one it
	// adds whatever the run's credential needs beyond a credential file, and
	// says so in note.
	Adjust(p *probe, r *contract.ProvisionResult) (note string, err error)
	// Credential stages the environment's credential: a placeholder that
	// names label in mock mode, the live one otherwise.
	Credential(p *probe, l contract.Layout, label string) *contract.CredentialFile
	// Script is the run's prompts.
	Script(m mode, n nonces) script
	// FreshID is the id a fresh open asks for: "" lets the harness choose.
	FreshID() string
	// Outside names the directories the harness writes outside the
	// environment's roots.
	Outside(e *environment) []string
	// Subagents are the saved files that are a subagent's own record.
	Subagents(entries []entry) []entry
	// Record is the path of the Session's native record in the environment.
	Record(e *environment, sessionID string) (string, error)
	// Variants are the inventories to try, the candidate recipe first.
	Variants(src *environment, sessionID string) []variant
	// Relocations are the path rules that fit a saved Session to layout dst.
	Relocations(m manifest, dst contract.Layout) []relocation
	// Conversation is the conversation a model request carries.
	Conversation(body []byte) ([]item, error)
	// Resume resumes the Session by its native id with the harness alone, no
	// adapter between: nil only when the harness resumed it, and errNoHistory
	// when it said it has no such Session.
	Resume(e *environment, sessionID string) (detail string, err error)
	// SetAux gives the stopped Session the native state beside its record
	// that a Save must keep, and returns it; nil for a harness with none.
	SetAux(e *environment, sessionID string) (map[string]string, error)
	// Aux reads that state back.
	Aux(e *environment, sessionID string) (map[string]string, error)
}

// errNoHistory is a harness's refusal to resume a Session it has no record
// of: the refusal the negative control wants, and no other failure.
var errNoHistory = errors.New("the harness has no record of the session")

// probe is one run: a harness, in a mode.
type probe struct {
	t       *testing.T
	h       harness
	mode    mode
	adapter contract.Adapter
	desc    contract.Descriptor
	bin     string          // the real harness binary
	work    string          // where the run's environments live
	mock    *mockapi.Server // nil in live mode
	wait    time.Duration   // how long a turn may take
	nonce   nonces
	script  script
	rep     *report
}

func randomWord(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// start sets a run up, or skips: without the binary, or — live — without a
// credential. A skipped live run is pending, never passed.
func start(t *testing.T, h harness, m mode) *probe {
	t.Helper()
	bin := os.Getenv(h.BinaryEnv())
	if bin == "" {
		t.Skipf("%s does not name a %s binary", h.BinaryEnv(), h.Name())
	}
	if m == modeLive {
		if ok, how := h.LiveReady(); !ok {
			t.Skipf("pending: no live credential for %s (%s)", h.Name(), how)
		}
	}
	out, err := exec.Command(bin, "--version").Output() //nolint:gosec // the binary the caller named
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	pin, _ := versions.Pinned(h.Name())
	if got := h.Version(string(out)); got != pin {
		t.Fatalf("%s is %s %q; the probe is for the pinned %s", bin, h.Name(), strings.TrimSpace(string(out)), pin)
	}
	a, ok := contract.Lookup(h.Name())
	if !ok {
		t.Fatalf("no adapter registered for %q", h.Name())
	}
	p := &probe{t: t, h: h, mode: m, adapter: a, desc: a.Describe(), bin: bin, wait: time.Minute}
	p.work = t.TempDir()
	if dir := os.Getenv(envWork); dir != "" {
		p.work = filepath.Join(dir, h.Name()+"-"+string(m))
		restoreAccess(p.work)
		if err := os.RemoveAll(p.work); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(p.work, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A run always leaves its directories removable.
	t.Cleanup(func() { restoreAccess(p.work) })
	p.nonce = nonces{A: randomWord("a"), T: randomWord("t"), M: randomWord("m")}
	p.script = h.Script(m, p.nonce)
	if m == modeMock {
		p.mock = mockapi.Start()
		p.mock.KeepBodies = true
		t.Cleanup(p.mock.Close)
	} else {
		p.wait = 4 * time.Minute
	}
	p.rep = newReport(p, strings.TrimSpace(string(out)))
	return p
}

// restoreAccess makes every directory under dir traversable again: a source
// taken away is left without permissions.
func restoreAccess(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			_ = os.Chmod(filepath.Join(dir, e.Name()), 0o700) //nolint:gosec // the probe's own directories
		}
	}
}

// requests are the mock's requests from the n-th on; nil in live mode.
func (p *probe) requests(n int) []mockapi.Request {
	if p.mock == nil {
		return nil
	}
	all := p.mock.Requests()
	if n > len(all) {
		n = len(all)
	}
	return all[n:]
}

func (p *probe) requestCount() int {
	if p.mock == nil {
		return 0
	}
	return len(p.mock.Requests())
}

// request is the first model request of reqs whose conversation ends with the
// user saying prompt: what the harness sent for that input.
func (p *probe) request(reqs []mockapi.Request, prompt string) (n int, conv []item, err error) {
	for _, r := range reqs {
		conv, cerr := p.h.Conversation(r.Body)
		if cerr != nil {
			continue
		}
		for i := len(conv) - 1; i >= 0; i-- {
			if conv[i].Role != "user" || conv[i].Type != "text" {
				continue
			}
			if strings.TrimSpace(conv[i].Text) == prompt {
				return r.N, conv, nil
			}
			if !synthetic(conv[i].Text) {
				break
			}
		}
	}
	return 0, nil, fmt.Errorf("none of %d model requests ends with the prompt %q", len(reqs), prompt)
}

// synthetic reports whether a user text is the harness's own — a reminder or
// context in tags — and not something the user said.
func synthetic(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "<")
}

// carries reports whether a conversation carries, in order, the source's two
// turns — the prompt with A, a reply with A, the tool call and its result
// with T, the reply after it — then every later exchange given, and ends with
// the last prompt. It returns what is missing.
func (p *probe) carries(conv []item, last string, between ...[2]string) []string {
	type want struct {
		what string
		ok   func(item) bool
	}
	text := func(role, needle string) func(item) bool {
		return func(it item) bool { return it.Role == role && it.Type == "text" && strings.Contains(it.Text, needle) }
	}
	wants := []want{
		{"the first prompt", text("user", p.script.remember)},
		{"the first reply, with nonce A", text("assistant", p.nonce.A)},
		{"the tool call, with nonce T", func(it item) bool { return it.Type == "tool_use" && strings.Contains(it.Text, p.nonce.T) }},
		{"the tool result, with nonce T", func(it item) bool { return it.Type == "tool_result" && strings.Contains(it.Text, p.nonce.T) }},
		{"the reply after the tool, with nonce T", text("assistant", p.nonce.T)},
	}
	for _, ex := range between {
		wants = append(wants, want{"the prompt " + ex[0], text("user", ex[0])}, want{"its reply " + ex[1], text("assistant", ex[1])})
	}
	wants = append(wants, want{"the prompt " + last + ", last", text("user", last)})
	var missing []string
	i := 0
	for _, w := range wants {
		found := false
		for ; i < len(conv); i++ {
			if w.ok(conv[i]) {
				found = true
				i++
				break
			}
		}
		if !found {
			missing = append(missing, w.what)
		}
	}
	return missing
}

// mentions reports whether any item of a conversation holds a nonce.
func (p *probe) mentions(conv []item) bool {
	for _, it := range conv {
		if strings.Contains(it.Text, p.nonce.A) || strings.Contains(it.Text, p.nonce.T) {
			return true
		}
	}
	return false
}

// offset is the record offset a checkpoint covers: both profiles' checkpoints
// are the transcript follower's. The probe reads it only to show where a
// checkpoint stands; nothing is decided from it.
func offset(cp *contract.Checkpoint) (transcript.Checkpoint, bool) {
	var c transcript.Checkpoint
	if cp == nil || json.Unmarshal(cp.Data, &c) != nil {
		return c, false
	}
	return c, true
}

// ---- the run

func run(t *testing.T, h harness, m mode) {
	p := start(t, h, m)
	defer p.rep.write()
	var sv saved
	if dir := os.Getenv(envImport); dir != "" {
		sv = p.imported(dir)
	} else {
		sv = p.save()
		if dir := os.Getenv(envExport); dir != "" {
			p.export(dir, sv)
		}
	}
	candidate := sv.variants[0]
	ld := p.load(candidate, sv.archives[candidate.Name], "restored/"+candidate.Name, true, sv.cp)
	p.continued(ld, sv)
	if m == modeMock {
		// What is the least a Load needs, and which file carries what: the
		// mock answers that; a live run need not pay for it again.
		for _, v := range sv.variants[1:] {
			p.variant(v, sv.archives[v.Name], sv)
		}
	}
	p.negative(sv.sessionID)
	if sv.gone.kept != "" {
		p.untouched(sv.gone)
	}
	if p.rep.failed() {
		t.Errorf("the gate's required checks did not all pass:\n%s", p.rep.clean(p.rep.table()))
	}
}

// goneSource is a source taken away: where it was, where it is kept out of
// reach, and what it held.
type goneSource struct {
	was, kept, digest string
	files             int
}

// saved is what a save leaves: the Session's native id, the source's last
// checkpoint, the native state beside the record, each variant's archive, and
// the source, gone.
type saved struct {
	sessionID string
	cp        *contract.Checkpoint
	aux       map[string]string
	variants  []variant
	archives  map[string]string
	gone      goneSource
}

// save runs the source agent, stops it, saves every variant's archive and
// takes the source away.
func (p *probe) save() saved {
	t := p.t
	src := p.newEnvironment("source", filepath.Join(p.work, "source"), sourceDirs)
	memory := []contract.MemoryFile{{Path: "notes.md", Content: "saveload probe memory " + p.nonce.M + "\n"}}
	src.provision(p.h.Spec(p, memory))
	s, err := src.open(contract.OpenFresh, p.h.FreshID(), nil)
	fatalIf(t, err, "source: opening a fresh Session")
	sessionID := s.id
	t1, err := s.turn(p.script.remember)
	fatalIf(t, err, "source: the first turn")
	p.rep.launched("source: the fresh open", src)
	t2, err := s.turn(p.script.tool)
	fatalIf(t, err, "source: the tool turn")
	uses, output := t2.tools()
	p.check("source.turns", "two source turns: a reply with nonce A, a tool result with nonce T", true,
		t1.Outcome == contract.TurnCompleted && strings.Contains(t1.Reply, p.nonce.A) &&
			t2.Outcome == contract.TurnCompleted && uses > 0 && strings.Contains(output, p.nonce.T),
		"turn 1 %s, reply %q; turn 2 %s, %d tool call(s), output %q", t1.Outcome, t1.Reply, t2.Outcome, uses, strings.TrimSpace(output))
	if p.script.delegate != "" {
		t3, err := s.turn(p.script.delegate)
		fatalIf(t, err, "source: the subagent turn")
		kinds := map[contract.Kind]int{}
		for _, o := range t3.Seen {
			kinds[o.Kind]++
		}
		_, handBack := t3.tools()
		p.info("source.subagent", "a third source turn hands a task to a subagent",
			"turn %s; the subagent started %d time(s) and stopped %d; the tool result holds its reply: %v", t3.Outcome,
			kinds[contract.KindSubagentStarted], kinds[contract.KindSubagentStopped], strings.Contains(handBack, p.script.delegateReply))
	}
	p.rep.record("source", s.snapshot())

	res, cp, err := s.close()
	left := src.quiet(10 * time.Second)
	p.check("source.stopped", "the source harness and its children stopped before anything is copied", true,
		err == nil && res.Stopped && res.Drained && len(left) == 0,
		"Close: stopped=%v drained=%v err=%v; processes still naming the source: %d", res.Stopped, res.Drained, err, len(left))

	aux, err := p.h.SetAux(src, sessionID)
	if aux != nil || err != nil {
		left = src.quiet(10 * time.Second)
		p.check("source.aux", "the Session's name and goal are set, and read back, before the save", true,
			err == nil && len(left) == 0 && aux["name"] != "" && aux["goal"] != "",
			"%s; err=%v; processes left: %d", auxString(aux), err, len(left))
	}

	recordPath, err := p.h.Record(src, sessionID)
	fatalIf(t, err, "source: locating the Session's record")
	p.rep.Source = p.rep.environment(src, sessionID, recordPath)

	// The archives: every variant's, while the source is still there.
	var files []entry
	for _, root := range []contract.Root{contract.RootConfig, contract.RootHome, contract.RootWorkspace, contract.RootScratch} {
		f, other, err := tree(src.layout, root)
		fatalIf(t, err, "source: listing %s", root)
		files = append(files, f...)
		p.rep.Skipped = append(p.rep.Skipped, other...)
	}
	// What the harness left outside its roots is no part of any archive: it
	// is listed, and goes with the source.
	var outside []entry
	for _, dir := range p.h.Outside(src) {
		_ = filepath.WalkDir(dir, func(name string, d fs.DirEntry, err error) error {
			if err != nil || name == dir {
				return nil //nolint:nilerr // a directory that is not there holds nothing
			}
			e := entry{Root: "outside", Path: name}
			if d.IsDir() {
				e.Path += "/"
			} else if info, ierr := d.Info(); ierr == nil {
				e.Size = info.Size()
			}
			outside = append(outside, e)
			return nil
		})
		fatalIf(t, os.RemoveAll(dir), "removing %s", dir)
	}
	variants := p.h.Variants(src, sessionID)
	archives := map[string]string{}
	fatalIf(t, os.MkdirAll(filepath.Join(p.work, "archives"), 0o700), "archives")
	for _, v := range variants {
		m := manifest{Harness: p.desc.Harness, SessionID: sessionID, Layout: src.layout, Workspace: canonical(src.layout.Workspace), Variant: v.Name}
		for _, f := range files {
			if v.holds(f.Root, f.Path) {
				m.Entries = append(m.Entries, f)
			}
		}
		name := filepath.Join(p.work, "archives", v.Name+".tar")
		fatalIf(t, writeArchive(name, m), "saving the %s archive", v.Name)
		archives[v.Name] = name
		info, err := os.Stat(name)
		fatalIf(t, err, "the %s archive", v.Name)
		p.rep.Archives = append(p.rep.Archives, archiveReport{Variant: v.Name, Note: v.Note, Bytes: info.Size(), Entries: m.Entries})
	}
	p.rep.inventory(src, files, variants)
	p.rep.Outside = outside
	held := len(p.rep.Archives[0].Entries)
	p.check("archive.standalone", "the candidate state is copied to a standalone archive, each file with its size and hash", true,
		held > 0, "%d files in the %s archive; %d variants saved", held, variants[0].Name, len(variants))

	// The source goes away: moved from its path, and unreadable where it is
	// kept. Nothing after this line may read it.
	gone := goneSource{was: src.base, kept: filepath.Join(p.work, ".gone-"+randomWord(""))}
	gone.digest, gone.files, err = treeDigest(src.base)
	fatalIf(t, err, "source: digest")
	fatalIf(t, os.Rename(src.base, gone.kept), "taking the source away")
	fatalIf(t, os.Chmod(gone.kept, 0), "taking the source away") //nolint:gosec // the point: no access
	_, statErr := os.Stat(recordPath)
	_, readErr := os.ReadDir(gone.kept)
	p.check("source.inaccessible", "the source's roots are gone before the restore starts", true,
		errors.Is(statErr, fs.ErrNotExist) && (readErr != nil || os.Geteuid() == 0),
		"the record's path: %v; the kept copy: %v", p.rep.cleanErr(statErr), p.rep.cleanErr(readErr))
	return saved{sessionID: sessionID, cp: cp, aux: aux, variants: variants, archives: archives, gone: gone}
}

// loaded is one archive restored into a fresh environment, and its Session
// reopened there for one turn.
type loaded struct {
	v      variant
	env    *environment
	m      manifest
	rules  []relocation
	placed map[string]contract.RootPath
	seed   recordRead
	// carried is the restored record read from the source's checkpoint
	// instead, before anything else read it.
	carried    recordRead
	carriedErr error
	s          *session // open, when err is nil: the caller closes it
	first      turn
	reqs       int   // how many model requests the mock had answered before the reopen
	err        error // what stopped the load
}

// load restores an archive into a fresh environment, in the order a Load
// takes: restore the files, render and apply the configuration with a fresh
// credential, read the restored record to its end with no harness running,
// then reopen the Session by its native id from that checkpoint and send the
// first prompt. Given the source's checkpoint, it first tries that on the
// restored record, before anything else reads it; full marks the load the
// evidence describes.
func (p *probe) load(v variant, archive, label string, full bool, source *contract.Checkpoint) *loaded {
	ld := &loaded{v: v}
	ld.env = p.newEnvironment(label, filepath.Join(p.work, filepath.FromSlash(label), "agent"), restoredDirs)
	if ld.m, ld.err = readManifest(archive); ld.err != nil {
		return ld
	}
	ld.rules = p.h.Relocations(ld.m, ld.env.layout)
	if ld.placed, ld.err = restore(archive, ld.env.layout, ld.rules); ld.err != nil {
		return ld
	}
	ld.env.provision(p.h.Spec(p, nil))
	if full {
		record, _ := p.h.Record(ld.env, ld.m.SessionID)
		p.rep.Relocations = ld.rules
		p.rep.Restored = p.rep.environment(ld.env, ld.m.SessionID, record)
	}
	if source != nil {
		ld.carried, ld.carriedErr = ld.env.readRecord(ld.m.SessionID, source)
	}
	if ld.seed, ld.err = ld.env.readRecord(ld.m.SessionID, nil); ld.err != nil {
		return ld
	}
	ld.reqs = p.requestCount()
	if ld.s, ld.err = ld.env.open(contract.OpenReopen, ld.m.SessionID, ld.seed.CP); ld.err != nil {
		return ld
	}
	if full {
		p.rep.launched("restored: the reopen", ld.env)
	}
	if ld.first, ld.err = ld.s.turn(p.script.recall); ld.err != nil {
		_, _, _ = ld.s.close()
		ld.s = nil
	}
	return ld
}

// continuity is whether the first turn after a load continued the saved
// conversation, and why not. Mock: the request for it carries the source's
// turns. Live: the model recalled nonce A, without a tool.
func (p *probe) continuity(ld *loaded) (ok bool, detail string, n int, conv []item) {
	if ld.err != nil {
		return false, "the load stopped: " + p.rep.cleanErr(ld.err), 0, nil
	}
	if p.mode == modeLive {
		uses, _ := ld.first.tools()
		ok = ld.first.Outcome == contract.TurnCompleted && strings.Contains(ld.first.Reply, p.script.recallReply) && uses == 0
		return ok, fmt.Sprintf("the reply to %q was %q, after %d tool call(s)", p.script.recall, ld.first.Reply, uses), 0, nil
	}
	n, conv, err := p.request(p.requests(ld.reqs), p.script.recall)
	if err != nil {
		return false, err.Error(), 0, nil
	}
	if missing := p.carries(conv, p.script.recall); len(missing) > 0 {
		return false, fmt.Sprintf("request %d (%d items) lacks: %s", n, len(conv), strings.Join(missing, "; ")), n, conv
	}
	return true, fmt.Sprintf("request %d carries the source's two turns — prompt, reply, tool call, tool result, reply — before the new prompt (%d items)", n, len(conv)), n, conv
}

// fresh checks that a Session's record-origin deliveries are its own turns'
// and nothing older: no id among known, exactly the prompts sent as its user
// inputs, and no entry dated at or before last, the restored record's last
// entry. A zero last leaves the dates alone: a record saved on another machine
// was dated by another clock.
func fresh(record []contract.Observation, known map[string]bool, last time.Time, prompts ...string) (ok bool, detail string) {
	var repeated, old, inputs []string
	for _, o := range record {
		if known[o.ID] {
			repeated = append(repeated, o.ID)
		}
		if !last.IsZero() && !o.Time.After(last) {
			old = append(old, o.ID)
		}
		if o.Kind == contract.KindUserInput {
			var d contract.TextData
			_ = o.Decode(&d)
			inputs = append(inputs, strings.TrimSpace(d.Text))
		}
	}
	ok = len(repeated) == 0 && len(old) == 0 && strings.Join(inputs, "\x00") == strings.Join(prompts, "\x00")
	dated := fmt.Sprintf("dated no later than its last entry: %d", len(old))
	if last.IsZero() {
		dated = "dates not compared: the record was saved under another machine's clock"
	}
	detail = fmt.Sprintf("%d record observations; ids the restored record already held: %d; %s; user inputs: %q", len(record), len(repeated), dated, inputs)
	return ok, detail
}

// latest is the time of the latest of a record's observations.
func latest(items []contract.Observation) time.Time {
	var at time.Time
	for _, o := range items {
		if o.Time.After(at) {
			at = o.Time
		}
	}
	return at
}

func recordOf(seen []contract.Observation) []contract.Observation {
	var out []contract.Observation
	for _, o := range seen {
		if o.Origin == contract.OriginRecord {
			out = append(out, o)
		}
	}
	return out
}

// continued runs the gate's checks on the candidate's load: what the restored
// record gave the record reader, what the reopened Session sent and reported,
// and what a stop and a second reopen kept.
func (p *probe) continued(ld *loaded, sv saved) {
	t := p.t
	sessionID, srcCP, aux := sv.sessionID, sv.cp, sv.aux
	if ld.placed == nil {
		t.Fatalf("restoring the candidate archive: %v", ld.err)
	}
	dst := ld.env

	// The restored record, read to its end with no harness running.
	var sawPrompt, sawReply, sawUse, sawResult bool
	for _, o := range ld.seed.Items {
		switch o.Kind {
		case contract.KindUserInput:
			var d contract.TextData
			_ = o.Decode(&d)
			sawPrompt = sawPrompt || strings.Contains(d.Text, p.script.remember)
		case contract.KindAssistantText:
			var d contract.AssistantTextData
			_ = o.Decode(&d)
			sawReply = sawReply || strings.Contains(d.Text, p.nonce.A)
		case contract.KindToolUse:
			sawUse = sawUse || strings.Contains(string(o.Data), p.nonce.T)
		case contract.KindToolResult:
			sawResult = sawResult || strings.Contains(string(o.Data), p.nonce.T)
		}
	}
	recordPath, rerr := p.h.Record(dst, sessionID)
	saved, isSaved := p.savedRecord(ld)
	at, _ := offset(ld.seed.CP)
	p.check("load.record-read", "the adapter's record reader reaches the restored record's end, with nothing published", true,
		rerr == nil && ld.seed.Ended && ld.seed.CP != nil && isSaved && at.Offset == saved.Size &&
			sawPrompt && sawReply && sawUse && sawResult && len(ld.seed.Resets)+len(ld.seed.Rescans)+len(ld.seed.Faults) == 0,
		"%d observations in %d batch(es), end of record %v; the saved prompt, reply, tool call and tool result read: %v %v %v %v; checkpoint at offset %d of the restored %d bytes; resets %d, rescans %d, faults %d; err=%v",
		len(ld.seed.Items), ld.seed.Batches, ld.seed.Ended, sawPrompt, sawReply, sawUse, sawResult, at.Offset, saved.Size,
		len(ld.seed.Resets), len(ld.seed.Rescans), len(ld.seed.Faults), p.rep.cleanErr(rerr))
	p.rep.record("restored: read to the end before the reopen (not published)", ld.seed.Items)

	// The source's own checkpoint, tried on the restored record: is a
	// checkpoint something an archive could carry?
	if ld.carriedErr == nil {
		was, _ := offset(srcCP)
		reasons := make([]string, 0, len(ld.carried.Resets))
		for _, r := range ld.carried.Resets {
			reasons = append(reasons, r.Reason)
		}
		p.info("checkpoint.carried", "the source's checkpoint, given to the restored record",
			"the source's checkpoint (offset %d, inode %d) on the restored file: %d reset(s) %v, then all %d observations delivered again — a checkpoint names the file it was made on, so a Load seeds its own",
			was.Offset, was.Inode, len(ld.carried.Resets), reasons, len(ld.carried.Items))
	}

	fatalIf(t, ld.err, "reopening the restored Session")
	s := ld.s

	// The first turn after the restore.
	ok, detail, n, conv := p.continuity(ld)
	title := "the first resumed model request carries the saved turns and the tool result"
	if p.mode == modeLive {
		title = "the live model recalls nonce A in the restored Session, without a tool"
	}
	silent := !strings.Contains(p.script.recall, p.nonce.A) && !strings.Contains(p.script.recall, p.nonce.T)
	p.check("load.history", title, true, ok && silent, "%s; the prompt names no nonce: %v", detail, silent)
	if conv != nil {
		p.rep.request("the first resumed request", n, conv)
	}
	if subs := p.h.Subagents(ld.m.Entries); len(subs) > 0 {
		moved, names := 0, make([]string, 0, len(subs))
		for _, e := range subs {
			to := ld.placed[e.name()]
			names = append(names, to.Path)
			if sum, _, err := hashFile(filepath.Join(dst.layout.Path(to.Root), filepath.FromSlash(to.Path)), -1); err == nil && sum == e.SHA256 {
				moved++
			}
		}
		handed := false
		for _, it := range conv {
			handed = handed || it.Type == "tool_result" && strings.Contains(it.Text, p.script.delegateReply)
		}
		p.info("load.subagents", "a subagent's record, saved beneath the Session's own, moves with it",
			"%d of %d subagent file(s) restored with their saved content, at %v; the first resumed request carries the subagent's hand-back: %v", moved, len(subs), names, handed)
	}
	p.check("load.same-session", "the Session reopens under its saved native id", true,
		s.id == sessionID, "saved %s, reopened %s", sessionID, s.id)
	var last time.Time
	if sv.gone.kept != "" {
		last = latest(ld.seed.Items)
	}
	ok, detail = fresh(ld.first.record(), ld.seed.ids(), last, p.script.recall)
	resets, faults := s.disturbed()
	p.check("load.new-events-only", "after the reopen from the seeded checkpoint, only the new turn's record events are delivered", true,
		ok && resets+faults == 0, "%s; resets %d, faults %d", detail, resets, faults)
	p.rep.record("restored: the first Session after the reopen", s.snapshot())
	known := ld.seed.ids()
	for _, o := range recordOf(s.snapshot()) {
		known[o.ID] = true
	}

	// Stop, and reopen again from the checkpoint this Session left.
	res, cp1, err := s.close()
	left := dst.quiet(10 * time.Second)
	if !res.Stopped || !res.Drained || err != nil || len(left) > 0 {
		t.Errorf("closing the restored Session: stopped=%v drained=%v err=%v, %d processes left", res.Stopped, res.Drained, err, len(left))
	}
	if aux != nil {
		got, err := p.h.Aux(dst, sessionID)
		p.check("load.aux", "the Session's name and goal are read back after the load", true,
			err == nil && sameAux(aux, got), "saved %s; read back %s; err=%v", auxString(aux), auxString(got), err)
	}
	reqs := p.requestCount()
	s2, err := dst.open(contract.OpenReopen, sessionID, cp1)
	fatalIf(t, err, "the second reopen")
	again, err := s2.turn(p.script.again)
	fatalIf(t, err, "the turn after the second reopen")
	where, err := s2.turn(p.script.where)
	fatalIf(t, err, "the working-directory turn")

	if p.mode == modeLive {
		uses, _ := again.tools()
		ok = again.Outcome == contract.TurnCompleted && strings.Contains(again.Reply, p.script.againReply) && uses == 0
		detail = fmt.Sprintf("the reply to %q was %q, after %d tool call(s)", p.script.again, again.Reply, uses)
	} else {
		n, conv, err := p.request(p.requests(reqs), p.script.again)
		var missing []string
		if err == nil {
			missing = p.carries(conv, p.script.again, [2]string{p.script.recall, p.script.recallReply})
			p.rep.request("the first request after the second reopen", n, conv)
		}
		ok = err == nil && len(missing) == 0 && strings.Contains(again.Reply, p.script.againReply)
		detail = fmt.Sprintf("request %d carries the saved turns and the first restored turn; missing: %v; err=%v", n, missing, err)
	}
	fresh2, fdetail := fresh(recordOf(s2.snapshot()), known, last, p.script.again, p.script.where)
	resets, faults = s2.disturbed()
	cp1at, _ := offset(cp1)
	p.check("load.second-reopen", "after a stop and a second reopen, what the restored Session added is kept, and only new record events follow", true,
		ok && s2.id == sessionID && fresh2 && resets+faults == 0,
		"%s; reopened %s from the checkpoint at offset %d; %s; resets %d, faults %d", detail, s2.id, cp1at.Offset, fdetail, resets, faults)
	p.rep.record("restored: the Session after the second reopen", s2.snapshot())

	_, output := where.tools()
	inNew := strings.Contains(output, dst.layout.Workspace) || strings.Contains(output, canonical(dst.layout.Workspace))
	p.check("load.new-cwd", "the resumed Session works in the new workspace", true,
		where.Outcome == contract.TurnCompleted && inNew && !strings.Contains(output, ld.m.Workspace),
		"a tool run after the restore printed %q", p.rep.clean(strings.TrimSpace(output)))
	if _, held := p.placedAt(ld, contract.RootWorkspace, noteFile); held && p.mode == modeMock {
		p.info("load.workspace-file", "a workspace file saved with the Session is read by a tool in the new workspace",
			"%s restored; the tool's output holds nonce T: %v (a smoke check, not workspace fidelity)", noteFile, strings.Contains(output, p.nonce.T))
	}

	res, _, err = s2.close()
	left = dst.quiet(10 * time.Second)
	if !res.Stopped || !res.Drained || err != nil || len(left) > 0 {
		t.Errorf("closing the restored Session again: stopped=%v drained=%v err=%v, %d processes left", res.Stopped, res.Drained, err, len(left))
	}
	if aux != nil {
		got, err := p.h.Aux(dst, sessionID)
		p.check("load.aux-kept", "the Session's name and goal are still there after two more turns and a second reopen", true,
			err == nil && sameAux(aux, got), "saved %s; read back %s; err=%v", auxString(aux), auxString(got), err)
	}

	// The restored record was continued, never rewritten: what the archive
	// held is still the file's first bytes.
	prefix, _, herr := hashFile(recordPath, saved.Size)
	info, serr := os.Stat(recordPath)
	grew := serr == nil && info.Size() > saved.Size
	p.check("load.no-rewrite", "the restored record is appended to, its saved bytes unchanged: relocation moves paths, not contents", true,
		herr == nil && isSaved && prefix == saved.SHA256 && grew,
		"the first %d bytes of the continued record hash to the saved sha256: %v; it grew: %v; relocation rules: %d", saved.Size, prefix == saved.SHA256, grew, len(ld.rules))

	if mem, held := p.placedAt(ld, contract.RootConfig, "memory/notes.md"); held {
		sum, _, err := hashFile(filepath.Join(dst.layout.Config, "memory", "notes.md"), -1)
		p.info("load.memory", "the saved memory file is in the new memory directory",
			"memory/notes.md restored with the saved content: %v", err == nil && sum == mem.SHA256)
	}
	if p.mode == modeMock {
		p.credentials(ld)
	}
}

// savedRecord is the archive's entry of the Session's record: the one file
// the restored record is.
func (p *probe) savedRecord(ld *loaded) (entry, bool) {
	recordPath, err := p.h.Record(ld.env, ld.m.SessionID)
	if err != nil {
		return entry{}, false
	}
	for _, e := range ld.m.Entries {
		to, ok := ld.placed[e.name()]
		if ok && canonical(filepath.Join(ld.env.layout.Path(to.Root), filepath.FromSlash(to.Path))) == canonical(recordPath) {
			return e, true
		}
	}
	return entry{}, false
}

// placedAt finds the saved file a restore put at root/path.
func (p *probe) placedAt(ld *loaded, root contract.Root, path string) (entry, bool) {
	for _, e := range ld.m.Entries {
		if to, ok := ld.placed[e.name()]; ok && to.Root == root && to.Path == path {
			return e, true
		}
	}
	return entry{}, false
}

// credentials checks, against the mock, that a restored environment's
// requests came with the credential staged for it, never the source's.
func (p *probe) credentials(ld *loaded) {
	own, source, none := 0, 0, 0
	for _, r := range p.requests(ld.reqs) {
		switch {
		case strings.Contains(r.Auth, placeholder(ld.env.label)):
			own++
		case strings.Contains(r.Auth, placeholder("source")):
			source++
		case r.Auth == "":
			none++
		}
	}
	p.check("load.fresh-credential", "the restored Session's requests carry the credential staged for the new environment", true,
		own > 0 && source == 0 && none == 0, "%d requests with the new environment's credential, %d with the source's, %d with none", own, source, none)
}

// placeholder is the mock credential of the environment named label.
func placeholder(label string) string {
	return "mock-" + strings.NewReplacer("/", "-", "+", "-").Replace(label) + "-not-a-credential"
}

// variant loads one of the other inventories, to find the least a Load needs
// and what each file carries.
func (p *probe) variant(v variant, archive string, sv saved) {
	sessionID, aux := sv.sessionID, sv.aux
	ld := p.load(v, archive, "restored/"+v.Name, false, nil)
	ok, detail, _, _ := p.continuity(ld)
	row := variantReport{Variant: v.Name, Note: v.Note, Files: len(ld.m.Entries), Conversation: ok, Detail: detail}
	if ld.s != nil {
		row.SameSession = ld.s.id == sessionID
		_, _, _ = ld.s.close()
		ld.env.quiet(10 * time.Second)
	}
	if aux != nil && ld.err == nil {
		got, err := p.h.Aux(ld.env, sessionID)
		row.Aux = got
		if err != nil {
			row.Detail += "; reading the name and goal: " + p.rep.cleanErr(err)
		}
	}
	p.rep.Variants = append(p.rep.Variants, row)
}

// negative is the control: the same load with nothing restored. It must fail
// to resume, or lack the earlier context.
func (p *probe) negative(sessionID string) {
	neg := p.newEnvironment("negative", filepath.Join(p.work, "negative", "agent"), restoredDirs)
	neg.provision(p.h.Spec(p, nil))

	detail, err := p.h.Resume(neg, sessionID)
	neg.quiet(10 * time.Second)
	p.check("negative.harness-refuses", "with no history restored, the harness alone refuses to resume the id", true,
		errors.Is(err, errNoHistory), "%s", p.rep.clean(firstNonEmpty(detail, fmt.Sprint(err))))

	seed, err := neg.readRecord(sessionID, nil)
	p.info("negative.empty-record", "with no history restored, the record reader finds an empty record",
		"%d observations, checkpoint %v, end of record %v, err=%v — the sign a Load must refuse on, before any open", len(seed.Items), seed.CP != nil, seed.Ended, err)

	reqs := p.requestCount()
	s, err := neg.open(contract.OpenReopen, sessionID, nil)
	if err != nil {
		p.check("negative.no-context", "with no history restored, a reopen through the adapter fails or lacks the earlier context", true,
			true, "the reopen failed: %s", p.rep.cleanErr(err))
		return
	}
	tn, err := s.turn(p.script.recall)
	var lacks bool
	var what string
	if p.mode == modeLive {
		lacks = err == nil && !strings.Contains(tn.Reply, p.nonce.A)
		what = fmt.Sprintf("the reply to %q was %q", p.script.recall, tn.Reply)
	} else {
		n, conv, rerr := p.request(p.requests(reqs), p.script.recall)
		lacks = err == nil && rerr == nil && !p.mentions(conv)
		what = fmt.Sprintf("request %d has %d items; a nonce among them: %v (err=%v)", n, len(conv), p.mentions(conv), rerr)
		if conv != nil {
			p.rep.request("the negative control's request", n, conv)
		}
	}
	p.check("negative.no-context", "with no history restored, a reopen through the adapter fails or lacks the earlier context", true,
		lacks, "the adapter opened a Session %s (the saved id: %v) — a fresh conversation, not an error; %s; err=%v", s.id, s.id == sessionID, what, err)
	_, _, _ = s.close()
	neg.quiet(10 * time.Second)
}

// untouched checks, once every restored environment has run, that nothing
// read from or wrote to the source: its path is still absent, and the copy
// kept out of reach holds what it held.
func (p *probe) untouched(gone goneSource) {
	_, statErr := os.Stat(gone.was)
	err := os.Chmod(gone.kept, 0o700) //nolint:gosec // the probe's own directory
	digest, files, derr := treeDigest(gone.kept)
	p.check("source.untouched", "nothing was read from or written to the source while the restored Sessions ran", true,
		errors.Is(statErr, fs.ErrNotExist) && err == nil && derr == nil && digest == gone.digest && files == gone.files,
		"the source's path is still absent: %v; its %d files, kept out of reach, are unchanged: %v", errors.Is(statErr, fs.ErrNotExist), gone.files, digest == gone.digest)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ---- the tests

// The mock-backed runs: the real pinned binary, the mock model API.

func TestClaudeCodeSaveLoad(t *testing.T) { run(t, claudeCode{}, modeMock) }

func TestCodexSaveLoad(t *testing.T) { run(t, codexCLI{}, modeMock) }

// The live smoke runs: the real pinned binary, its real model API, a
// credential the caller names. Skipped — pending — without one.

func TestClaudeCodeSaveLoadLive(t *testing.T) { run(t, claudeCode{}, modeLive) }

func TestCodexSaveLoadLive(t *testing.T) { run(t, codexCLI{}, modeLive) }
