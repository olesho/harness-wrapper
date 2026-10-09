package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/harnesscore"
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

// ownTurnTranscript is a Session's transcript in which the input's turn
// starts background work and claude, once it ends, takes the result up in a
// turn of its own (ADR-017). With inputEnd, the input's turn ends first, as
// claude ends it; without, the record holds no end of it.
func ownTurnTranscript(t *testing.T, session, native string, inputEnd bool) string {
	t.Helper()
	entry := func(uuid, parent, typ string, extra map[string]any) string {
		e := map[string]any{"type": typ, "uuid": uuid, "parentUuid": parent, "sessionId": session, "isSidechain": false, "timestamp": "2026-10-09T10:00:00.000Z"}
		for k, v := range extra {
			e[k] = v
		}
		b, _ := json.Marshal(e)
		return string(b)
	}
	lines := []string{
		entry(native, "", "user", map[string]any{"message": map[string]any{"role": "user", "content": "BG sleep 3; echo bg-done"}}),
		entry("a1", native, "assistant", map[string]any{"message": map[string]any{
			"id": "msg_1", "role": "assistant", "stop_reason": "tool_use",
			"content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "sleep 3; echo bg-done", "run_in_background": true}}},
		}}),
		entry("u2", "a1", "user", map[string]any{"message": map[string]any{
			"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "Command running in background with ID: b1"}},
		}}),
	}
	parent := "u2"
	if inputEnd {
		lines = append(lines, entry("a3", parent, "assistant", map[string]any{"message": map[string]any{
			"id": "msg_2", "role": "assistant", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "TOOL DONE"}},
		}}))
		parent = "a3"
	}
	lines = append(
		lines,
		entry("u4", parent, "user", map[string]any{"origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{
			"role": "user", "content": "<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>",
		}}),
		entry("a5", "u4", "assistant", map[string]any{"message": map[string]any{
			"id": "msg_3", "role": "assistant", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "BG DONE"}},
		}}),
	)
	file := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// The turn claude starts itself after an input's turn is not that turn: the
// end of the one is no evidence of the other's. An input whose turn the
// record holds no end of is unknown, however claude's own turn after it
// ended.
func TestRecoverStopsAtAnOwnTurn(t *testing.T) {
	const session, native = "7c1a5c0e-3a52-4b8e-9d3e-0b8d1f2a6c11", "1b6f0a3e-58c4-4f43-a1d2-6a7e9c0d4b25"
	for _, tc := range []struct {
		inputEnd bool
		want     contract.RecoveredOutcome
	}{{true, contract.RecoveredCompleted}, {false, contract.RecoveredUnknown}} {
		l, oc, m := recordAgent(t, session, ownTurnTranscript(t, session, native, tc.inputEnd), map[string]string{"in-bg": native})
		r := openReader(t, session, l, oc, m, nil)
		mk, _, _ := m.Lookup("in-bg")
		if got, err := r.Recover(context.Background(), mk); err != nil || got.Outcome != tc.want {
			t.Errorf("Recover with the input's own end %v = %+v %v, want %s", tc.inputEnd, got, err, tc.want)
		}
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
	spool := sessionSpool(cfg.Spool, fixtureSession)
	if err := os.MkdirAll(spool, 0o700); err != nil {
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
		if err := os.WriteFile(filepath.Join(spool, name), data, 0o600); err != nil {
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
	if _, err := os.Stat(filepath.Join(spool, "pre-tool-use-1-1-1.json")); err != nil {
		t.Errorf("the tool hook's file went before its chunk was committed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spool, "session-start-1-1-2.json")); err == nil {
		t.Error("a file that reports nothing stays")
	}
	if err := r.Commit(ch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spool, "pre-tool-use-1-1-1.json")); err == nil {
		t.Error("the tool hook's file stays after its chunk was committed")
	}
}

// A Session's reader takes its own spool, and its own files at the spool
// root, which a host kept before each Session had a spool, its subagents'
// included; another Session's files it leaves to that Session: those in its
// spool where they are, those at the root moved into its spool.
func TestSpoolPerSession(t *testing.T) {
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", nil)
	cfg, _ := parseOpenConfig(oc)
	const other = "b2c3d4e5-0000-4000-8000-000000000001"
	tool := func(session, parent, id string) transcript.ParsedEvent {
		return transcript.ParsedEvent{HarnessSessionID: session, ParentSessionID: parent, Event: transcript.Event{
			Type: transcript.EventToolUse, Role: transcript.RoleAssistant, ToolName: "Bash", ToolUseID: id,
			ToolInput: json.RawMessage(`{"command":"echo hi"}`), Source: transcript.SourceHook,
		}}
	}
	files := map[string]transcript.ParsedEvent{
		filepath.Join(sessionSpool(cfg.Spool, fixtureSession), "pre-tool-use-1-1-1.json"): tool(fixtureSession, "", "toolu_own"),
		filepath.Join(sessionSpool(cfg.Spool, other), "pre-tool-use-1-1-2.json"):          tool(other, "", "toolu_other"),
		filepath.Join(cfg.Spool, "pre-tool-use-1-1-3.json"):                               tool(fixtureSession, "", "toolu_root"),
		filepath.Join(cfg.Spool, "pre-tool-use-1-1-4.json"):                               tool(other, "", "toolu_root_other"),
		filepath.Join(cfg.Spool, "pre-tool-use-1-1-5.json"):                               tool("agent-1", fixtureSession, "toolu_root_subagent"),
	}
	for path, ev := range files {
		data, err := transcript.MarshalParsedEvents([]transcript.ParsedEvent{ev})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := readAll(t, openReader(t, fixtureSession, l, oc, m, nil), contract.MaxObserveBytes)
	got := map[string]bool{}
	for _, o := range items {
		if o.Kind == contract.KindToolStarted {
			got[o.ID] = true
		}
	}
	for _, id := range []string{"toolu_own", "toolu_root", "toolu_root_subagent"} {
		if !got["tool_started:"+id] {
			t.Errorf("no tool_started of %s, the Session's", id)
		}
	}
	for _, id := range []string{"toolu_other", "toolu_root_other"} {
		if got["tool_started:"+id] {
			t.Errorf("tool_started of %s, another Session's", id)
		}
	}
	for path, ev := range files {
		_, err := os.Stat(path)
		atRoot := filepath.Dir(path) == cfg.Spool
		switch ours := ev.HarnessSessionID == fixtureSession || ev.ParentSessionID == fixtureSession; {
		case (ours || atRoot) && err == nil:
			t.Errorf("%s stays where it was", filepath.Base(path))
		case !ours && !atRoot && err != nil:
			t.Errorf("%s, another Session's, is gone: %v", filepath.Base(path), err)
		}
	}
	if got := spoolToolIDs(t, sessionSpool(cfg.Spool, other)); !got["toolu_other"] || !got["toolu_root_other"] || len(got) != 2 {
		t.Errorf("the other Session's spool holds %v, want its own file and the one moved from the root", got)
	}
}

// spoolToolIDs reads the tool use ids the spool in dir holds, consuming
// nothing.
func spoolToolIDs(t *testing.T, dir string) map[string]bool {
	t.Helper()
	sc, err := harnesscore.ReadSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sb := range sc.Batches {
		for _, pe := range sb.Events {
			got[pe.Event.ToolUseID] = true
		}
	}
	return got
}

// Every Session's reader polls the spool root at once. The root's files are
// moved into the Sessions' spools by one reader at a time: each Session gets
// exactly its own events — from a file holding several Sessions' events too,
// which is then acknowledged — and no Session is told of a file at the root
// it cannot be shown to own.
func TestLegacySpoolRootSharedBySessions(t *testing.T) {
	l, oc, m := recordAgent(t, fixtureSession, "../../transcript/claudecode/testdata/entries-2.1.283.jsonl", nil)
	cfg, _ := parseOpenConfig(oc)
	const other = "b2c3d4e5-0000-4000-8000-000000000001"
	tool := func(session, id string) transcript.ParsedEvent {
		return transcript.ParsedEvent{HarnessSessionID: session, Event: transcript.Event{
			Type: transcript.EventToolUse, Role: transcript.RoleAssistant, ToolName: "Bash", ToolUseID: id,
			ToolInput: json.RawMessage(`{"command":"echo hi"}`), Source: transcript.SourceHook,
		}}
	}
	root := map[string][]transcript.ParsedEvent{
		"pre-tool-use-1-1-1.json": {tool(fixtureSession, "toolu_a")},
		"pre-tool-use-2-1-2.json": {tool(other, "toolu_b")},
		"pre-tool-use-3-1-3.json": {tool(fixtureSession, "toolu_mixed_a"), tool(other, "toolu_mixed_b")},
	}
	for name, evs := range root {
		data, err := transcript.MarshalParsedEvents(evs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.Spool, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Spool, "pre-tool-use-4-1-4.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	sessions := []string{fixtureSession, other}
	readers := make([]adapter.Reader, len(sessions))
	for i, s := range sessions {
		readers[i] = openReader(t, s, l, oc, m, nil)
	}
	got := make([]map[string]int, len(sessions))
	faults := make([][]contract.Fault, len(sessions))
	var wg sync.WaitGroup
	for i, r := range readers {
		got[i] = map[string]int{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				ch, err := r.Read(context.Background(), contract.MaxObserveBytes)
				if err != nil {
					t.Error(err)
					return
				}
				for _, o := range ch.Items {
					if o.Kind == contract.KindToolStarted {
						got[i][o.ID]++
					}
				}
				faults[i] = append(faults[i], ch.Faults...)
				if err := r.Commit(ch); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	// A reader that found the root held may have finished its rounds before
	// the holder moved its files: what is left is in its spool by now.
	for i, r := range readers {
		ch, err := r.Read(context.Background(), contract.MaxObserveBytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range ch.Items {
			if o.Kind == contract.KindToolStarted {
				got[i][o.ID]++
			}
		}
		faults[i] = append(faults[i], ch.Faults...)
		if err := r.Commit(ch); err != nil {
			t.Fatal(err)
		}
	}
	want := []map[string]int{
		{"tool_started:toolu_a": 1, "tool_started:toolu_mixed_a": 1},
		{"tool_started:toolu_b": 1, "tool_started:toolu_mixed_b": 1},
	}
	for i := range sessions {
		if len(got[i]) != len(want[i]) {
			t.Errorf("session %d got %v, want %v", i, got[i], want[i])
		}
		for id, n := range want[i] {
			if got[i][id] != n {
				t.Errorf("session %d got %v, want %v", i, got[i], want[i])
				break
			}
		}
		for _, f := range faults[i] {
			if f.Kind == "spool_quarantined" {
				t.Errorf("session %d was told of a root file it does not own: %+v", i, f)
			}
		}
	}
	left, _ := filepath.Glob(filepath.Join(cfg.Spool, "*.json"))
	if len(left) != 0 {
		t.Errorf("files left at the root: %v", left)
	}
	if _, err := os.Stat(filepath.Join(cfg.Spool, harnesscore.SpoolQuarantineDir, "pre-tool-use-4-1-4.json")); err != nil {
		t.Errorf("the unreadable root file is not in the root's quarantine: %v", err)
	}
}

// A spool file is dispatched by the hook it was written under, read from its
// name in the current timestamp-first form and in the legacy event-first one;
// a failure's file is never taken for a success's, though its event name
// starts with the other's.
func TestSpoolItemsDispatchByHook(t *testing.T) {
	tool := transcript.ParsedEvent{HarnessSessionID: fixtureSession, Event: transcript.Event{
		Type: transcript.EventToolUse, ToolName: "Bash", ToolUseID: "toolu_1", Source: transcript.SourceHook,
	}}
	start := transcript.ParsedEvent{
		HarnessSessionID: "agent-1", ParentSessionID: fixtureSession,
		Event: transcript.Event{Type: transcript.EventSubagentStart, Source: transcript.SourceHook},
	}
	stop := transcript.ParsedEvent{
		HarnessSessionID: "agent-1", ParentSessionID: fixtureSession,
		Event: transcript.Event{Type: transcript.EventSubagentStop, Source: transcript.SourceHook},
	}
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, c := range []struct {
		event  string
		ev     transcript.ParsedEvent
		kind   contract.Kind
		failed bool
	}{
		{harnesscore.HookArgPreToolUse, tool, contract.KindToolStarted, false},
		{harnesscore.HookArgPostToolUse, tool, contract.KindToolFinished, false},
		{harnesscore.HookArgPostToolUseFailure, tool, contract.KindToolFinished, true},
		{harnesscore.HookArgSubagentStart, start, contract.KindSubagentStarted, false},
		{harnesscore.HookArgSubagentStop, stop, contract.KindSubagentStopped, false},
	} {
		for _, name := range []string{
			fmt.Sprintf("%020d-%s-%d-%d.json", time.Now().UnixNano(), c.event, 42, 1),
			c.event + "-1-1-1.json",
		} {
			items := spoolItems(harnesscore.SpoolBatch{
				Receipt: harnesscore.SpoolReceipt{Name: name, Digest: digest},
				Events:  []transcript.ParsedEvent{c.ev},
			})
			if len(items) != 1 || items[0].Kind != c.kind {
				t.Errorf("%s: items %+v, want one %s", name, items, c.kind)
				continue
			}
			if c.kind == contract.KindToolFinished {
				var d contract.ToolFinishedData
				if err := json.Unmarshal(items[0].Data, &d); err != nil || d.Failed != c.failed {
					t.Errorf("%s: finished %+v (%v), want failed=%v", name, d, err, c.failed)
				}
			}
		}
	}
	if items := spoolItems(harnesscore.SpoolBatch{
		Receipt: harnesscore.SpoolReceipt{Name: fmt.Sprintf("%020d-session-start-1-1.json", 1), Digest: digest},
		Events:  []transcript.ParsedEvent{tool},
	}); len(items) != 0 {
		t.Errorf("a session-start file reports %+v", items)
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

// thinkingSession is a session whose prompt (native id thinkingPrompt) claude
// answered after thinking: a thinking-only entry, then the text, both with
// stop_reason end_turn (claude 2.1.283/2.1.284).
const (
	thinkingSession = "a1f82cd3-6936-4ffd-8998-70b64b65dec9"
	thinkingPrompt  = "11111111-1111-4111-8111-111111111111"
	thinkingEntry   = "22222222-2222-4222-8222-222222222222"
	replyEntry      = "33333333-3333-4333-8333-333333333333"
)

func thinkingLine(uuid, parent, at, content string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"message":{"id":"msg_reply","type":"message","role":"assistant","content":[` + content + `],"stop_reason":"end_turn"},"type":"assistant","uuid":"` + uuid + `","timestamp":"` + at + `","sessionId":"` + thinkingSession + `","version":"2.1.283"}` + "\n"
}

var (
	thinkingLines = `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"REPORT"},"uuid":"` + thinkingPrompt + `","timestamp":"2026-10-08T18:05:00.000Z","sessionId":"` + thinkingSession + `","version":"2.1.283"}` + "\n" +
		thinkingLine(thinkingEntry, thinkingPrompt, "2026-10-08T18:05:04.000Z", `{"type":"thinking","thinking":"…","signature":"…"}`)
	replyLine = thinkingLine(replyEntry, thinkingEntry, "2026-10-08T18:05:34.000Z", `{"type":"text","text":"THE REPORT"}`)
	// claude's summary of the Stop hooks that ran after the reply: the
	// claude profile always has one.
	summaryLine = `{"parentUuid":"` + replyEntry + `","isSidechain":false,"type":"system","subtype":"stop_hook_summary","hookCount":1,"preventedContinuation":false,"uuid":"55555555-5555-4555-8555-555555555555","timestamp":"2026-10-08T18:05:34.100Z","sessionId":"` + thinkingSession + `","version":"2.1.283"}` + "\n"
	nextLine    = `{"parentUuid":"` + replyEntry + `","isSidechain":false,"type":"user","message":{"role":"user","content":"NEXT"},"uuid":"44444444-4444-4444-8444-444444444444","timestamp":"2026-10-08T18:06:00.000Z","sessionId":"` + thinkingSession + `","version":"2.1.283"}` + "\n"
)

// thinkingAgent lays out an agent whose Session's transcript holds lines, and
// returns the transcript's path for more.
func thinkingAgent(t *testing.T, lines string) (contract.Layout, []byte, *adapter.Markers, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(file, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	l, oc, m := recordAgent(t, thinkingSession, file, map[string]string{"in-report": thinkingPrompt})
	cfg, _ := parseOpenConfig(oc)
	f, err := claudecode.FollowEntries(thinkingSession, cfg.WorkingDir, cfg.Env, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	return l, oc, m, f.Path()
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// checkReply checks that the reply is the turn's, and that the turn ends once,
// completed, with the reply's text at the reply's entry.
func checkReply(t *testing.T, items []contract.Observation) {
	t.Helper()
	ends := 0
	for _, o := range items {
		switch o.Kind {
		case contract.KindAssistantText:
			if o.InputID != "in-report" || o.TurnID != adapter.TurnID("in-report") {
				t.Errorf("the reply is input %q turn %q, want in-report's", o.InputID, o.TurnID)
			}
		case contract.KindTurnEnded:
			ends++
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			if o.InputID != "in-report" || d.Outcome != contract.TurnCompleted || d.Text != "THE REPORT" || o.Entry != replyEntry {
				t.Errorf("the turn's end: input %q entry %s %+v, want in-report's, completed with the reply at its entry", o.InputID, o.Entry, d)
			}
		}
	}
	if ends != 1 {
		t.Errorf("%d turn ends, want 1", ends)
	}
}

// A reply after thinking is the turn's, and the turn's end carries it: the
// thinking entry's end_turn does not end the turn before its text, whether
// the text is read with it, a record at a time, after a later read, or by a
// reader reopened between them.
func TestReplyAfterThinking(t *testing.T) {
	t.Run("one read", func(t *testing.T) {
		l, oc, m, _ := thinkingAgent(t, thinkingLines+replyLine+summaryLine)
		items, _ := readAll(t, openReader(t, thinkingSession, l, oc, m, nil), contract.MaxObserveBytes)
		checkReply(t, items)
	})
	t.Run("a record at a time", func(t *testing.T) {
		l, oc, m, _ := thinkingAgent(t, thinkingLines+replyLine+summaryLine)
		items, _ := readAll(t, openReader(t, thinkingSession, l, oc, m, nil), 1)
		checkReply(t, items)
	})
	t.Run("text written later", func(t *testing.T) {
		l, oc, m, path := thinkingAgent(t, thinkingLines)
		r := openReader(t, thinkingSession, l, oc, m, nil)
		items, _ := readAll(t, r, contract.MaxObserveBytes)
		if ends := turnEnds(items); len(ends) != 0 {
			t.Fatalf("the turn ended before its text: %+v", ends)
		}
		appendLine(t, path, replyLine)
		more, _ := readAll(t, r, contract.MaxObserveBytes)
		items = append(items, more...)
		if ends := turnEnds(items); len(ends) != 0 {
			t.Fatalf("the turn ended before its Stop hooks ran: %+v", ends)
		}
		appendLine(t, path, summaryLine)
		more, _ = readAll(t, r, contract.MaxObserveBytes)
		checkReply(t, append(items, more...))
	})
	t.Run("reopened before the text", func(t *testing.T) {
		l, oc, m, path := thinkingAgent(t, thinkingLines)
		items, chunks := readAll(t, openReader(t, thinkingSession, l, oc, m, nil), contract.MaxObserveBytes)
		cp := chunks[len(chunks)-1].Checkpoint
		appendLine(t, path, replyLine+summaryLine)
		more, _ := readAll(t, openReader(t, thinkingSession, l, oc, m, cp), contract.MaxObserveBytes)
		checkReply(t, append(items, more...))
	})
}

// A message of thinking alone that ends the turn ends it at its entry, once
// an entry of another message shows no text follows.
func TestThinkingOnlyEnd(t *testing.T) {
	l, oc, m, _ := thinkingAgent(t, thinkingLines+nextLine)
	items, _ := readAll(t, openReader(t, thinkingSession, l, oc, m, nil), 1)
	var ended []contract.Observation
	for _, o := range items {
		if o.Kind == contract.KindTurnEnded {
			ended = append(ended, o)
		}
		if o.Kind == contract.KindUserInput && o.Entry != thinkingPrompt && o.InputID != "" {
			t.Errorf("the unmarked prompt is attributed to %q", o.InputID)
		}
	}
	if len(ended) != 1 || ended[0].InputID != "in-report" || ended[0].Entry != thinkingEntry {
		t.Fatalf("turn ends %+v, want in-report's at the thinking entry", ended)
	}
	if d := turnEnds(items)["in-report"]; d.Outcome != contract.TurnCompleted || d.Text != "" {
		t.Errorf("the end: %+v, want completed with no text", d)
	}
}

// The 2.1.283 fixture of a turn a Stop hook carried on: claude replied APPLE,
// the hook blocked the stop asking for BANANA, and claude replied BANANA in
// the same turn (one result, num_turns 2, in its stream).
const (
	stopHookSession  = "c3d555c7-136d-4098-b79d-6d0c79894666"
	stopHookPrompt   = "2d679f3d-7461-447c-bfa5-e63824065211"
	stopHookApple    = "0dcd34e3-9653-4d40-b6cc-1c9c9d88aaab"
	stopHookFeedback = "05670955-150d-4b9a-8df3-1d22f3f86cdd"
	stopHookBanana   = "20004e3a-6382-453a-8604-350c36e95640"
)

// checkStopHookTurn checks that both replies are the turn's, that the hook's
// feedback is no input, and that the turn ends once, with the last reply.
func checkStopHookTurn(t *testing.T, items []contract.Observation) {
	t.Helper()
	var ends []contract.Observation
	for _, o := range items {
		switch {
		case o.Entry == stopHookFeedback:
			t.Errorf("the Stop hook's feedback gave %s", o.ID)
		case o.Kind == contract.KindAssistantText && o.TurnID != adapter.TurnID("in-apple"):
			t.Errorf("the reply %s is turn %q, want in-apple's", o.Entry, o.TurnID)
		case o.Kind == contract.KindTurnEnded:
			ends = append(ends, o)
		}
	}
	if len(ends) != 1 {
		t.Fatalf("%d turn ends, want 1", len(ends))
	}
	var d contract.TurnEndedData
	_ = ends[0].Decode(&d)
	if ends[0].InputID != "in-apple" || d.Outcome != contract.TurnCompleted || d.Text != "BANANA" || ends[0].Entry != stopHookBanana {
		t.Errorf("the turn's end: input %q entry %s %+v, want in-apple's, completed with BANANA at its entry", ends[0].InputID, ends[0].Entry, d)
	}
}

// A Stop hook that blocks a turn's stop carries the turn on: the reply after
// its feedback is the turn's, and the turn ends with it, read whole, a record
// at a time, or as claude writes it.
func TestStopHookCarriesTheTurnOn(t *testing.T) {
	const fixture = "testdata/stophook-2.1.283.jsonl"
	inputs := map[string]string{"in-apple": stopHookPrompt}
	for _, max := range []int{contract.MaxObserveBytes, 1} {
		l, oc, m := recordAgent(t, stopHookSession, fixture, inputs)
		items, _ := readAll(t, openReader(t, stopHookSession, l, oc, m, nil), max)
		checkStopHookTurn(t, items)
	}

	lines := fixtureLines(t, fixture)
	l, oc, m := recordAgent(t, stopHookSession, fixture, inputs)
	cfg, _ := parseOpenConfig(oc)
	f, err := claudecode.FollowEntries(stopHookSession, cfg.WorkingDir, cfg.Env, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, stopHookSession, l, oc, m, nil)
	var items []contract.Observation
	for i, line := range lines {
		appendLine(t, f.Path(), line)
		more, _ := readAll(t, r, contract.MaxObserveBytes)
		items = append(items, more...)
		if ends := turnEnds(items); len(ends) != 0 && i < len(lines)-1 {
			t.Fatalf("the turn ended at line %d of %d: %+v", i+1, len(lines), ends)
		}
	}
	checkStopHookTurn(t, items)
}

// Recover takes a reply a Stop hook's feedback follows for no end of the
// turn: the turn went on, and only what follows says how it ended.
func TestRecoverPastStopHookFeedback(t *testing.T) {
	lines := fixtureLines(t, "testdata/stophook-2.1.283.jsonl")
	for _, c := range []struct {
		name  string
		lines int
		want  contract.RecoveredOutcome
	}{
		{"whole", len(lines), contract.RecoveredCompleted},
		{"after the first reply", 3, contract.RecoveredCompleted},
		{"after the hook's feedback", 5, contract.RecoveredUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "t.jsonl")
			if err := os.WriteFile(file, []byte(strings.Join(lines[:c.lines], "")), 0o600); err != nil {
				t.Fatal(err)
			}
			l, oc, m := recordAgent(t, stopHookSession, file, map[string]string{"in-apple": stopHookPrompt})
			mk, _, _ := m.Lookup("in-apple")
			got, err := openReader(t, stopHookSession, l, oc, m, nil).Recover(context.Background(), mk)
			if err != nil || got.Outcome != c.want {
				t.Errorf("Recover = %+v %v, want %s", got, err, c.want)
			}
		})
	}
}

// fixtureLines are a transcript fixture's lines, each with its newline.
func fixtureLines(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
