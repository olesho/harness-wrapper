package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	tcodex "github.com/olesho/harness-wrapper/pkg/transcript/codex"
)

// The record is the thread's rollout, followed from the checkpoint; codex
// writes it from the thread's first turn on.
//
// A turn begins at its task_started, and belongs to the input whose client id
// its user message carries — the id's marker names it. Its user message
// becomes user_input, its replies assistant_text (keyed by message id and
// block), its tool calls tool_use and tool_result (keyed by call id), and
// its end a record-origin turn_ended: interrupted at turn_aborted, errored at
// a task_complete with an error, completed at one with a reply. A
// task_complete with neither — how codex 0.144 records a failed turn —
// proves no outcome: it ends the turn, and the record ends no input's.
//
// A turn codex started itself, for the thread's goal, holds no user message:
// it opens with a message of codex's own (tcodex.KindGoalContext), or — one
// stopped before codex wrote that — with nothing. Its items name the turn
// (adapter.AutoTurnID of codex's turn id) and no input, and so does its
// turn_ended. Should such a turn take an input in — codex folds one sent
// while a turn runs into that turn — codex's own turn ends there,
// interrupted, and the rest is the input's.
type reader struct {
	session string
	cfg     openConfig
	markers *adapter.Markers
	from    transcript.Checkpoint // where to start once the rollout exists
	f       *transcript.Follower  // nil until it does
	rescan  *contract.Rescan
	cur     recordState // at the committed position
	pending *adapter.Chunk
}

// recordState is where the record is: the turn it is in, the input that turn
// is, and whether the turn ended.
type recordState struct {
	turnID string
	input  string
	// user: the turn holds a user message. auto: it is known to be one codex
	// started itself — it opened with codex's message to itself, or said
	// something before any user message.
	user, auto bool
	ended      bool
}

// own is the turn id of the turn the record is in, when that is known to be
// one codex started itself, and it runs still.
func (st recordState) own() string {
	if st.auto && !st.ended && st.input == "" && st.turnID != "" {
		return adapter.AutoTurnID(st.turnID)
	}
	return ""
}

// said notes that the turn produced something: before any user message, that
// makes it codex's own.
func (st *recordState) said() {
	if !st.user && !st.ended && st.input == "" && st.turnID != "" {
		st.auto = true
	}
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
	r := &reader{session: src.SessionID, cfg: cfg, markers: src.Markers}
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
		return nil, openFailed(contract.OpenConfigInvalid, "the rollout: %v", err)
	}
	return r, nil
}

// open starts following the rollout once codex has written it.
func (r *reader) open() error {
	if r.f != nil {
		return nil
	}
	path, err := tcodex.Rollout(r.cfg.CodexHome, r.session)
	if err != nil {
		return err
	}
	f, err := tcodex.FollowRollout(path, r.session, r.from)
	if err != nil && r.rescan == nil && r.from != (transcript.Checkpoint{}) {
		r.rescan = &contract.Rescan{Reason: "the checkpoint is not one of this thread's: " + err.Error()}
		r.from = transcript.Checkpoint{}
		f, err = tcodex.FollowRollout(path, r.session, r.from)
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

// rebuild reads the rollout up to offset for the state the record is in
// there.
func (r *reader) rebuild(path string, offset int64) recordState {
	f, err := tcodex.FollowRollout(path, r.session, transcript.Checkpoint{})
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
	err := r.open()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return adapter.Chunk{}, nil // codex has not written its first turn yet
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
		tok.after = recordState{}
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
// after them.
func (r *reader) items(events []transcript.FollowedEvent, st recordState) ([]contract.Observation, recordState) {
	var out []contract.Observation
	for _, fe := range events {
		e, _ := fe.Meta.(*tcodex.Entry)
		if e == nil {
			continue
		}
		ev := fe.Event
		at := ev.Timestamp
		if at.IsZero() {
			at = time.Now()
		}
		// Items belong to the input whose turn the record is in; after that
		// turn's end, to none, until the next turn of an input with a marker.
		stamp := func(o contract.Observation) contract.Observation {
			switch {
			case st.input != "" && !st.ended:
				o.InputID, o.TurnID = st.input, adapter.TurnID(st.input)
			case st.own() != "":
				o.TurnID = st.own()
			}
			return o
		}
		fallback := "o" + strconv.FormatInt(fe.Offset, 10) + ":" + strconv.Itoa(fe.Block)
		switch {
		case e.Kind == "task_started":
			st = recordState{turnID: e.TurnID}
		case e.Kind == tcodex.KindGoalContext:
			if st.turnID == "" && !st.ended {
				st.turnID = e.TurnID
			}
			st.said()
		case ev.Type == transcript.EventText && ev.Role == transcript.RoleUser:
			st.user = true
			key := fallback
			if e.ClientID != "" {
				key = "u:" + e.ClientID
				if mk, ok := r.markers.ByNative(e.ClientID); ok && mk.SessionID == r.session {
					if own := st.own(); own != "" {
						// codex took the input into a turn of its own: that
						// turn ends here, and the rest is the input's.
						o := contract.NewObservation(contract.KindTurnEnded, own, contract.OriginRecord, at, contract.TurnEndedData{Outcome: contract.TurnInterrupted})
						o.TurnID = own
						out = append(out, o)
					}
					st.input, st.ended, st.auto = mk.InputID, false, false
					if st.turnID == "" {
						st.turnID = e.TurnID
					}
				}
			}
			text, cut := adapter.Truncate(ev.Text)
			o := stamp(contract.NewObservation(contract.KindUserInput, key, contract.OriginRecord, at, contract.TextData{Text: text}))
			o.Truncated = cut
			out = append(out, o)
		case ev.Type == transcript.EventText && ev.Role == transcript.RoleAssistant:
			st.said()
			key := fallback
			if e.MessageID != "" {
				key = e.MessageID + ":" + strconv.Itoa(fe.Block)
			}
			text, cut := adapter.Truncate(ev.Text)
			o := stamp(contract.NewObservation(contract.KindAssistantText, key, contract.OriginRecord, at,
				contract.AssistantTextData{MessageID: e.MessageID, Text: text}))
			o.Truncated = cut
			out = append(out, o)
		case ev.Type == transcript.EventToolUse:
			st.said()
			input, cut := boundInput(ev.ToolInput)
			o := stamp(contract.NewObservation(contract.KindToolUse, toolKey(ev.ToolUseID, fallback), contract.OriginRecord, at,
				contract.ToolUseData{ToolUseID: ev.ToolUseID, Name: ev.ToolName, Input: input}))
			o.Truncated = cut
			out = append(out, o)
		case ev.Type == transcript.EventToolResult:
			st.said()
			text, cut := adapter.Truncate(ev.Output)
			o := stamp(contract.NewObservation(contract.KindToolResult, toolKey(ev.ToolUseID, fallback), contract.OriginRecord, at,
				contract.ToolResultData{ToolUseID: ev.ToolUseID, Output: text}))
			o.Truncated = cut
			out = append(out, o)
		case e.Kind == "task_complete" || e.Kind == "turn_aborted":
			if st.ended || e.TurnID != "" && st.turnID != "" && e.TurnID != st.turnID {
				continue
			}
			// A turn that ends with no user message in it is codex's own,
			// whatever it said.
			st.said()
			if data, ok := ended(e, at); ok {
				switch own := st.own(); {
				case st.input != "":
					out = append(out, stamp(contract.NewObservation(contract.KindTurnEnded, st.input, contract.OriginRecord, at, data)))
				case own != "":
					out = append(out, stamp(contract.NewObservation(contract.KindTurnEnded, own, contract.OriginRecord, at, data)))
				}
			}
			st.ended = true
		}
	}
	return out, st
}

// ended is how the turn e ends ended, when the record proves it.
func ended(e *tcodex.Entry, at time.Time) (contract.TurnEndedData, bool) {
	switch {
	case e.Kind == "turn_aborted":
		return contract.TurnEndedData{Outcome: contract.TurnInterrupted}, true
	case e.ErrorInfo != "":
		return contract.TurnEndedData{
			Outcome: contract.TurnErrored,
			Error:   failure(turnError{Message: e.ErrorMessage, CodexErrorInfo: jsonString(e.ErrorInfo)}, nil, at),
		}, true
	case e.LastAgentMessage != nil:
		text, _ := adapter.Truncate(*e.LastAgentMessage)
		return contract.TurnEndedData{Outcome: contract.TurnCompleted, Text: text}, true
	}
	return contract.TurnEndedData{}, false
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
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

// Recover reads the rollout for the input the marker names: its user
// message, then the end of its turn. Without the message, or without an end
// that proves an outcome, the record cannot say.
func (r *reader) Recover(_ context.Context, m adapter.Marker) (contract.Recovered, error) {
	unknown := contract.Recovered{Outcome: contract.RecoveredUnknown}
	path, err := tcodex.Rollout(r.cfg.CodexHome, r.session)
	if err != nil {
		return unknown, nil
	}
	f, err := tcodex.FollowRollout(path, r.session, transcript.Checkpoint{})
	if err != nil {
		return unknown, nil
	}
	f.MaxBatchBytes = 4 << 20
	turn, found := "", false
	for {
		b, err := f.Poll()
		if err != nil || b.Checkpoint == b.From {
			return unknown, nil
		}
		for _, fe := range b.Events {
			e, _ := fe.Meta.(*tcodex.Entry)
			if e == nil {
				continue
			}
			switch {
			case e.Kind == "task_started":
				if found {
					// The next turn began, and nothing said how this one ended.
					return unknown, nil
				}
				turn = e.TurnID
			case e.ClientID != "" && e.ClientID == m.Native:
				found = true
				if turn == "" {
					turn = e.TurnID
				}
			case found && (e.Kind == "task_complete" || e.Kind == "turn_aborted") && (e.TurnID == "" || turn == "" || e.TurnID == turn):
				data, ok := ended(e, time.Now())
				if !ok {
					return unknown, nil
				}
				switch data.Outcome {
				case contract.TurnInterrupted:
					return contract.Recovered{Outcome: contract.RecoveredInterrupted}, nil
				case contract.TurnErrored:
					return contract.Recovered{Outcome: contract.RecoveredErrored}, nil
				}
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
