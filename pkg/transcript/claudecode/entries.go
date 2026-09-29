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
}

// EventEntry is the Type of the one event FollowEntries gives an entry that
// holds facts and no events: an assistant entry whose content is all
// thinking, say, but whose stop_reason ends its turn.
const EventEntry = "entry"

// interruptText begins every user entry claude writes for an interrupt.
const interruptText = "[Request interrupted by user"

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
		UUID              string          `json:"uuid"`
		IsSidechain       bool            `json:"isSidechain"`
		IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
		Error             json.RawMessage `json:"error"`
		APIErrorStatus    int             `json:"apiErrorStatus"`
		Message           *struct {
			ID         string `json:"id"`
			StopReason string `json:"stop_reason"`
		} `json:"message"`
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
	if raw.Type == transcript.TypeUser {
		for _, be := range events {
			if be.Event.Type == transcript.EventText && strings.HasPrefix(be.Event.Text, interruptText) {
				e.Interrupt = true
			}
		}
	}
	for i := range events {
		events[i].Meta = e
	}
	if len(events) == 0 && (e.StopReason != "" || e.APIError != "") {
		events = append(events, transcript.BlockEvent{Meta: e, Event: transcript.Event{
			Role: raw.Type, Type: EventEntry, UUID: raw.UUID, Source: transcript.SourceFile,
		}})
	}
	return events, nil
}
