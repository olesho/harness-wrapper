package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	tpi "github.com/olesho/harness-wrapper/pkg/transcript/pi"
)

// The record is the session's file, followed from the checkpoint; pi writes
// it from the session's first user message on.
//
// An input's run begins at its user message, which the tag extension's entry
// names: the tag is the user message's nearest ancestor among the entries
// since the last message of the conversation — pi writes the tag first, then
// may compact the context or write a system message, then the user message.
// A tag pairs by parentId and never by order: pi writes the tag of an input
// it refuses, mid-run, with no user message after it. The tag's marker names
// the input.
//
// The user message becomes user_input; the run's answers assistant_text
// (keyed by entry id and block), its tool calls tool_use and tool_started,
// its tool results tool_result and tool_finished (keyed by call id); and its
// end a record-origin turn_ended. An answer that stops completes the run, and
// one aborted interrupts it. A failed one fails it unless pi retries the
// call, which it does after a context_edit that drops the failed answer: a
// failure is the run's end once pi gives up on it — a class it never retries,
// its last attempt — or once anything but that edit follows it. pi records an
// abort mid-tool as a failure, "The operation was aborted.": that one
// interrupts the run when the profile noted that it interrupted the input.
// An abort while pi waits to retry leaves the run at the dropped failure with
// nothing after it: the next input's user message ends it, interrupted when
// the profile noted that it interrupted the input, and without an end
// otherwise.
// A user message with no tag of a marker's is no input's, and so is what
// follows it.
type reader struct {
	session string
	cfg     openConfig
	scratch string
	// lookup is the input a tag is, from its marker.
	lookup  func(tag string) (inputID string, ok bool)
	from    transcript.Checkpoint // where to start once the file exists
	f       *transcript.Follower  // nil until it does
	rescan  *contract.Rescan
	cur     recordState // at the committed position
	pending *adapter.Chunk
}

// recordState is where the record is.
type recordState struct {
	// tags maps the id of each tag since the conversation's last message to
	// its tag, and links the id of each other entry since then to its
	// parent's.
	tags  map[string]string
	links map[string]string
	// input is the input the run is, and native its tag; "" when the run is
	// no input's.
	input, native string
	ended         bool
	// attempts counts the run's failed answers; failed is the latest, while
	// a context_edit may yet drop it, and retrying says the latest was
	// dropped, for a retry pi has not answered yet.
	attempts int
	failed   *failedAnswer
	retrying bool
}

// failedAnswer is a failed answer the run may yet retry.
type failedAnswer struct {
	id   string
	data contract.TurnEndedData
	at   time.Time
}

// clone is st with maps of its own, for a chunk's state after.
func (st recordState) clone() recordState {
	st.tags, st.links = maps.Clone(st.tags), maps.Clone(st.links)
	if st.tags == nil {
		st.tags = map[string]string{}
	}
	if st.links == nil {
		st.links = map[string]string{}
	}
	return st
}

// tagOf is the tag of a user message whose parent is parent: its nearest
// ancestor that is a tag, through the entries since the conversation's last
// message.
func (st recordState) tagOf(parent string) string {
	for range 256 {
		if tag, ok := st.tags[parent]; ok {
			return tag
		}
		next, ok := st.links[parent]
		if !ok {
			return ""
		}
		parent = next
	}
	return ""
}

// conversational reports whether an entry is a message of the conversation —
// the user's, the model's, a tool's — rather than an entry pi keeps beside it.
func conversational(e *tpi.Entry) bool {
	if e.Type != "message" {
		return false
	}
	switch e.Role {
	case "user", "assistant", "toolResult", "bashExecution":
		return true
	}
	return false
}

type chunkToken struct {
	batch transcript.Batch
	moved bool
	after recordState
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
	r := &reader{session: src.SessionID, cfg: cfg, scratch: src.Layout.Scratch, cur: recordState{}.clone()}
	r.lookup = func(tag string) (string, bool) {
		if src.Markers == nil {
			return "", false
		}
		mk, ok := src.Markers.ByNative(tag)
		return mk.InputID, ok && mk.SessionID == src.SessionID
	}
	if cp := src.Checkpoint; cp != nil {
		switch {
		case cp.Format != CheckpointFormat:
			r.rescan = &contract.Rescan{Reason: fmt.Sprintf("checkpoint format %d; this profile reads format %d", cp.Format, CheckpointFormat)}
		case json.Unmarshal(cp.Data, &r.from) != nil:
			r.rescan = &contract.Rescan{Reason: "the checkpoint does not parse"}
			r.from = transcript.Checkpoint{}
		}
	}
	if err := r.open(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, openFailed(contract.OpenConfigInvalid, "the session file: %v", err)
	}
	return r, nil
}

// open starts following the session's file once pi has written it.
func (r *reader) open() error {
	if r.f != nil {
		return nil
	}
	path, err := tpi.SessionFile(r.cfg.SessionDir, r.session)
	if err != nil {
		return err
	}
	f, err := tpi.FollowSession(path, r.session, r.from)
	if err != nil && r.rescan == nil && r.from != (transcript.Checkpoint{}) {
		r.rescan = &contract.Rescan{Reason: "the checkpoint is not one of this session's: " + err.Error()}
		r.from = transcript.Checkpoint{}
		f, err = tpi.FollowSession(path, r.session, r.from)
	}
	if err != nil {
		return err
	}
	r.f = f
	if r.from.Offset > 0 {
		r.cur = r.rebuild(path, r.from.Offset)
	}
	return nil
}

// rebuild reads the file up to offset for the state the record is in there.
func (r *reader) rebuild(path string, offset int64) recordState {
	st := recordState{}.clone()
	f, err := tpi.FollowSession(path, r.session, transcript.Checkpoint{})
	if err != nil {
		return st
	}
	f.MaxBatchBytes = 4 << 20
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
	err := r.open()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return adapter.Chunk{}, nil // pi has written no user message yet
	case err != nil:
		return adapter.Chunk{}, err
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
		tok.after = recordState{}.clone()
	case errors.Is(err, fs.ErrNotExist):
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
	r.cur = tok.after
	if tok.moved {
		return r.f.Ack(tok.batch)
	}
	return nil
}

func (r *reader) Close() error { return nil }

// items are the observations of events, read from state st, and the state
// after them. An entry's events are together, and its end is read once,
// from all of them.
func (r *reader) items(events []transcript.FollowedEvent, st recordState) ([]contract.Observation, recordState) {
	st = st.clone()
	var out []contract.Observation
	// stamp makes an item the input's, while its run lasts.
	stamp := func(o contract.Observation) contract.Observation {
		if st.input != "" && !st.ended {
			o.InputID, o.TurnID = st.input, adapter.TurnID(st.input)
		}
		return o
	}
	// end ends the run with data, as the record proves it.
	end := func(data contract.TurnEndedData, at time.Time) {
		if st.input != "" && !st.ended {
			out = append(out, stamp(contract.NewObservation(contract.KindTurnEnded, st.input, contract.OriginRecord, at, data)))
		}
		st.ended, st.failed = true, nil
	}
	for i := 0; i < len(events); {
		j := i + 1
		for j < len(events) && events[j].Offset == events[i].Offset {
			j++
		}
		entry := events[i:j]
		i = j
		e, _ := entry[0].Meta.(*tpi.Entry)
		if e == nil {
			continue
		}
		at := entry[0].Event.Timestamp
		if at.IsZero() {
			at = time.Now()
		}
		// A failed answer pi has not dropped is the run's end once anything
		// else follows it.
		if f := st.failed; f != nil {
			if e.Type == "context_edit" && e.TargetID == f.id {
				st.failed, st.retrying = nil, true
				continue
			}
			end(f.data, f.at)
		}
		switch {
		case e.Type == "custom" && e.Tag != "":
			st.tags[e.ID] = e.Tag
		case !conversational(e):
			st.links[e.ID] = e.ParentID
		case e.Role == "user":
			if st.retrying && hasInterruptNote(r.scratch, st.native) {
				end(contract.TurnEndedData{Outcome: contract.TurnInterrupted}, at)
			}
			tag := st.tagOf(e.ParentID)
			st.input, st.native, st.ended, st.attempts, st.retrying = "", "", false, 0, false
			if tag != "" {
				if input, ok := r.lookup(tag); ok {
					st.input, st.native = input, tag
				}
			}
			clear(st.tags)
			clear(st.links)
		default:
			st.retrying = false
			clear(st.tags)
			clear(st.links)
		}
		var answer []contentBlock
		for _, fe := range entry {
			ev := fe.Event
			fallback := "o" + strconv.FormatInt(fe.Offset, 10) + ":" + strconv.Itoa(fe.Block)
			switch {
			case ev.Type == transcript.EventText && e.Role == "user":
				text, cut := adapter.Truncate(ev.Text)
				o := stamp(contract.NewObservation(contract.KindUserInput, "u:"+e.ID, contract.OriginRecord, at, contract.TextData{Text: text}))
				o.Truncated = cut
				out = append(out, o)
			case ev.Type == transcript.EventText && e.Role == "assistant":
				answer = append(answer, contentBlock{Type: "text", Text: ev.Text})
				text, cut := adapter.Truncate(ev.Text)
				o := stamp(contract.NewObservation(contract.KindAssistantText, e.ID+":"+strconv.Itoa(fe.Block), contract.OriginRecord, at,
					contract.AssistantTextData{MessageID: e.ID, Text: text}))
				o.Truncated = cut
				out = append(out, o)
			case ev.Type == transcript.EventToolUse:
				input, cut := boundInput(ev.ToolInput)
				data := contract.ToolUseData{ToolUseID: ev.ToolUseID, Name: ev.ToolName, Input: input}
				for _, kind := range []contract.Kind{contract.KindToolUse, contract.KindToolStarted} {
					o := stamp(contract.NewObservation(kind, toolKey(ev.ToolUseID, fallback), contract.OriginRecord, at, data))
					o.Truncated = cut
					out = append(out, o)
				}
			case ev.Type == transcript.EventToolResult:
				text, cut := adapter.Truncate(ev.Output)
				for _, o := range []contract.Observation{
					contract.NewObservation(contract.KindToolResult, toolKey(ev.ToolUseID, fallback), contract.OriginRecord, at,
						contract.ToolResultData{ToolUseID: ev.ToolUseID, Output: text, IsError: e.IsError}),
					contract.NewObservation(contract.KindToolFinished, toolKey(ev.ToolUseID, fallback), contract.OriginRecord, at,
						contract.ToolFinishedData{ToolUseID: ev.ToolUseID, Name: ev.ToolName, Output: text, Failed: e.IsError}),
				} {
					o.Truncated = cut
					out = append(out, stamp(o))
				}
			}
		}
		if e.Role != "assistant" || st.ended {
			continue
		}
		switch e.StopReason {
		case "stop", "aborted", "length":
			content, _ := json.Marshal(answer)
			outcome, text, terr := ended(&assistantMessage{StopReason: e.StopReason, ErrorMessage: e.ErrorMessage, Content: content}, false, at)
			end(contract.TurnEndedData{Outcome: outcome, Text: text, Error: terr}, at)
		case "error":
			st.attempts++
			if e.ErrorMessage == abortedMessage && hasInterruptNote(r.scratch, st.native) {
				end(contract.TurnEndedData{Outcome: contract.TurnInterrupted}, at)
				continue
			}
			terr := failure(e.ErrorMessage, at)
			data := contract.TurnEndedData{Outcome: contract.TurnErrored, Error: terr}
			if final(terr.Class) || st.attempts > r.cfg.MaxRetries {
				end(data, at)
				continue
			}
			st.failed = &failedAnswer{id: e.ID, data: data, at: at}
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

// Recover reads the session for the input the marker names: its tag's user
// message, then its run's end. Without the user message — pi never took the
// input in, or the record stops before it says — or without an end the
// record proves, it cannot say.
func (r *reader) Recover(_ context.Context, m adapter.Marker) (contract.Recovered, error) {
	unknown := contract.Recovered{Outcome: contract.RecoveredUnknown}
	path, err := tpi.SessionFile(r.cfg.SessionDir, r.session)
	if err != nil {
		return unknown, nil
	}
	f, err := tpi.FollowSession(path, r.session, transcript.Checkpoint{})
	if err != nil {
		return unknown, nil
	}
	f.MaxBatchBytes = 4 << 20
	// Read as the reader does, with this marker's tag the only input.
	one := &reader{session: r.session, cfg: r.cfg, scratch: r.scratch, lookup: func(tag string) (string, bool) {
		return m.InputID, tag == m.Native
	}}
	st := recordState{}.clone()
	for {
		b, err := f.Poll()
		if err != nil || b.Checkpoint == b.From {
			return unknown, nil
		}
		var items []contract.Observation
		items, st = one.items(b.Events, st)
		for _, o := range items {
			if o.Kind != contract.KindTurnEnded || o.InputID != m.InputID {
				continue
			}
			var data contract.TurnEndedData
			if json.Unmarshal(o.Data, &data) != nil {
				return unknown, nil
			}
			switch data.Outcome {
			case contract.TurnCompleted:
				return contract.Recovered{Outcome: contract.RecoveredCompleted}, nil
			case contract.TurnInterrupted:
				return contract.Recovered{Outcome: contract.RecoveredInterrupted}, nil
			case contract.TurnErrored:
				return contract.Recovered{Outcome: contract.RecoveredErrored}, nil
			}
			return unknown, nil
		}
		if f.Ack(b) != nil {
			return unknown, nil
		}
	}
}

// The interrupt notes: a file per input the profile interrupted, written and
// synced before pi is asked to abort.
const notesDir = "pi-interrupts"

// noteInterrupt notes, durably, that the profile interrupted the input with
// tag native.
func noteInterrupt(scratch, native string) error {
	if !sessionIDRE.MatchString(native) {
		return fmt.Errorf("%q is no input tag", native)
	}
	dir := filepath.Join(scratch, notesDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, native), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// hasInterruptNote reports whether the profile noted that it interrupted the
// input with tag native.
func hasInterruptNote(scratch, native string) bool {
	if native == "" || !sessionIDRE.MatchString(native) {
		return false
	}
	_, err := os.Stat(filepath.Join(scratch, notesDir, native))
	return err == nil
}

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
