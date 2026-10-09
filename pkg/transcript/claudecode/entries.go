package claudecode

import (
	"encoding/json"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Entry is what a Claude Code transcript entry says beyond its events: the
// facts a turn's input and its end are read from. FollowEntries attaches one
// to each event as its Meta.
type Entry struct {
	// Type is the entry's type: user, assistant, system, attachment, ….
	Type string
	// UUID is the entry's own id. A user entry holding a prompt claude took
	// over stream-json has the message's uuid (claude 2.1.283).
	UUID string
	// MessageID is an assistant entry's API message id; one message spans
	// several entries, one per content block.
	MessageID string
	// StopReason is an assistant entry's stop_reason: end_turn on the entry
	// whose message ended its turn.
	StopReason string
	// APIError is a synthetic API-error entry's tag (Line.Error), and
	// APIErrorStatus its HTTP status.
	APIError       string
	APIErrorStatus int
	// Interrupt marks a user entry recording an interrupt: "[Request
	// interrupted by user]", or "… for tool use]".
	Interrupt bool
	// Sidechain marks an entry of a subagent's conversation.
	Sidechain bool
	// TaskNotification is set on the user entry claude writes to tell the
	// model background work ended (origin kind task-notification): the
	// first task id it names, or "unknown". The turn it starts is claude's
	// own (claude 2.1.283).
	TaskNotification string
	// PromptID is the id claude gave the prompt a user entry holds
	// (promptId), on that entry alone: the one that says where the prompt
	// came from (promptSource). The entries of the turn it starts — tool
	// results, an interrupt — carry the same promptId, and no PromptID here.
	// A prompt typed into claude's TUI is matched to its input by it, since
	// the entry's uuid is claude's own (claude 2.1.283).
	PromptID string
	// StopHookFeedback marks the user entry claude writes when a Stop hook
	// blocked the stop of a turn (isMeta, "Stop hook feedback:"): the hook's
	// words to the model, and the turn goes on (claude 2.1.283).
	StopHookFeedback bool
	// StopHooksRan marks the system entry claude writes once the Stop hooks
	// of a reply ran (stop_hook_summary), after their feedback when one
	// blocked: a reply before it that ended its turn did.
	StopHooksRan bool
}

// EventEntry is the Type of the one event FollowEntries gives an entry that
// holds facts and no events: an assistant entry whose content is all
// thinking, say, but whose stop_reason ends its turn, or a Stop hooks'
// summary.
const EventEntry = "entry"

// interruptText begins every user entry claude writes for an interrupt.
const interruptText = "[Request interrupted by user"

// stopHookFeedbackText begins the user entry claude writes when a Stop hook
// blocked a turn's stop.
const stopHookFeedbackText = "Stop hook feedback:"

// originTaskNotification is the origin kind of what claude writes to tell
// the model background work ended, and of the turn that takes it up.
const originTaskNotification = "task-notification"

// TaskID is the first task id a task notification's text names
// (<task-id>…</task-id>); "" for none.
func TaskID(text string) string {
	_, rest, ok := strings.Cut(text, "<task-id>")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "</task-id>")
	if !ok {
		return ""
	}
	return strings.TrimSpace(id)
}

// FollowEntries is Follow with DecodeEntry: every event carries its entry.
func FollowEntries(sessionID, workingDir string, env []string, from transcript.Checkpoint) (*transcript.Follower, error) {
	return follow(sessionID, workingDir, env, from, DecodeEntry)
}

// DecodeEntry is Follow's decoder, with each event's Meta set to its *Entry.
// The events are exactly the ones Follow gives the entry, so their follower
// identities are the same; an entry with facts but no events gets one event
// of Type EventEntry.
func DecodeEntry(record []byte) ([]transcript.BlockEvent, error) {
	events, err := decodeRecord(record)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Type              string          `json:"type"`
		Subtype           string          `json:"subtype"`
		UUID              string          `json:"uuid"`
		Timestamp         string          `json:"timestamp"`
		IsSidechain       bool            `json:"isSidechain"`
		IsMeta            bool            `json:"isMeta"`
		IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
		Error             json.RawMessage `json:"error"`
		APIErrorStatus    int             `json:"apiErrorStatus"`
		Message           *struct {
			ID         string          `json:"id"`
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
		Origin *struct {
			Kind string `json:"kind"`
		} `json:"origin"`
		PromptID     string `json:"promptId"`
		PromptSource string `json:"promptSource"`
	}
	if json.Unmarshal(record, &raw) != nil {
		return events, nil
	}
	e := &Entry{Type: raw.Type, UUID: raw.UUID, Sidechain: raw.IsSidechain}
	if raw.Message != nil && raw.Type == transcript.TypeAssistant {
		e.MessageID, e.StopReason = raw.Message.ID, raw.Message.StopReason
	}
	if raw.IsAPIErrorMessage {
		var tag string
		_ = json.Unmarshal(raw.Error, &tag)
		e.APIError = strings.TrimSpace(tag)
		e.APIErrorStatus = raw.APIErrorStatus
	}
	if raw.Type == transcript.TypeUser && raw.Origin != nil && raw.Origin.Kind == originTaskNotification {
		e.TaskNotification = "unknown"
		if raw.Message != nil {
			var text string
			_ = json.Unmarshal(raw.Message.Content, &text)
			if id := TaskID(text); id != "" {
				e.TaskNotification = id
			}
		}
	}
	if raw.Type == transcript.TypeUser && raw.PromptSource != "" {
		e.PromptID = raw.PromptID
	}
	if raw.Type == transcript.TypeUser {
		for _, be := range events {
			if be.Event.Type == transcript.EventText && strings.HasPrefix(be.Event.Text, interruptText) {
				e.Interrupt = true
			}
			if be.Event.Type == transcript.EventText && raw.IsMeta && strings.HasPrefix(be.Event.Text, stopHookFeedbackText) {
				e.StopHookFeedback = true
			}
		}
	}
	for i := range events {
		events[i].Meta = e
	}
	e.StopHooksRan = raw.Type == "system" && raw.Subtype == "stop_hook_summary"
	if len(events) == 0 && (e.StopReason != "" || e.APIError != "" || e.StopHooksRan) {
		// The entry's time orders it among the others, as a reader that
		// sorts a read by time does (RecordOptions.PromptLag).
		events = append(events, transcript.BlockEvent{Meta: e, Event: transcript.Event{
			Role: raw.Type, Type: EventEntry, UUID: raw.UUID, Source: transcript.SourceFile,
			Timestamp: parseLineTimestamp(raw.Timestamp),
		}})
	}
	return events, nil
}
