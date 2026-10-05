package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Observation is a fact an adapter reports.
//
// ID is "<kind>:<key>": unique within the Session, and stable — the same fact
// has the same id in every batch, process and adapter version. Keys by kind:
// turn_started and turn_ended, the input id — or, for a turn the harness
// started with no input (capability autonomous_turns), its turn id, which no
// input id equals; user_input and assistant_text,
// the record entry's id and block index (for a record without entry ids,
// assistant_text's message id and block index); api_error, the record entry's
// id; tool_*, the tool use id; subagent_*, the subagent id; prompt_*, the
// prompt id; text_delta, the message id and index; retrying, the input id and
// attempt; rate_limit, blocked, unblocked and session_exited, an id of the
// harness process instance and a counter. A record entry with no id of its
// own is keyed by its place in the record. The Supervisor stores an
// observation under (agent, session, id): the same id in another Session is
// another fact.
type Observation struct {
	ID     string    `json:"id"`
	Kind   Kind      `json:"kind"`
	Origin Origin    `json:"origin"`
	Time   time.Time `json:"time"`
	// TurnID is the turn the fact belongs to, and InputID the input that
	// turn is for. A turn the harness started itself has a TurnID and no
	// InputID, and so has everything it reports.
	TurnID  string `json:"turn_id,omitempty"`
	InputID string `json:"input_id,omitempty"`
	// Entry is the id of the harness's record entry that holds a record-origin
	// fact, where the record gives its entries ids. It says where the fact
	// is; ID alone is its identity.
	Entry string `json:"entry,omitempty"`
	// Data is the kind's payload (the *Data types below).
	Data json.RawMessage `json:"data"`
	// Truncated: a text field was cut at MaxObservationText.
	Truncated bool `json:"truncated,omitempty"`
}

// ObservationID is the id of kind's fact with key.
func ObservationID(kind Kind, key string) string { return string(kind) + ":" + key }

// NewObservation is an observation of kind with key, carrying data.
func NewObservation(kind Kind, key string, origin Origin, at time.Time, data any) Observation {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(fmt.Sprintf("contract: observation data: %v", err))
	}
	return Observation{ID: ObservationID(kind, key), Kind: kind, Origin: origin, Time: at.UTC(), Data: raw}
}

// Decode unmarshals the observation's data into v.
func (o Observation) Decode(v any) error { return json.Unmarshal(o.Data, v) }

// Key is the id's key: what follows "<kind>:".
func (o Observation) Key() string {
	key, _ := strings.CutPrefix(o.ID, string(o.Kind)+":")
	return key
}

// Origin is where an observation came from.
type Origin string

// Origins.
const (
	// OriginRecord: a durable fact, from the harness's own record. Until it is
	// acknowledged, every later read of the record delivers it again, with
	// the same id.
	OriginRecord Origin = "record"
	// OriginLive: best-effort telemetry that exists only while the Session
	// runs; a crash can lose it.
	OriginLive Origin = "live"
)

// Values lists the set.
func (Origin) Values() []string { return []string{"record", "live"} }

// Kind is an observation's kind. The set is closed in 1.0.
type Kind string

// Kinds, with their data.
const (
	KindTurnStarted     Kind = "turn_started"     // live; {}
	KindTurnEnded       Kind = "turn_ended"       // live, and record when the record proves it; TurnEndedData
	KindUserInput       Kind = "user_input"       // record; TextData
	KindAssistantText   Kind = "assistant_text"   // record; AssistantTextData
	KindToolUse         Kind = "tool_use"         // record; ToolUseData
	KindToolResult      Kind = "tool_result"      // record; ToolResultData
	KindAPIError        Kind = "api_error"        // record; APIErrorData
	KindTextDelta       Kind = "text_delta"       // live, streaming_text; TextDeltaData
	KindToolStarted     Kind = "tool_started"     // record or live, tools_observed; ToolUseData
	KindToolFinished    Kind = "tool_finished"    // record or live, tools_observed; ToolFinishedData
	KindSubagentStarted Kind = "subagent_started" // record or live, subagents; SubagentData
	KindSubagentStopped Kind = "subagent_stopped" // record or live, subagents; SubagentData
	KindPromptRaised    Kind = "prompt_raised"    // live, prompts; PromptInfo
	KindPromptResolved  Kind = "prompt_resolved"  // live, prompts; PromptResolvedData
	KindRateLimit       Kind = "rate_limit"       // live, rate_limits; RateLimitData
	KindRetrying        Kind = "retrying"         // live, retry_visible; RetryingData
	KindBlocked         Kind = "blocked"          // live; Block
	KindUnblocked       Kind = "unblocked"        // live; {}
	KindSessionExited   Kind = "session_exited"   // live; SessionExitedData
	KindBackgroundTasks Kind = "background_tasks" // live, background_turns; BackgroundTasksData (1.4)
)

// Values lists the set.
func (Kind) Values() []string {
	return []string{
		"turn_started", "turn_ended", "user_input", "assistant_text", "tool_use", "tool_result",
		"api_error", "text_delta", "tool_started", "tool_finished", "subagent_started",
		"subagent_stopped", "prompt_raised", "prompt_resolved", "rate_limit", "retrying",
		"blocked", "unblocked", "session_exited", "background_tasks",
	}
}

// Capability is the capability a kind needs, "" for none.
func (k Kind) Capability() Capability {
	switch k {
	case KindTextDelta:
		return CapStreamingText
	case KindToolStarted, KindToolFinished:
		return CapToolsObserved
	case KindSubagentStarted, KindSubagentStopped:
		return CapSubagents
	case KindPromptRaised, KindPromptResolved:
		return CapPrompts
	case KindRateLimit:
		return CapRateLimits
	case KindRetrying:
		return CapRetryVisible
	case KindBackgroundTasks:
		return CapBackgroundTurns
	}
	return ""
}

// TurnOutcome is how a turn ended.
type TurnOutcome string

// Turn outcomes.
const (
	TurnCompleted   TurnOutcome = "completed"
	TurnErrored     TurnOutcome = "errored"
	TurnInterrupted TurnOutcome = "interrupted"
	TurnCancelled   TurnOutcome = "cancelled"
	TurnRefused     TurnOutcome = "refused"
)

// Values lists the set.
func (TurnOutcome) Values() []string {
	return []string{"completed", "errored", "interrupted", "cancelled", "refused"}
}

// ErrorClass classifies a turn's or an API call's failure.
type ErrorClass string

// Error classes.
const (
	ErrorAPI        ErrorClass = "api"
	ErrorOverloaded ErrorClass = "overloaded"
	ErrorUsageLimit ErrorClass = "usage_limit"
	ErrorAuth       ErrorClass = "auth"
	ErrorBilling    ErrorClass = "billing"
	ErrorMaxOutput  ErrorClass = "max_output"
	ErrorInternal   ErrorClass = "internal"
)

// Values lists the set.
func (ErrorClass) Values() []string {
	return []string{"api", "overloaded", "usage_limit", "auth", "billing", "max_output", "internal"}
}

// TurnEndedData is turn_ended's payload.
type TurnEndedData struct {
	Outcome TurnOutcome `json:"outcome"`
	// Text is the turn's final assistant text, if any.
	Text  string     `json:"text,omitempty"`
	Error *TurnError `json:"error,omitempty"`
}

// TurnError is why a turn failed.
type TurnError struct {
	Class       ErrorClass `json:"class"`
	HTTPStatus  int        `json:"http_status,omitempty"`
	RetryAfterS int        `json:"retry_after_s,omitempty"`
	ResumeAt    *time.Time `json:"resume_at,omitempty"`
}

// TextData is user_input's payload.
type TextData struct {
	Text string `json:"text"`
}

// AssistantTextData is assistant_text's payload.
type AssistantTextData struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

// ToolUseData is tool_use's and tool_started's payload.
type ToolUseData struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// ToolResultData is tool_result's payload.
type ToolResultData struct {
	ToolUseID string `json:"tool_use_id"`
	Output    string `json:"output"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ToolFinishedData is tool_finished's payload.
type ToolFinishedData struct {
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	Output    string `json:"output,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
}

// APIErrorData is api_error's payload.
type APIErrorData struct {
	Class      ErrorClass `json:"class"`
	HTTPStatus int        `json:"http_status,omitempty"`
	// Message is the error as the harness rendered it, when it did.
	Message string `json:"message,omitempty"`
}

// TextDeltaData is text_delta's payload.
type TextDeltaData struct {
	MessageID string `json:"message_id"`
	Index     int    `json:"index"`
	Text      string `json:"text"`
	Final     bool   `json:"final,omitempty"`
}

// SubagentData is subagent_started's and subagent_stopped's payload.
type SubagentData struct {
	SubagentID  string `json:"subagent_id"`
	Type        string `json:"type,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

// PromptResolvedData is prompt_resolved's payload.
type PromptResolvedData struct {
	PromptID string `json:"prompt_id"`
	// By is answer, or harness when the harness withdrew it.
	By string `json:"by"`
}

// RateLimitData is rate_limit's payload.
type RateLimitData struct {
	Status  string            `json:"status"`
	Windows []RateLimitWindow `json:"windows,omitempty"`
}

// RateLimitWindow is one usage window's standing.
type RateLimitWindow struct {
	Name     string     `json:"name"`
	UsedPct  *float64   `json:"used_pct,omitempty"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// BackgroundTasksData is background_tasks' payload: every task the harness
// runs in the background now, each of which a turn of its own takes up when
// it ends (background_turns). Empty: none is left. It is reported whenever
// the set changes.
type BackgroundTasksData struct {
	Tasks []BackgroundTask `json:"tasks"`
}

// BackgroundTask is one task the harness runs in the background.
type BackgroundTask struct {
	// ID is the harness's id for the task.
	ID   string         `json:"id"`
	Kind BackgroundKind `json:"kind"`
	// Description is the harness's short description of it, if any.
	Description string `json:"description,omitempty"`
}

// BackgroundKind is what a background task runs.
type BackgroundKind string

// Background task kinds.
const (
	BackgroundCommand  BackgroundKind = "command"  // a shell command
	BackgroundSubagent BackgroundKind = "subagent" // a subagent
	BackgroundOther    BackgroundKind = "other"    // anything else
)

// Values lists the set.
func (BackgroundKind) Values() []string { return []string{"command", "subagent", "other"} }

// RetryingData is retrying's payload.
type RetryingData struct {
	Attempt    int `json:"attempt"`
	Max        int `json:"max"`
	DelayMS    int `json:"delay_ms,omitempty"`
	HTTPStatus int `json:"http_status,omitempty"`
}

// ExitClass is how a harness process ended.
type ExitClass string

// Exit classes.
const (
	ExitClean         ExitClass = "clean"
	ExitCrashed       ExitClass = "crashed"
	ExitKilled        ExitClass = "killed"
	ExitBinaryMissing ExitClass = "binary_missing"
	ExitAuth          ExitClass = "auth"
)

// Values lists the set.
func (ExitClass) Values() []string {
	return []string{"clean", "crashed", "killed", "binary_missing", "auth"}
}

// SessionExitedData is session_exited's payload, read from the process's exit
// status, never from its last message.
type SessionExitedData struct {
	Class    ExitClass `json:"class"`
	ExitCode *int      `json:"exit_code,omitempty"`
	Signal   string    `json:"signal,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Batch is one Observe result. A batch with items, a checkpoint, a reset, a
// rescan or faults has a BatchID and must be acknowledged; an empty poll has
// none and needs no acknowledgement.
type Batch struct {
	BatchID    string        `json:"batch_id,omitempty"`
	Items      []Observation `json:"items"`
	Checkpoint *Checkpoint   `json:"checkpoint,omitempty"`
	// Reset: the record no longer continues the checkpoint — replaced,
	// truncated or rewritten — and the items that follow start from the new
	// record.
	Reset *Reset `json:"reset,omitempty"`
	// Rescan: the adapter could not read the checkpoint it was given and read
	// the record again from its start; identities deduplicate what the
	// Supervisor already stored.
	Rescan *Rescan `json:"rescan,omitempty"`
	Faults []Fault `json:"faults,omitempty"`
	// EndOfRecord: a record handle has nothing more to deliver.
	EndOfRecord bool `json:"end_of_record,omitempty"`
}

// NeedsAck reports whether the batch must be acknowledged.
func (b Batch) NeedsAck() bool { return b.BatchID != "" }

// Checkpoint is an adapter's position in its record. The Host may read
// Format; Data is opaque. At most MaxCheckpointBytes.
type Checkpoint struct {
	Format int    `json:"format"`
	Data   []byte `json:"data"`
}

func (c *Checkpoint) validate() error {
	if c == nil {
		return nil
	}
	if c.Format < 1 {
		return &Error{Code: CodeProtocol, Field: "checkpoint.format", Message: fmt.Sprintf("%d", c.Format)}
	}
	if len(c.Data) > MaxCheckpointBytes {
		return &Error{Code: CodeProtocol, Field: "checkpoint.data", Message: "too large"}
	}
	return nil
}

// Reset says why the record no longer continues a checkpoint.
type Reset struct {
	Reason   string      `json:"reason"`
	Previous *Checkpoint `json:"previous,omitempty"`
}

// Rescan says why the record was read again from its start.
type Rescan struct {
	Reason string `json:"reason"`
}

// Fault reports observations lost, cut or rejected.
type Fault struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}
