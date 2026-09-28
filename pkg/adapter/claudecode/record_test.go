package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// The 2.1.283 fixture's session, and the native ids of the inputs it holds.
const fixtureSession = "a1f82cd3-6936-4ffd-8998-70b64b65dec9"

var fixtureInputs = map[string]string{
	"in-ping":  "836c3ebb-1e87-4a68-9aa2-127f926f40e6", // PING 1
	"in-slow":  "ed92a320-e8b0-4f64-9122-98294968809f", // SLOW 20, interrupted mid-reply
	"in-tool":  "001ce920-4466-4105-8857-f9632c9e9634", // TOOL sleep 5, interrupted mid-tool
	"in-stall": "09a22003-0543-48d0-8fa0-ea62e54e1026", // STALL 30, interrupted before the first token
	"in-529":   "600e83f1-0bc5-4712-b0d3-4ec504ed12c0", // ERR 529 99
	"in-gone":  "540bfeb7-5fbf-4cc1-8d13-6e32c0f250fa", // ERR 429 99, until the mock went away
}

// recordAgent lays out an agent whose Session's transcript is file, with a
// marker for each of inputs.
func recordAgent(t *testing.T, session, file string, inputs map[string]string) (contract.Layout, []byte, *adapter.Markers) {
	t.Helper()
	base := t.TempDir()
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"), Workspace: filepath.Join(base, "workspace"),
		Secrets: filepath.Join(base, "secrets"), Scratch: filepath.Join(base, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Profile{}.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/claude-code", Layout: l, Spec: contract.AgentSpec{PermissionPosture: contract.PostureBypass}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := parseOpenConfig(res.OpenConfig)
	f, err := claudecode.FollowEntries(session, cfg.WorkingDir, cfg.Env, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := adapter.OpenMarkers(l.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	for in, native := range inputs {
		if err := m.Write(adapter.Marker{InputID: in, Native: native, SessionID: session, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return l, res.OpenConfig, m
}

// readAll reads the record to its end, chunk by chunk, committing each.
func readAll(t *testing.T, r adapter.Reader, max int) (items []contract.Observation, chunks []adapter.Chunk) {
	t.Helper()
	for i := 0; ; i++ {
		ch, err := r.Read(context.Background(), max)
		if err != nil {
			t.Fatal(err)
		}
		if len(ch.Items) == 0 && ch.Checkpoint == nil && ch.Reset == nil && ch.Rescan == nil && len(ch.Faults) == 0 {
			return items, chunks
		}
		if i > 10000 {
			t.Fatal("the record never ends")
		}
		items = append(items, ch.Items...)
		chunks = append(chunks, ch)
		if err := r.Commit(ch); err != nil {
			t.Fatal(err)
		}
	}
}

func openReader(t *testing.T, session string, l contract.Layout, oc []byte, m *adapter.Markers, cp *contract.Checkpoint) adapter.Reader {
	t.Helper()
	r, err := Profile{}.Record(adapter.RecordSource{SessionID: session, OpenConfig: oc, Layout: l, Checkpoint: cp, Markers: m})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func turnEnds(items []contract.Observation) map[string]contract.TurnEndedData {
	out := map[string]contract.TurnEndedData{}
	for _, o := range items {
		if o.Kind == contract.KindTurnEnded {
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			out[o.InputID] = d
		}
	}
	return out
}

// The record of a 2.1.283 session yields each input's items, attributed to
// it, and the end of every turn the record proves: the reply, three
// interrupts, and two errors, classed by what claude wrote.
func TestReadRecord(t *testing.T) {
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", fixtureInputs)
	items, chunks := readAll(t, openReader(t, fixtureSession, l, oc, m, nil), contract.MaxObserveBytes)
	if len(chunks) == 0 || chunks[len(chunks)-1].Checkpoint == nil || chunks[len(chunks)-1].Checkpoint.Format != CheckpointFormat {
		t.Fatalf("chunks %+v: want the last with a format-%d checkpoint", len(chunks), CheckpointFormat)
	}
	ends := turnEnds(items)
	for in, want := range map[string]contract.TurnOutcome{
		"in-ping": contract.TurnCompleted, "in-slow": contract.TurnInterrupted, "in-tool": contract.TurnInterrupted,
		"in-stall": contract.TurnInterrupted, "in-529": contract.TurnErrored, "in-gone": contract.TurnErrored,
	} {
		if got := ends[in]; got.Outcome != want {
			t.Errorf("%s ended %+v, want %s", in, got, want)
		}
	}
	if d := ends["in-ping"]; d.Text != "PONG 1" {
		t.Errorf("the reply's end has text %q", d.Text)
	}
	if e := ends["in-529"].Error; e == nil || e.Class != contract.ErrorOverloaded || e.HTTPStatus != 529 {
		t.Errorf("the exhausted 529: %+v", e)
	}
	if e := ends["in-gone"].Error; e == nil || e.Class != contract.ErrorAPI {
		t.Errorf("the connection refused: %+v", e)
	}
	ids := map[string]bool{}
	kinds := map[contract.Kind]int{}
	for _, o := range items {
		if ids[o.ID] {
			t.Errorf("id %s twice", o.ID)
		}
		ids[o.ID] = true
		kinds[o.Kind]++
		if o.Origin != contract.OriginRecord || !strings.HasPrefix(o.ID, string(o.Kind)+":") {
			t.Errorf("item %s: origin %s", o.ID, o.Origin)
		}
		if o.Kind == contract.KindUserInput && o.Entry == fixtureInputs["in-ping"] && o.InputID != "in-ping" {
			t.Errorf("the prompt's user_input is attributed to %q", o.InputID)
		}
	}
	if kinds[contract.KindToolUse] != 1 || kinds[contract.KindToolResult] != 1 || kinds[contract.KindAPIError] != 2 || kinds[contract.KindAssistantText] < 2 {
		t.Errorf("kinds %v", kinds)
	}

	// Read in small chunks, the record gives the same items.
	small, _ := readAll(t, openReader(t, fixtureSession, l, oc, m, nil), 1)
	if len(small) != len(items) {
		t.Fatalf("read a record at a time: %d items, want %d", len(small), len(items))
	}
	for i := range items {
		if small[i].ID != items[i].ID {
			t.Errorf("item %d: %s, want %s", i, small[i].ID, items[i].ID)
		}
	}
}

// A reader opened at a checkpoint in the middle of a turn knows which input
// the turn is, and one given a checkpoint it cannot read reads from the start,
// with the same ids.
func TestReopenReader(t *testing.T) {
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", fixtureInputs)
	r := openReader(t, fixtureSession, l, oc, m, nil)
	var all []contract.Observation
	var mid *contract.Checkpoint
	for {
		ch, err := r.Read(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if ch.Checkpoint == nil && len(ch.Items) == 0 {
			break
		}
		all = append(all, ch.Items...)
		for _, o := range ch.Items {
			if o.Kind == contract.KindUserInput && o.InputID == "in-slow" && mid == nil {
				mid = ch.Checkpoint
			}
		}
		if err := r.Commit(ch); err != nil {
			t.Fatal(err)
		}
	}
	if mid == nil {
		t.Fatal("no checkpoint after the slow input's prompt")
	}
	rest, _ := readAll(t, openReader(t, fixtureSession, l, oc, m, mid), contract.MaxObserveBytes)
	if len(rest) == 0 || rest[0].InputID != "in-slow" {
		t.Fatalf("after the checkpoint: %+v, want the slow input's items first", rest)
	}
	if d, ok := turnEnds(rest)["in-slow"]; !ok || d.Outcome != contract.TurnInterrupted {
		t.Errorf("the slow turn's end after reopening: %+v", d)
	}

	rescanned, chunks := readAll(t, openReader(t, fixtureSession, l, oc, m, &contract.Checkpoint{Format: 99, Data: []byte("?")}), contract.MaxObserveBytes)
	if chunks[0].Rescan == nil {
		t.Error("an unreadable checkpoint without a rescan")
	}
	if len(rescanned) != len(all) {
		t.Errorf("rescan: %d items, want %d", len(rescanned), len(all))
	}
	for i := range all {
		if i < len(rescanned) && rescanned[i].ID != all[i].ID {
			t.Errorf("rescan item %d: %s, want %s", i, rescanned[i].ID, all[i].ID)
		}
	}
}

// Recover answers from the record: each input's end, and unknown for one the
// record does not hold.
func TestRecoverFromRecord(t *testing.T) {
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", fixtureInputs)
	r := openReader(t, fixtureSession, l, oc, m, nil)
	for in, want := range map[string]contract.RecoveredOutcome{
		"in-ping": contract.RecoveredCompleted, "in-slow": contract.RecoveredInterrupted, "in-tool": contract.RecoveredInterrupted,
		"in-stall": contract.RecoveredInterrupted, "in-529": contract.RecoveredErrored, "in-gone": contract.RecoveredErrored,
	} {
		mk, _, _ := m.Lookup(in)
		if got, err := r.Recover(context.Background(), mk); err != nil || got.Outcome != want {
			t.Errorf("Recover(%s) = %+v %v, want %s", in, got, err, want)
		}
	}
	got, err := r.Recover(context.Background(), adapter.Marker{InputID: "in-lost", Native: "00000000-0000-4000-8000-000000000000", SessionID: fixtureSession})
	if err != nil || got.Outcome != contract.RecoveredUnknown {
		t.Errorf("Recover of an input the record lacks = %+v %v, want unknown", got, err)
	}
}

// Checkpoint format 1 is what agentd stored before this profile: a
// checkpoint copied from a runtime's node.db, over the transcript it covers,
// resumes exactly where it stood — and over a copy of that transcript (another
// inode), the reader resets and reads it again.
func TestNodeDBCheckpoint(t *testing.T) {
	const session = "5e413723-3603-4b13-899c-6b1e47359544"
	stored, err := os.ReadFile("testdata/nodedb/checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	l, oc, m := recordAgent(t, session, "testdata/nodedb/transcript.jsonl", nil)

	// The transcript here is a copy: another inode. The reader reports a
	// reset and reads the copy from its start.
	items, chunks := readAll(t, openReader(t, session, l, oc, m, &contract.Checkpoint{Format: 1, Data: stored}), contract.MaxObserveBytes)
	if len(chunks) == 0 || chunks[0].Reset == nil || chunks[0].Reset.Reason != "replaced" {
		t.Fatalf("over a copy: %+v, want a reset (replaced)", chunks)
	}
	var reply bool
	for _, o := range items {
		var d contract.AssistantTextData
		if o.Kind == contract.KindAssistantText && o.Decode(&d) == nil && d.Text == "PONG reimaged" {
			reply = true
		}
	}
	if !reply {
		t.Error("the copy's reply was not read again")
	}

	// With the copy's inode, the stored checkpoint covers exactly the file:
	// nothing to read, and a record appended after it is read alone.
	var cp map[string]any
	_ = json.Unmarshal(stored, &cp)
	cfg, _ := parseOpenConfig(oc)
	f, _ := claudecode.FollowEntries(session, cfg.WorkingDir, cfg.Env, transcript.Checkpoint{})
	fi, err := os.Stat(f.Path())
	if err != nil {
		t.Fatal(err)
	}
	cp["inode"] = fi.Sys().(*syscall.Stat_t).Ino
	here, _ := json.Marshal(cp)
	r := openReader(t, session, l, oc, m, &contract.Checkpoint{Format: 1, Data: here})
	if ch, err := r.Read(context.Background(), contract.MaxObserveBytes); err != nil || ch.Reset != nil || len(ch.Items) != 0 {
		t.Fatalf("at the stored checkpoint: %+v %v, want nothing to read", ch, err)
	}
	next := `{"type":"user","uuid":"11111111-2222-4333-8444-555555555555","message":{"role":"user","content":"after the checkpoint"},"timestamp":"2026-09-28T09:30:00Z"}` + "\n"
	fh, err := os.OpenFile(f.Path(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString(next)
	_ = fh.Close()
	ch, err := r.Read(context.Background(), contract.MaxObserveBytes)
	if err != nil || ch.Reset != nil || len(ch.Items) != 1 || ch.Items[0].Entry != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("after appending: %+v %v, want the one new entry", ch, err)
	}
}

// A spool file's observations are delivered with the chunk that holds them,
// and the file stays until that chunk is committed; a file that reports
// nothing — a session marker — goes at once.
func TestSpoolCommittedBeforeDeletion(t *testing.T) {
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", nil)
	cfg, _ := parseOpenConfig(oc)
	if err := os.MkdirAll(cfg.Spool, 0o700); err != nil {
		t.Fatal(err)
	}
	pre := transcript.ParsedEvent{HarnessSessionID: fixtureSession, Event: transcript.Event{
		Type: transcript.EventToolUse, Role: transcript.RoleAssistant, ToolName: "Bash", ToolUseID: "toolu_9",
		ToolInput: json.RawMessage(`{"command":"echo hi"}`), Source: transcript.SourceHook,
	}}
	marker := transcript.ParsedEvent{HarnessSessionID: fixtureSession, Event: transcript.Event{Type: transcript.EventSessionMeta, Source: transcript.SourceFile}}
	for name, evs := range map[string][]transcript.ParsedEvent{"pre-tool-use-1-1-1.json": {pre}, "session-start-1-1-2.json": {marker}} {
		data, err := transcript.MarshalParsedEvents(evs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.Spool, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := openReader(t, fixtureSession, l, oc, m, nil)
	ch, err := r.Read(context.Background(), contract.MaxObserveBytes)
	if err != nil {
		t.Fatal(err)
	}
	var started bool
	for _, o := range ch.Items {
		started = started || o.ID == "tool_started:toolu_9"
	}
	if !started {
		t.Fatal("no tool_started from the spool")
	}
	if _, err := os.Stat(filepath.Join(cfg.Spool, "pre-tool-use-1-1-1.json")); err != nil {
		t.Errorf("the tool hook's file went before its chunk was committed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Spool, "session-start-1-1-2.json")); err == nil {
		t.Error("a file that reports nothing stays")
	}
	if err := r.Commit(ch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Spool, "pre-tool-use-1-1-1.json")); err == nil {
		t.Error("the tool hook's file stays after its chunk was committed")
	}
}

// A prompt without a marker — sent before this profile, or by another host —
// never inherits the input of a turn that ended before it.
func TestUnmarkedPromptTakesNoInput(t *testing.T) {
	only := map[string]string{"in-ping": fixtureInputs["in-ping"]}
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", only)
	items, _ := readAll(t, openReader(t, fixtureSession, l, oc, m, nil), contract.MaxObserveBytes)
	ends := turnEnds(items)
	if len(ends) != 1 || ends["in-ping"].Outcome != contract.TurnCompleted {
		t.Errorf("turn ends %v, want only in-ping's", ends)
	}
	for _, o := range items {
		if o.InputID == "in-ping" && o.Entry != fixtureInputs["in-ping"] && o.Kind != contract.KindAssistantText && o.Kind != contract.KindTurnEnded {
			t.Errorf("%s (entry %s) is attributed to in-ping", o.ID, o.Entry)
		}
	}
}

// With LegacyHookFacts, a Stop hook's file reports its firing, keyed by the
// file's digest; without, nothing.
func TestLegacyHookFacts(t *testing.T) {
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", nil)
	cfg, _ := parseOpenConfig(oc)
	stop := transcript.ParsedEvent{HarnessSessionID: fixtureSession, Event: transcript.Event{Type: transcript.EventSessionMeta, Source: transcript.SourceFile}}
	write := func(name string) {
		data, _ := transcript.MarshalParsedEvents([]transcript.ParsedEvent{stop})
		if err := os.WriteFile(filepath.Join(cfg.Spool, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := func() []contract.Observation {
		items, _ := readAll(t, openReader(t, fixtureSession, l, oc, m, nil), contract.MaxObserveBytes)
		var out []contract.Observation
		for _, o := range items {
			if o.Kind == KindLegacyHook {
				out = append(out, o)
			}
		}
		return out
	}
	write("stop-1-1-1.json")
	if got := legacy(); len(got) != 0 {
		t.Errorf("without LegacyHookFacts: %v", got)
	}
	LegacyHookFacts.Store(true)
	defer LegacyHookFacts.Store(false)
	write("stop-1-1-2.json")
	write("session-end-1-1-3.json")
	got := legacy()
	if len(got) != 2 {
		t.Fatalf("legacy hook facts %v, want the stop and the session end", got)
	}
	hooks := map[string]bool{}
	for _, o := range got {
		var d LegacyHookData
		_ = o.Decode(&d)
		hooks[d.Hook] = true
		if !strings.HasPrefix(d.Digest, "sha256:") || o.Key() != strings.TrimPrefix(d.Digest, "sha256:")[:24] {
			t.Errorf("%s: digest %s", o.ID, d.Digest)
		}
	}
	if !hooks["stop"] || !hooks["session_end"] {
		t.Errorf("hooks %v", hooks)
	}
}
