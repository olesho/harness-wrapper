package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// The record is claude's session transcript, followed from the checkpoint,
// and the Session's hook spool.
//
// Transcript entries become user_input, assistant_text, tool_use,
// tool_result and api_error, keyed by the entry's uuid (and block) or the
// tool use id. The record tells which input a turn belongs to — the prompt
// entry's uuid is the input's native id — and how it ended: an assistant
// entry with stop_reason end_turn, a synthetic API-error entry, or an
// interrupt entry, each a record-origin turn_ended. Hook spool files become
// tool_started, tool_finished and the subagents' start and stop.
type reader struct {
	session string
	cfg     openConfig
	// spool is the Session's own hook spool; "" for an id that names none.
	spool   string
	markers *adapter.Markers
	f       *transcript.Follower
	rescan  *contract.Rescan
	cur     recordState // at the committed position
	pending *adapter.Chunk
}

// recordState is where the record is: the input whose turn it is in, and
// whether that turn's end is in it yet.
type recordState struct {
	input string
	// own is the turn claude started itself to take background work up
	// (background_turns), when the record is in one.
	own   string
	ended bool
}

type chunkToken struct {
	batch transcript.Batch
	moved bool
	reset bool
	// receipts acknowledge the files of the Session's spool.
	receipts []harnesscore.SpoolReceipt
	after    recordState
}

func checkpointOf(cp transcript.Checkpoint) *contract.Checkpoint {
	b, _ := json.Marshal(cp)
	return &contract.Checkpoint{Format: CheckpointFormat, Data: b}
}

// Record opens the reader of a Session's record. A checkpoint it cannot read
// makes it read from the start, reporting a rescan.
func (Profile) Record(src adapter.RecordSource) (adapter.Reader, error) {
	cfg, err := parseOpenConfig(src.OpenConfig)
	if err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "%v", err)
	}
	r := &reader{session: src.SessionID, cfg: cfg, markers: src.Markers}
	if sessionid.IsUUID(src.SessionID) {
		r.spool = sessionSpool(cfg.Spool, src.SessionID)
	}
	var from transcript.Checkpoint
	if cp := src.Checkpoint; cp != nil {
		switch {
		case cp.Format != CheckpointFormat:
			r.rescan = &contract.Rescan{Reason: fmt.Sprintf("checkpoint format %d; this profile reads format %d", cp.Format, CheckpointFormat)}
		case json.Unmarshal(cp.Data, &from) != nil:
			r.rescan = &contract.Rescan{Reason: "the checkpoint does not parse"}
			from = transcript.Checkpoint{}
		}
	}
	r.f, err = claudecode.FollowEntries(src.SessionID, cfg.WorkingDir, cfg.Env, from)
	if err != nil && r.rescan == nil && from != (transcript.Checkpoint{}) {
		r.rescan = &contract.Rescan{Reason: "the checkpoint is not one of this session's: " + err.Error()}
		from = transcript.Checkpoint{}
		r.f, err = claudecode.FollowEntries(src.SessionID, cfg.WorkingDir, cfg.Env, from)
	}
	if err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "the transcript: %v", err)
	}
	if from.Offset > 0 {
		r.cur = r.rebuild(from.Offset)
	}
	return r, nil
}

// rebuild reads the transcript up to offset for the state the record is in
// there.
func (r *reader) rebuild(offset int64) recordState {
	f, err := claudecode.FollowEntries(r.session, r.cfg.WorkingDir, r.cfg.Env, transcript.Checkpoint{})
	if err != nil {
		return recordState{}
	}
	f.MaxBatchBytes = 4 << 20
	st := recordState{}
	for {
		b, err := f.Poll()
		if err != nil || b.Checkpoint == b.From {
			return st
		}
		var events []transcript.FollowedEvent
		for _, fe := range b.Events {
			if fe.Offset < offset {
				events = append(events, fe)
			}
		}
		_, st = r.items(events, st)
		if b.Checkpoint.Offset >= offset || f.Ack(b) != nil {
			return st
		}
	}
}

func (r *reader) Read(_ context.Context, maxBytes int) (adapter.Chunk, error) {
	if r.pending != nil {
		return *r.pending, nil
	}
	tok := &chunkToken{after: r.cur}
	ch := adapter.Chunk{Rescan: r.rescan, Token: tok}
	r.f.MaxBatchBytes = max(1, maxBytes/2)
	b, err := r.f.Poll()
	var reset *transcript.ResetError
	switch {
	case errors.As(err, &reset):
		ch.Reset = &contract.Reset{Reason: string(reset.Reason), Previous: checkpointOf(reset.Previous)}
		ch.Checkpoint = checkpointOf(r.f.Checkpoint())
		tok.reset, tok.after = true, recordState{}
	case errors.Is(err, fs.ErrNotExist):
		// claude has not written its first entry yet
	case err != nil:
		return adapter.Chunk{}, err
	default:
		ch.Items, tok.after = r.items(b.Events, r.cur)
		for _, se := range b.Errors {
			ch.Faults = append(ch.Faults, contract.Fault{Kind: "unreadable_entry", Detail: capText(se.Error(), 1024)})
		}
		if b.Checkpoint != b.From {
			ch.Checkpoint = checkpointOf(b.Checkpoint)
			tok.batch, tok.moved = b, true
		}
	}

	migrateLegacySpool(r.cfg.Spool)
	if r.spool != "" {
		tok.receipts = r.readSpool(&ch, r.spool)
	}
	if len(ch.Items) == 0 && ch.Checkpoint == nil && ch.Reset == nil && ch.Rescan == nil && len(ch.Faults) == 0 {
		return adapter.Chunk{}, nil
	}
	r.pending = &ch
	return ch, nil
}

func (r *reader) Commit(c adapter.Chunk) error {
	tok, ok := c.Token.(*chunkToken)
	if !ok {
		return nil
	}
	r.pending = nil
	r.rescan = nil
	var errs []error
	if tok.moved {
		errs = append(errs, r.f.Ack(tok.batch))
	}
	r.cur = tok.after
	if len(tok.receipts) > 0 {
		errs = append(errs, harnesscore.AckSpool(r.spool, tok.receipts...))
	}
	return errors.Join(errs...)
}

// readSpool adds what the Session's own spool in dir reports to ch, and
// returns the receipts that acknowledge it. A file that reports nothing goes
// at once: it has nothing to commit.
func (r *reader) readSpool(ch *adapter.Chunk, dir string) []harnesscore.SpoolReceipt {
	sc, _ := harnesscore.ReadSpool(dir)
	var nothing, taken []harnesscore.SpoolReceipt
	for _, sb := range sc.Batches {
		items := spoolItems(sb)
		if len(items) == 0 {
			nothing = append(nothing, sb.Receipt)
			continue
		}
		ch.Items = append(ch.Items, items...)
		taken = append(taken, sb.Receipt)
	}
	_ = harnesscore.AckSpool(dir, nothing...)
	for _, q := range sc.Quarantined {
		ch.Faults = append(ch.Faults, contract.Fault{Kind: "spool_quarantined", Detail: capText(q.Name+": "+q.Reason, 1024)})
	}
	return taken
}

// legacySpoolLock is the lock file, at the spool root, that one reader at a
// time holds to move the root's files into the Sessions' own spools.
const legacySpoolLock = "spool-legacy.lock"

// migrateLegacySpool moves the hook files a host kept at the spool root —
// before each Session had a spool of its own — into the spools of the
// Sessions they belong to, where each Session's reader takes its own.
//
// The root is shared by every Session of the agent, and ReadSpool assumes one
// consumer. When each reader took its own files straight from the root, the
// readers raced to quarantine the same files, every Session got the faults of
// files that were not its own, and a file holding several Sessions' events
// was never acknowledged by any. So the root is handled under an flock, taken
// without waiting: a reader that finds it held skips the root this time,
// since the holder is moving the files anyway. Under it, each file's events
// are split by the Session they belong to (the event's own, or its parent's
// for a subagent's), written durably to that Session's spool under the same
// hook and time, and only then is the root file acknowledged. A crash in
// between writes a Session's part again, which its reader dedups by
// observation id. Events of no claude Session (no UUID) can reach no reader
// and go with their file, as does a file that reports nothing.
//
// A file the root's ReadSpool quarantines cannot be told to be any Session's,
// so it is reported to none: it stays in the root's quarantine for
// inspection. Each Session's own spool reports its own.
func migrateLegacySpool(root string) {
	lf, err := os.OpenFile(filepath.Join(root, legacySpoolLock), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the agent's scratch root
	if err != nil {
		return
	}
	defer func() { _ = lf.Close() }()
	if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return // another reader holds the root
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	sc, _ := harnesscore.ReadSpool(root)
	var moved []harnesscore.SpoolReceipt
	for _, sb := range sc.Batches {
		if moveLegacyFile(root, sb) == nil {
			moved = append(moved, sb.Receipt)
		}
	}
	_ = harnesscore.AckSpool(root, moved...)
}

// moveLegacyFile writes the events of one root spool file to the spools of
// the Sessions they belong to. A nil return means the root file may go.
func moveLegacyFile(root string, sb harnesscore.SpoolBatch) error {
	if len(spoolItems(sb)) == 0 {
		return nil
	}
	event, nanos, ok := harnesscore.ParseSpoolFileName(sb.Receipt.Name)
	if !ok {
		return nil // spoolItems reports nothing for such a name
	}
	var order []string
	bySession := map[string][]transcript.ParsedEvent{}
	for _, pe := range sb.Events {
		top := pe.HarnessSessionID
		if pe.ParentSessionID != "" {
			top = pe.ParentSessionID
		}
		if !sessionid.IsUUID(top) {
			continue
		}
		if _, seen := bySession[top]; !seen {
			order = append(order, top)
		}
		bySession[top] = append(bySession[top], pe)
	}
	for _, id := range order {
		dir := sessionSpool(root, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := harnesscore.WriteSpoolFile(dir, event, nanos, bySession[id]); err != nil {
			return err
		}
	}
	return nil
}

func (r *reader) Close() error { return nil }

// entryRun is one entry's consecutive events.
type entryRun struct {
	entry  *claudecode.Entry
	events []transcript.FollowedEvent
}

func runs(events []transcript.FollowedEvent) []entryRun {
	var out []entryRun
	for _, fe := range events {
		e, _ := fe.Meta.(*claudecode.Entry)
		if e == nil {
			continue
		}
		if n := len(out); n > 0 && out[n-1].entry == e {
			out[n-1].events = append(out[n-1].events, fe)
			continue
		}
		out = append(out, entryRun{entry: e, events: []transcript.FollowedEvent{fe}})
	}
	return out
}

// items are the observations of events, read from state st, and the state
// after them.
func (r *reader) items(events []transcript.FollowedEvent, st recordState) ([]contract.Observation, recordState) {
	var out []contract.Observation
	for _, run := range runs(events) {
		e := run.entry
		if e.Sidechain {
			continue
		}
		if e.Type == transcript.TypeUser && e.UUID != "" {
			if mk, ok := r.markers.ByNative(e.UUID); ok && mk.SessionID == r.session {
				st = recordState{input: mk.InputID}
			}
		}
		if e.TaskNotification != "" {
			// claude tells the model background work ended: the turn it
			// takes that up in is its own.
			st = recordState{own: adapter.AutoTurnID(ownNative(e.TaskNotification))}
		}
		// Items belong to the input whose turn the record is in; after that
		// turn's end, to none, until the next prompt of an input with a
		// marker. A prompt without one — sent by another host — never takes
		// an ended turn's input.
		stamp := func(o contract.Observation) contract.Observation {
			o.Entry = e.UUID
			switch {
			case st.ended:
			case st.input != "":
				o.InputID, o.TurnID = st.input, adapter.TurnID(st.input)
			case st.own != "":
				o.TurnID = st.own
			}
			return o
		}
		end := func(at time.Time, data contract.TurnEndedData) {
			switch {
			case st.ended:
				return
			case st.input != "":
				out = append(out, stamp(contract.NewObservation(contract.KindTurnEnded, st.input, contract.OriginRecord, at, data)))
			case st.own != "":
				out = append(out, stamp(contract.NewObservation(contract.KindTurnEnded, st.own, contract.OriginRecord, at, data)))
			default:
				return
			}
			st.ended = true
		}
		var reply []string
		var at time.Time
		for _, fe := range run.events {
			ev := fe.Event
			at = ev.Timestamp
			if at.IsZero() {
				at = time.Now()
			}
			key := e.UUID + ":" + strconv.Itoa(fe.Block)
			switch {
			case ev.Type == transcript.EventText && ev.Role == transcript.RoleUser && e.TaskNotification != "":
				// What claude told the model, not an input.
			case ev.Type == transcript.EventText && ev.Role == transcript.RoleUser:
				text, cut := adapter.Truncate(ev.Text)
				o := stamp(contract.NewObservation(contract.KindUserInput, key, contract.OriginRecord, at, contract.TextData{Text: text}))
				o.Truncated = cut
				out = append(out, o)
			case ev.Type == transcript.EventToolUse:
				input, cut := boundInput(ev.ToolInput)
				o := stamp(contract.NewObservation(contract.KindToolUse, toolKey(ev.ToolUseID, key), contract.OriginRecord, at,
					contract.ToolUseData{ToolUseID: ev.ToolUseID, Name: ev.ToolName, Input: input}))
				o.Truncated = cut
				out = append(out, o)
			case ev.Type == transcript.EventToolResult:
				text, cut := adapter.Truncate(ev.Output)
				o := stamp(contract.NewObservation(contract.KindToolResult, toolKey(ev.ToolUseID, key), contract.OriginRecord, at,
					contract.ToolResultData{ToolUseID: ev.ToolUseID, Output: text}))
				o.Truncated = cut
				out = append(out, o)
			case ev.Type == transcript.EventText && ev.Role == transcript.RoleAssistant && ev.APIError != "":
				fl := failure{tag: ev.APIError, status: e.APIErrorStatus, text: ev.Text}
				text, cut := adapter.Truncate(ev.Text)
				o := stamp(contract.NewObservation(contract.KindAPIError, e.UUID, contract.OriginRecord, at,
					contract.APIErrorData{Class: fl.errorClass(), HTTPStatus: e.APIErrorStatus, Message: text}))
				o.Truncated = cut
				out = append(out, o)
				end(at, contract.TurnEndedData{Outcome: contract.TurnErrored, Error: fl.turnError(at)})
			case ev.Type == transcript.EventText && ev.Role == transcript.RoleAssistant:
				text, cut := adapter.Truncate(ev.Text)
				o := stamp(contract.NewObservation(contract.KindAssistantText, key, contract.OriginRecord, at,
					contract.AssistantTextData{MessageID: e.MessageID, Text: text}))
				o.Truncated = cut
				out = append(out, o)
				reply = append(reply, ev.Text)
			}
		}
		switch {
		case e.Interrupt:
			end(at, contract.TurnEndedData{Outcome: contract.TurnInterrupted})
		case e.StopReason == "end_turn" && e.APIError == "":
			text, _ := adapter.Truncate(strings.Join(reply, ""))
			end(at, contract.TurnEndedData{Outcome: contract.TurnCompleted, Text: text})
		}
	}
	return out, st
}

func toolKey(toolUseID, fallback string) string {
	if toolUseID != "" {
		return toolUseID
	}
	return fallback
}

// boundInput keeps a tool's input within an observation's text bound: one
// over it is replaced by a JSON string holding the cut of its text.
func boundInput(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) <= contract.MaxObservationText {
		return raw, false
	}
	cut, _ := adapter.Truncate(string(raw))
	b, _ := json.Marshal(cut)
	return b, true
}

// spoolItems are the observations one hook spool file reports, by the hook it
// was written under (harnesscore.ParseSpoolFileName). Other files — session markers, the
// prompt, Stop's and SessionEnd's copy of the transcript — report nothing.
func spoolItems(sb harnesscore.SpoolBatch) []contract.Observation {
	name := sb.Receipt.Name
	digest := strings.TrimPrefix(sb.Receipt.Digest, "sha256:")
	if len(digest) > 24 {
		digest = digest[:24]
	}
	var out []contract.Observation
	tool := func(kind contract.Kind, failed, finished bool) {
		for i, pe := range sb.Events {
			ev := pe.Event
			if ev.ToolName == "" {
				continue
			}
			at := ev.Timestamp
			if at.IsZero() {
				at = time.Now()
			}
			key := toolKey(ev.ToolUseID, digest+":"+strconv.Itoa(i))
			var data any
			cut := false
			if finished {
				out1, c := adapter.Truncate(ev.Output)
				cut = c
				data = contract.ToolFinishedData{ToolUseID: ev.ToolUseID, Name: ev.ToolName, Output: out1, Failed: failed}
			} else {
				in, c := boundInput(ev.ToolInput)
				cut = c
				data = contract.ToolUseData{ToolUseID: ev.ToolUseID, Name: ev.ToolName, Input: in}
			}
			o := contract.NewObservation(kind, key, contract.OriginRecord, at, data)
			o.Truncated = cut
			out = append(out, o)
		}
	}
	subagent := func(kind contract.Kind, marker string) {
		for _, pe := range sb.Events {
			if pe.Event.Type != marker || pe.HarnessSessionID == "" {
				continue
			}
			at := pe.Event.Timestamp
			if at.IsZero() {
				at = time.Now()
			}
			data := contract.SubagentData{SubagentID: pe.HarnessSessionID, Type: capText(pe.Event.AgentType, 256)}
			if kind == contract.KindSubagentStopped {
				data.LastMessage, _ = adapter.Truncate(pe.Event.Text)
			}
			out = append(out, contract.NewObservation(kind, pe.HarnessSessionID, contract.OriginRecord, at, data))
		}
	}
	// The hook is read from the name by harnesscore, which knows both the
	// current timestamp-first form and the legacy event-first one.
	event, _, _ := harnesscore.ParseSpoolFileName(name)
	switch event {
	case harnesscore.HookArgPreToolUse:
		tool(contract.KindToolStarted, false, false)
	case harnesscore.HookArgPostToolUseFailure:
		tool(contract.KindToolFinished, true, true)
	case harnesscore.HookArgPostToolUse:
		tool(contract.KindToolFinished, false, true)
	case harnesscore.HookArgSubagentStart:
		subagent(contract.KindSubagentStarted, transcript.EventSubagentStart)
	case harnesscore.HookArgSubagentStop:
		subagent(contract.KindSubagentStopped, transcript.EventSubagentStop)
	}
	return out
}

// Recover reads the transcript for the input the marker names: its prompt
// entry, then the evidence of its turn's end. Without the entry, or without
// the evidence, the record cannot say.
func (r *reader) Recover(_ context.Context, m adapter.Marker) (contract.Recovered, error) {
	unknown := contract.Recovered{Outcome: contract.RecoveredUnknown}
	f, err := claudecode.FollowEntries(r.session, r.cfg.WorkingDir, r.cfg.Env, transcript.Checkpoint{})
	if err != nil {
		return unknown, nil
	}
	f.MaxBatchBytes = 4 << 20
	found := false
	for {
		b, err := f.Poll()
		if err != nil || b.Checkpoint == b.From {
			return unknown, nil
		}
		for _, run := range runs(b.Events) {
			e := run.entry
			if e.Sidechain {
				continue
			}
			if e.Type == transcript.TypeUser && e.UUID == m.Native {
				found = true
				continue
			}
			if !found {
				continue
			}
			if e.Type == transcript.TypeUser && e.UUID != "" {
				if _, ours := r.markers.ByNative(e.UUID); ours {
					// The next input began, and nothing said how this one ended.
					return unknown, nil
				}
			}
			switch {
			case e.Interrupt:
				return contract.Recovered{Outcome: contract.RecoveredInterrupted}, nil
			case e.APIError != "":
				return contract.Recovered{Outcome: contract.RecoveredErrored}, nil
			case e.StopReason == "end_turn":
				return contract.Recovered{Outcome: contract.RecoveredCompleted}, nil
			}
		}
		if f.Ack(b) != nil {
			return unknown, nil
		}
	}
}

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ownNative is the native id of the turn claude starts itself to take up
// background work task ended: live, from the task_notification frame before
// it; in the record, from the notification's entry.
func ownNative(task string) string { return "task-" + task }
