package transcript

import (
	"encoding/json"
	"fmt"
	"time"
)

// The DURABLE serialization of events (the hook spool, and later the event
// store) — distinct from Event's PUBLIC JSON, which omits the internal
// Source/NativeID/SchemaVersion (json:"-") for the byte-identical Runs-tab DTO.
// The durable form MUST persist those fields: the orchestrator's authority
// filter keys on Source and dedup keys on NativeID, so a round-trip that dropped
// them would silently corrupt acquisition. (See the event.go note on the two
// serializations.)

// wireEvent mirrors Event with EVERY field serialized, including the internal
// ones. It exists only so json can round-trip the full event; callers use
// MarshalParsedEvents / UnmarshalParsedEvents.
type wireEvent struct {
	Seq           int             `json:"seq"`
	Timestamp     time.Time       `json:"timestamp"`
	Role          string          `json:"role"`
	Type          string          `json:"type"`
	Text          string          `json:"text,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolUseID     string          `json:"tool_use_id,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	Output        string          `json:"output,omitempty"`
	UUID          string          `json:"uuid,omitempty"`
	Source        string          `json:"source,omitempty"`
	NativeID      string          `json:"native_id,omitempty"`
	SchemaVersion int             `json:"schema_version"`
}

// wireParsedEvent is the durable wire form of a ParsedEvent (session-tagged).
type wireParsedEvent struct {
	HarnessSessionID string    `json:"harness_session_id"`
	ParentSessionID  string    `json:"parent_session_id,omitempty"`
	Event            wireEvent `json:"event"`
}

func toWire(pe ParsedEvent) wireParsedEvent {
	e := pe.Event
	return wireParsedEvent{
		HarnessSessionID: pe.HarnessSessionID,
		ParentSessionID:  pe.ParentSessionID,
		Event: wireEvent{
			Seq: e.Seq, Timestamp: e.Timestamp, Role: e.Role, Type: e.Type, Text: e.Text,
			ToolName: e.ToolName, ToolUseID: e.ToolUseID, ToolInput: e.ToolInput,
			Output: e.Output, UUID: e.UUID,
			Source: e.Source, NativeID: e.NativeID, SchemaVersion: e.SchemaVersion,
		},
	}
}

func fromWire(w wireParsedEvent) ParsedEvent {
	e := w.Event
	return ParsedEvent{
		HarnessSessionID: w.HarnessSessionID,
		ParentSessionID:  w.ParentSessionID,
		Event: Event{
			Seq: e.Seq, Timestamp: e.Timestamp, Role: e.Role, Type: e.Type, Text: e.Text,
			ToolName: e.ToolName, ToolUseID: e.ToolUseID, ToolInput: e.ToolInput,
			Output: e.Output, UUID: e.UUID,
			Source: e.Source, NativeID: e.NativeID, SchemaVersion: e.SchemaVersion,
		},
	}
}

// MarshalParsedEvents serializes events in the DURABLE form (all fields,
// including Source/NativeID/SchemaVersion), as a JSON array.
func MarshalParsedEvents(events []ParsedEvent) ([]byte, error) {
	wire := make([]wireParsedEvent, len(events))
	for i, pe := range events {
		wire[i] = toWire(pe)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("transcript: marshal parsed events: %w", err)
	}
	return data, nil
}

// UnmarshalParsedEvents parses the durable form produced by MarshalParsedEvents,
// restoring all fields (so Source/NativeID survive the round-trip).
func UnmarshalParsedEvents(data []byte) ([]ParsedEvent, error) {
	var wire []wireParsedEvent
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("transcript: unmarshal parsed events: %w", err)
	}
	out := make([]ParsedEvent, len(wire))
	for i, w := range wire {
		out[i] = fromWire(w)
	}
	return out, nil
}
