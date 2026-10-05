package contract

import (
	"fmt"
	"time"
)

// OpenMode is how Open starts a Session.
type OpenMode string

// Open modes.
const (
	// OpenFresh starts a new Session: under SessionID when set (capability
	// assign_session_id), else under an id the harness chooses. No
	// checkpoint.
	OpenFresh OpenMode = "fresh"
	// OpenReopen continues the Session SessionID from Checkpoint (capability
	// resume).
	OpenReopen OpenMode = "reopen"
)

// Values lists the set.
func (OpenMode) Values() []string { return []string{"fresh", "reopen"} }

// OpenRequest is what NewSession takes.
type OpenRequest struct {
	Mode OpenMode `json:"mode"`
	// SessionID is the Session to reopen, or the id a fresh one takes.
	SessionID string `json:"session_id,omitempty"`
	// OpenConfig is ProvisionResult.OpenConfig, unchanged.
	OpenConfig []byte `json:"open_config"`
	// Layout is the agent's roots, as provisioned.
	Layout Layout `json:"layout"`
	// Credential is the staged credential; nil for a harness that needs none.
	Credential *CredentialFile `json:"credential,omitempty"`
	// Checkpoint is the last committed one of the Session being reopened.
	// A fresh open has none.
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	// Loaded says the Session was loaded into this environment from an
	// archive (capability session_load; a reopen only). Such an open never
	// starts a fresh conversation: it fails with session_not_found when the
	// harness's record of the Session is not where the harness looks, and
	// the id it returns is SessionID. The first open in the new environment
	// also checks that what the harness keeps of the Session outside its
	// record — a thread's name, its goal — came with it, and fails with
	// state_mismatch when it did not.
	Loaded bool `json:"loaded,omitempty"`
}

// CredentialFile is a staged credential: its kind, and the file holding it.
type CredentialFile struct {
	Kind string `json:"kind"`
	File string `json:"file"`
}

// Validate checks the request's own consistency: the mode, and a checkpoint
// only on reopen.
func (r OpenRequest) Validate() error {
	switch r.Mode {
	case OpenFresh:
		if r.Checkpoint != nil {
			return &Error{Code: CodeProtocol, Field: "checkpoint", Message: "a fresh open takes no checkpoint"}
		}
	case OpenReopen:
		if r.SessionID == "" {
			return &Error{Code: CodeProtocol, Field: "session_id", Message: "reopen needs the session id"}
		}
	default:
		return &Error{Code: CodeProtocol, Field: "mode", Message: fmt.Sprintf("%q, want fresh or reopen", r.Mode)}
	}
	if r.Loaded && r.Mode != OpenReopen {
		return &Error{Code: CodeProtocol, Field: "loaded", Message: "a loaded Session is reopened, never opened fresh"}
	}
	if r.SessionID != "" && !ValidID(r.SessionID) {
		return &Error{Code: CodeProtocol, Field: "session_id", Message: fmt.Sprintf("%q is not an id", r.SessionID)}
	}
	if len(r.OpenConfig) > MaxOpenConfigBytes {
		return &Error{Code: CodeProtocol, Field: "open_config", Message: "too large"}
	}
	return r.Checkpoint.validate()
}

// OpenResult is what Open returns.
type OpenResult struct {
	// SessionID is the harness's own id for the Session. The Host stores it
	// and reopens with it, even when it differs from the id it asked for.
	SessionID string `json:"session_id"`
	State     State  `json:"state"`
}

// Input is one input to Send.
type Input struct {
	// InputID is the Host's id for it, unique per agent for all time.
	InputID string `json:"input_id"`
	// Content is a nonempty list of parts with at least one nonempty text;
	// 1.0 has text parts only.
	Content []ContentPart `json:"content"`
}

// ContentPart is one part of an input.
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ContentText is the only content type 1.0 has.
const ContentText = "text"

// Text is an input of one text part.
func Text(inputID, text string) Input {
	return Input{InputID: inputID, Content: []ContentPart{{Type: ContentText, Text: text}}}
}

// Validate checks the input against the contract and a limit on its size.
// Every failure is not_submitted: it is refused before anything is handed to
// the harness.
func (in Input) Validate(maxBytes int) error {
	if !ValidID(in.InputID) {
		return &Error{Code: CodeProtocol, Field: "input_id", Certainty: NotSubmitted, Message: fmt.Sprintf("%q is not an id", in.InputID)}
	}
	if len(in.Content) == 0 {
		return &Error{Code: CodeInvalidInput, Certainty: NotSubmitted, Message: "no content"}
	}
	size, text := 0, false
	for _, p := range in.Content {
		if p.Type != ContentText {
			return &Error{Code: CodeInvalidInput, Certainty: NotSubmitted, Message: fmt.Sprintf("content type %q", p.Type)}
		}
		size += len(p.Text)
		text = text || p.Text != ""
	}
	if !text {
		return &Error{Code: CodeInvalidInput, Certainty: NotSubmitted, Message: "no text"}
	}
	if maxBytes > 0 && size > maxBytes {
		return &Error{Code: CodeInvalidInput, Certainty: NotSubmitted, Message: fmt.Sprintf("%d bytes, more than %d", size, maxBytes)}
	}
	return nil
}

// JoinText is the input's text parts, joined by newlines.
func (in Input) JoinText() string {
	s := ""
	for i, p := range in.Content {
		if i > 0 {
			s += "\n"
		}
		s += p.Text
	}
	return s
}

// Receipt is Send's answer: the input was handed to the harness.
type Receipt string

// ReceiptSubmitted is the one receipt: an input that was not submitted is an
// error carrying a Certainty.
const ReceiptSubmitted Receipt = "submitted"

// Values lists the set.
func (Receipt) Values() []string { return []string{"submitted"} }

// SendResult is Send's result, returned once the adapter has handed the input
// to the harness, and no earlier.
type SendResult struct {
	Receipt Receipt `json:"receipt"`
	TurnID  string  `json:"turn_id"`
}

// InterruptRequest names the turn to stop: an input's, by InputID, or — with
// capability autonomous_turns — one the harness started itself, by TurnID.
// Exactly one of them is set.
type InterruptRequest struct {
	InputID string `json:"input_id,omitempty"`
	// TurnID is the turn id a turn_started with no input reported, or State
	// gives while such a turn runs.
	TurnID string `json:"turn_id,omitempty"`
	// DeadlineMS bounds the wait for the outcome, 100–60000.
	DeadlineMS int `json:"deadline_ms,omitempty"`
}

// Validate checks that the request names exactly one turn, by an id.
func (r InterruptRequest) Validate() error {
	switch {
	case r.InputID != "" && r.TurnID != "":
		return &Error{Code: CodeProtocol, Field: "turn_id", Message: "an interrupt names an input or a turn, not both"}
	case r.TurnID != "" && !ValidID(r.TurnID):
		return &Error{Code: CodeProtocol, Field: "turn_id", Message: fmt.Sprintf("%q is not an id", r.TurnID)}
	case r.TurnID == "" && !ValidID(r.InputID):
		return &Error{Code: CodeProtocol, Field: "input_id", Message: fmt.Sprintf("%q is not an id", r.InputID)}
	}
	if r.DeadlineMS != 0 && (r.Deadline() < MinInterruptDeadline || r.Deadline() > MaxInterruptDeadline) {
		return &Error{Code: CodeProtocol, Field: "deadline_ms", Message: "out of range"}
	}
	return nil
}

// Deadline is the request's deadline, DefaultInterruptDeadline when unset.
func (r InterruptRequest) Deadline() time.Duration {
	if r.DeadlineMS <= 0 {
		return DefaultInterruptDeadline
	}
	return time.Duration(r.DeadlineMS) * time.Millisecond
}

// InterruptOutcome is what an interrupt did.
type InterruptOutcome string

// Interrupt outcomes.
const (
	// InterruptStopped: the turn had begun and was stopped; its turn_ended
	// with outcome interrupted follows.
	InterruptStopped InterruptOutcome = "stopped"
	// InterruptCancelled: positive evidence proves the input was withdrawn
	// before it ran and cannot run later; its turn_ended with outcome
	// cancelled follows. No first token, no record entry and a timeout are
	// not such evidence.
	InterruptCancelled InterruptOutcome = "cancelled"
	// InterruptNoTurn: no turn is running.
	InterruptNoTurn InterruptOutcome = "no_turn"
	// InterruptTooLate: the named turn already ended, or another turn is
	// current. Nothing was stopped.
	InterruptTooLate InterruptOutcome = "too_late"
)

// Values lists the set.
func (InterruptOutcome) Values() []string {
	return []string{"stopped", "cancelled", "no_turn", "too_late"}
}

// Choice answers a prompt: exactly one of OptionID, OptionIDs and Text.
type Choice struct {
	OptionID  string   `json:"option_id,omitempty"`
	OptionIDs []string `json:"option_ids,omitempty"`
	Text      string   `json:"text,omitempty"`
}

// Validate checks that exactly one form is set.
func (c Choice) Validate() error {
	n := 0
	if c.OptionID != "" {
		n++
	}
	if len(c.OptionIDs) > 0 {
		n++
	}
	if c.Text != "" {
		n++
	}
	if n != 1 {
		return &Error{Code: CodeInvalidChoice, Message: "want exactly one of option_id, option_ids and text"}
	}
	return nil
}

// Phase is a Session's state.
type Phase string

// Phases.
const (
	// PhaseUnopened: a new handle. Legal: Open, State, Close.
	PhaseUnopened Phase = "unopened"
	// PhaseStarting: Open in progress. Legal: State, Observe, Ack, Close.
	PhaseStarting Phase = "starting"
	// PhaseIdle: ready for one input.
	PhaseIdle Phase = "idle"
	// PhaseBusy: a turn is running, possibly retrying or being interrupted.
	// While the turn is one the harness started itself, Send is still legal:
	// it stops that turn, then submits its input.
	PhaseBusy Phase = "busy"
	// PhaseAwaitingAnswer: the turn waits on a prompt (capability prompts).
	PhaseAwaitingAnswer Phase = "awaiting_answer"
	// PhaseBlocked: no turn runs, and the adapter refuses a new input until
	// the condition in State.Block clears.
	PhaseBlocked Phase = "blocked"
	// PhaseExited: the harness's session process ended. Terminal.
	PhaseExited Phase = "exited"
)

// Values lists the set.
func (Phase) Values() []string {
	return []string{"unopened", "starting", "idle", "busy", "awaiting_answer", "blocked", "exited"}
}

// BlockReason is why a Session refuses input.
type BlockReason string

// Block reasons.
const (
	BlockUsageLimited       BlockReason = "usage_limited"
	BlockAuthRequired       BlockReason = "auth_required"
	BlockBilling            BlockReason = "billing"
	BlockUnrecognizedDialog BlockReason = "unrecognized_dialog"
)

// Values lists the set.
func (BlockReason) Values() []string {
	return []string{"usage_limited", "auth_required", "billing", "unrecognized_dialog"}
}

// Block is a condition that refuses input.
type Block struct {
	Reason BlockReason `json:"reason"`
	// ResumeAt is the earliest time the condition may clear, when known.
	ResumeAt *time.Time `json:"resume_at,omitempty"`
}

// PromptInfo is a prompt the harness raised, awaiting Answer.
type PromptInfo struct {
	PromptID    string         `json:"prompt_id"`
	Kind        string         `json:"kind"`
	Prompt      string         `json:"prompt"`
	Options     []PromptOption `json:"options,omitempty"`
	MultiSelect bool           `json:"multi_select,omitempty"`
}

// PromptOption is one option of a prompt.
type PromptOption struct {
	OptionID string `json:"option_id"`
	Label    string `json:"label"`
}

// RetryInfo is the retry a turn is in (capability retry_visible).
type RetryInfo struct {
	Attempt int `json:"attempt"`
	Max     int `json:"max"`
}

// State is a Session's snapshot. It is observational: nothing may be
// authorized from it. While a turn the harness started itself runs
// (capability autonomous_turns), it has that turn's TurnID and no InputID.
type State struct {
	Phase   Phase       `json:"phase"`
	TurnID  string      `json:"turn_id,omitempty"`
	InputID string      `json:"input_id,omitempty"`
	Prompt  *PromptInfo `json:"prompt,omitempty"`
	Block   *Block      `json:"block,omitempty"`
	Retry   *RetryInfo  `json:"retry,omitempty"`
	// Background is the work the harness runs in the background now
	// (background_turns, 1.4), whatever the phase.
	Background []BackgroundTask `json:"background,omitempty"`
}

// CloseReason is why the Host closes a Session.
type CloseReason string

// Close reasons.
const (
	ClosePark    CloseReason = "park"
	CloseDelete  CloseReason = "delete"
	CloseUpgrade CloseReason = "upgrade"
	CloseReset   CloseReason = "reset"
)

// Values lists the set.
func (CloseReason) Values() []string { return []string{"park", "delete", "upgrade", "reset"} }

// CloseResult is what Close established.
type CloseResult struct {
	// Stopped: every process of the harness's process group exited. False
	// when the adapter cannot confirm it. Only the Supervisor, emptying the
	// agent's isolation unit, confirms the workload is gone.
	Stopped bool `json:"stopped"`
	// Drained: the Host acknowledged every final batch, the exit observation
	// included. False when the drain time ran out first; the unacknowledged
	// record items stay in the record.
	Drained bool `json:"drained"`
}

// RecordRequest is what OpenRecord takes.
type RecordRequest struct {
	SessionID  string      `json:"session_id"`
	OpenConfig []byte      `json:"open_config"`
	Layout     Layout      `json:"layout"`
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
}

// RecoveredOutcome is what Recover found.
type RecoveredOutcome string

// Recovered outcomes.
const (
	RecoveredCompleted   RecoveredOutcome = "completed"
	RecoveredErrored     RecoveredOutcome = "errored"
	RecoveredInterrupted RecoveredOutcome = "interrupted"
	RecoveredCancelled   RecoveredOutcome = "cancelled"
	RecoveredRefused     RecoveredOutcome = "refused"
	// RecoveredNotFound: the intact marker store has no marker for the input,
	// so it was never handed to the harness and can run safely.
	RecoveredNotFound RecoveredOutcome = "not_found"
	// RecoveredUnknown: the record cannot prove the input's outcome, nor that
	// it was never submitted.
	RecoveredUnknown RecoveredOutcome = "unknown"
)

// Values lists the set.
func (RecoveredOutcome) Values() []string {
	return []string{"completed", "errored", "interrupted", "cancelled", "refused", "not_found", "unknown"}
}

// Recovered is Recover's answer.
type Recovered struct {
	Outcome RecoveredOutcome `json:"outcome"`
	TurnID  string           `json:"turn_id,omitempty"`
}

// ValidID reports whether s is an id: 1–128 characters of [A-Za-z0-9._:-].
func ValidID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != ':' && r != '-' {
			return false
		}
	}
	return true
}
