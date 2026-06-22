package main

import (
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
)

type runTurnRequest struct {
	Harness        string            `json:"harness"`
	TurnHarness    string            `json:"turn_harness,omitempty"`
	BinaryPath     string            `json:"binary_path"`
	Args           []string          `json:"args,omitempty"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	Env            []string          `json:"env,omitempty"`
	Prompt         string            `json:"prompt"`
	ExitAfterTurn  *bool             `json:"exit_after_turn,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Cols           int               `json:"cols,omitempty"`
	Rows           int               `json:"rows,omitempty"`
	InputPolicy    *chat.InputPolicy `json:"input_policy,omitempty"`
	Effort         string            `json:"effort,omitempty"`
	Model          string            `json:"model,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
}

type sessionDTO struct {
	ID               string    `json:"id"`
	Harness          string    `json:"harness"`
	WorkingDir       string    `json:"working_dir,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	HarnessSessionID string    `json:"harness_session_id,omitempty"`
}

type runTurnResponse struct {
	Turn                    turnDTO    `json:"turn"`
	Session                 sessionDTO `json:"session"`
	History                 []turnDTO  `json:"history"`
	ProcessStoppedAfterTurn bool       `json:"process_stopped_after_turn"`
	WrapperStatus           string     `json:"wrapper_status,omitempty"`
	WrapperReason           string     `json:"wrapper_reason,omitempty"`
	Error                   string     `json:"error,omitempty"`
}

type openRequest struct {
	Harness     string            `json:"harness"`
	BinaryPath  string            `json:"binary_path"`
	Args        []string          `json:"args,omitempty"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	Env         []string          `json:"env,omitempty"`
	Cols        int               `json:"cols,omitempty"`
	Rows        int               `json:"rows,omitempty"`
	InputPolicy *chat.InputPolicy `json:"input_policy,omitempty"`
	// Effort / Model / MaxTokens are execution-mode controls threaded to
	// wrapper.Config (claude --effort/--model, codex config overrides, token cap
	// best-effort). Omitted/zero leaves the harness default.
	Effort    string `json:"effort,omitempty"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// DisableCodexAutoDismiss disables the built-in auto-dismissal of Codex's
	// blocking startup interstitials. Omitted/false keeps auto-dismiss on.
	DisableCodexAutoDismiss bool `json:"disable_codex_auto_dismiss,omitempty"`
}

type openResponse struct {
	ID string `json:"id"`
}

type conversationSummary struct {
	ID        string `json:"id"`
	Harness   string `json:"harness"`
	SessionID string `json:"session_id,omitempty"`
}

type controlResponse struct {
	Token string `json:"token"`
}

// screenResponse is the rendered-terminal snapshot returned by
// GET /v1/conversations/{id}/screen. Generation lets pollers skip no-op
// redraws (compare against the previous response).
type screenResponse struct {
	Text       string `json:"text"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
	CursorCol  int    `json:"cursor_col"`
	CursorRow  int    `json:"cursor_row"`
	Generation uint64 `json:"generation"`
}

type sendRequest struct {
	Token string `json:"token"`
	Text  string `json:"text"`
}

type sendResponse struct {
	TurnID string `json:"turn_id"`
}

// answerRequest answers a pending interactive prompt. RequestID is optional
// (empty targets whatever prompt is currently pending). Set OptionID (id or
// alias) for menu/trust prompts, or Text for free-text prompts.
type answerRequest struct {
	Token     string `json:"token"`
	RequestID string `json:"request_id,omitempty"`
	OptionID  string `json:"option_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

type inputOptionDTO struct {
	ID    string `json:"id"`
	Alias string `json:"alias,omitempty"`
	Label string `json:"label"`
}

type inputRequestDTO struct {
	ID      string           `json:"id"`
	Kind    string           `json:"kind"`
	Prompt  string           `json:"prompt"`
	Options []inputOptionDTO `json:"options,omitempty"`
}

// eventDTO is the typed SSE envelope. Type discriminates the payload:
// "turn" → Turn set; "input_request"/"input_resolved" → Input set. Turn
// frames keep the same "turn" object shape as before, so existing consumers
// that read .turn remain compatible.
type eventDTO struct {
	Type  string           `json:"type"`
	Turn  *turnDTO         `json:"turn,omitempty"`
	Input *inputRequestDTO `json:"input,omitempty"`
	Error string           `json:"error,omitempty"`
}

type turnDTO struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	Role        string    `json:"role"`
	State       string    `json:"state"`
	Text        string    `json:"text,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitzero"`

	// HTTPCode is the upstream API status code parsed from a Blocked
	// turn caused by an api_error wrapper event. Omitted (zero) for
	// non-api blocks and for transport errors without a numeric code.
	HTTPCode int `json:"http_code,omitempty"`

	// RetryAfter is a Go duration string ("30s", "2m") the harness
	// suggested as a backoff. Omitted when no hint was parseable.
	RetryAfter string `json:"retry_after,omitempty"`
}

type turnEventDTO struct {
	Turn  turnDTO `json:"turn"`
	Error string  `json:"error,omitempty"`
}

type historyResponse struct {
	Turns []turnDTO `json:"turns"`
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func toTurnDTO(t chat.Turn) turnDTO {
	out := turnDTO{
		ID:          t.ID,
		SessionID:   t.SessionID,
		Role:        string(t.Role),
		State:       string(t.State),
		Text:        t.Text,
		Reason:      t.Reason,
		StartedAt:   t.StartedAt,
		CompletedAt: t.CompletedAt,
		HTTPCode:    t.HTTPCode,
	}
	if t.RetryAfter > 0 {
		out.RetryAfter = t.RetryAfter.String()
	}
	return out
}

func toInputRequestDTO(r *chat.InputRequest) *inputRequestDTO {
	if r == nil {
		return nil
	}
	out := &inputRequestDTO{ID: r.ID, Kind: r.Kind, Prompt: r.Prompt}
	for _, o := range r.Options {
		out.Options = append(out.Options, inputOptionDTO{ID: o.ID, Alias: o.Alias, Label: o.Label})
	}
	return out
}

func toEventDTO(ev chat.ConversationEvent) eventDTO {
	out := eventDTO{Type: string(ev.Type)}
	if ev.Err != nil {
		out.Error = ev.Err.Error()
	}
	switch ev.Type {
	case chat.EventInputRequest, chat.EventInputResolved:
		out.Input = toInputRequestDTO(ev.Input)
	default: // EventTurn (and any future turn-shaped event)
		t := toTurnDTO(ev.Turn)
		out.Turn = &t
	}
	return out
}

func toSessionDTO(s chat.Session) sessionDTO {
	return sessionDTO{
		ID:               s.ID,
		Harness:          s.Harness,
		WorkingDir:       s.WorkingDir,
		CreatedAt:        s.CreatedAt,
		HarnessSessionID: s.HarnessSessionID,
	}
}
