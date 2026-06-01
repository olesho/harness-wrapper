package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion is the current canonical Event wire-schema version. Consumers
// (loom's Runs tab) assert they support >= a minimum and surface a typed error
// rather than rendering garbage when a wrapper bump changes the shape. Bump only
// on a breaking change; add fields additively otherwise.
const SchemaVersion = 1

// Event is the canonical, harness-agnostic representation of a single moment in
// an agent session's transcript. Per-harness parsers translate native formats
// into []Event so consumers handle one shape.
//
// Promoted from loomcli's internal/sessions/transcript.Event (the public fields
// + their JSON tags are kept identical so loom can serve a byte-identical Runs-
// tab DTO). The added fields (SchemaVersion, Source, NativeID) are INTERNAL —
// json:"-" — and live on the durable store row / envelope, never the public DTO.
type Event struct {
	Seq       int             `json:"seq"`
	Timestamp time.Time       `json:"timestamp"`
	Role      string          `json:"role"` // user, assistant, tool, system
	Type      string          `json:"type"` // text, tool_use, tool_result, session_meta
	Text      string          `json:"text,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Output    string          `json:"output,omitempty"` // tool_result text
	UUID      string          `json:"uuid,omitempty"`   // native message UUID when available

	// --- INTERNAL metadata (json:"-"): durable store row only, NOT public DTO ---
	SchemaVersion int    `json:"-"` // wire-schema version stamped at write
	Source        string `json:"-"` // SourceLive | SourceFile — for the mode authority filter
	NativeID      string `json:"-"` // PRIMARY identity (parser-owned); see ID()
}

// Canonical role constants.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

// Canonical event type constants (the public Type values — DTO-compatible with
// loomcli). Distinct from acquisition Source, below.
const (
	EventText        = "text"
	EventToolUse     = "tool_use"
	EventToolResult  = "tool_result"
	EventSessionMeta = "session_meta"
)

// Event provenance (Source) — which acquisition produced the event. Used by the
// mode authority filter (P3) so live + file copies of the same parent event
// don't both land.
const (
	SourceLive = "live"
	SourceFile = "file"
)

// ID returns the stable dedup identity for the event. Identity is PARSER-OWNED:
// each parser should set a kind-qualified NativeID (e.g. "msg:<uuid>",
// "tool-use:<id>", "tool-result:<id>"). When NativeID is unset, ID falls back to
// a content hash — which must be CROSS-SOURCE-STABLE (the same logical event from
// the live stream vs the on-disk file collapses to one row on upsert), so it must
// NOT include Seq (parser-local) or wall-clock arrival time. The native event
// Timestamp is fine (it's recorded in the source, not arrival).
func (e Event) ID() string {
	if e.NativeID != "" {
		return e.NativeID
	}
	if e.UUID != "" {
		return "msg:" + e.UUID
	}
	if e.ToolUseID != "" {
		// Kind-qualified so a tool-use and its tool-result never collapse.
		return e.Type + ":" + e.ToolUseID
	}
	h := sha256.New()
	_, _ = h.Write([]byte(strings.Join([]string{
		e.Type, e.Role,
		strconv.FormatInt(e.Timestamp.UnixNano(), 10),
		e.Text, string(e.ToolInput), e.Output,
	}, "\x00")))
	return "h:" + hex.EncodeToString(h.Sum(nil)[:16])
}

// ParsedEvent is what a per-harness parser returns: an Event tagged with the
// native session it belongs to. A single hook payload / export / file can yield
// events for the parent AND a spawned subagent (different native sessions), so
// the tagging must travel with each event. The orchestrator stamps RunID +
// Harness to produce an EventEnvelope.
type ParsedEvent struct {
	HarnessSessionID string
	ParentSessionID  string // empty for the top session; set for subagent/nested
	Event            Event
}

// EventEnvelope is the durable, routable unit the orchestrator emits to the
// consumer and the event store persists. loom routes/stores by
// (RunID, HarnessSessionID) and dedups by (RunID, HarnessSessionID, Event.ID()).
type EventEnvelope struct {
	RunID            string // STABLE LOGICAL run id (persisted in the lock, reused across resume)
	Harness          string // "claude" | "codex" | ...
	HarnessSessionID string
	ParentSessionID  string
	Event            Event
}

// Envelope stamps run-level identity onto a ParsedEvent.
func (pe ParsedEvent) Envelope(runID, harness string) EventEnvelope {
	return EventEnvelope{
		RunID:            runID,
		Harness:          harness,
		HarnessSessionID: pe.HarnessSessionID,
		ParentSessionID:  pe.ParentSessionID,
		Event:            pe.Event,
	}
}

// TurnsFromEvents projects canonical Events down to the lossy chat Turn view
// (Role/Text/Timestamp) that pkg/chat.History returns. Tool-only events without
// renderable Text are dropped — chat shows conversational turns, not tool I/O.
func TurnsFromEvents(events []Event) []Turn {
	out := make([]Turn, 0, len(events))
	for _, e := range events {
		if e.Text == "" {
			continue
		}
		role := e.Role
		if role == "" {
			role = RoleSystem
		}
		out = append(out, Turn{Role: role, Text: e.Text, Timestamp: e.Timestamp})
	}
	return out
}
