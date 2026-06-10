package main

import (
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
)

type runTurnRequest struct {
	Harness        string   `json:"harness"`
	TurnHarness    string   `json:"turn_harness,omitempty"`
	BinaryPath     string   `json:"binary_path"`
	Args           []string `json:"args,omitempty"`
	WorkingDir     string   `json:"working_dir,omitempty"`
	Env            []string `json:"env,omitempty"`
	Prompt         string   `json:"prompt"`
	ExitAfterTurn  *bool    `json:"exit_after_turn,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Cols           int      `json:"cols,omitempty"`
	Rows           int      `json:"rows,omitempty"`
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
	Harness    string   `json:"harness"`
	BinaryPath string   `json:"binary_path"`
	Args       []string `json:"args,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	Env        []string `json:"env,omitempty"`
	Cols       int      `json:"cols,omitempty"`
	Rows       int      `json:"rows,omitempty"`
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

type sendRequest struct {
	Token string `json:"token"`
	Text  string `json:"text"`
}

type sendResponse struct {
	TurnID string `json:"turn_id"`
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

func toTurnEventDTO(ev chat.TurnEvent) turnEventDTO {
	out := turnEventDTO{Turn: toTurnDTO(ev.Turn)}
	if ev.Err != nil {
		out.Error = ev.Err.Error()
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
