package main

import (
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
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
	// PermissionMode is the launch-time permission rung threaded to
	// wrapper.Config (claude --permission-mode, codex -s/-a). Canonical
	// rungs: "plan", "manual", "ask", "auto", "bypass" (per-harness native
	// spellings also accepted). Empty leaves the harness default.
	//
	// Restrictive rungs (`plan`, `manual`, `ask`) are fully enforced only
	// when a human is at the TUI (passthrough, or `run` from a terminal for
	// codex). Under `structured-run` and unattended `run`, claude's
	// permission dialogs are not detected (the turn stalls to the deadline)
	// and codex's approval prompts are auto-approved (only the `-s` sandbox
	// axis still binds).
	PermissionMode string `json:"permission_mode,omitempty"`
	// Containment requests Landlock containment for this one-shot turn: the
	// complete request object (see containment.Request). Omitted runs the
	// turn uncontained, exactly as before. Clients send it only after GET
	// /v1/capabilities lists the kind: a chatd built before containment
	// silently ignores unknown fields.
	Containment *containment.Request `json:"containment,omitempty"`
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
	// Containment echoes the applied containment policy of a contained turn,
	// including its cleanup outcome; omitted for an uncontained one.
	Containment *containment.Applied `json:"containment,omitempty"`
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
	// Effort / Model / PermissionMode are execution-mode controls threaded to
	// wrapper.Config (claude --effort/--model/--permission-mode, codex config
	// overrides and -s/-a). Omitted leaves the harness default.
	Effort string `json:"effort,omitempty"`
	Model  string `json:"model,omitempty"`
	// PermissionMode's canonical rungs are "plan", "manual", "ask", "auto",
	// "bypass" (per-harness native spellings also accepted). Empty leaves the
	// harness default.
	//
	// Restrictive rungs (`plan`, `manual`, `ask`) are fully enforced only
	// when a human is at the TUI (passthrough, or `run` from a terminal for
	// codex). Under `structured-run` and unattended `run`, claude's
	// permission dialogs are not detected (the turn stalls to the deadline)
	// and codex's approval prompts are auto-approved (only the `-s` sandbox
	// axis still binds).
	//
	// `bypass` caveat: unlike --sandbox-defaults (cmd/harness-wrapper), which
	// contributes both args and env (IS_SANDBOX=1, suppressing claude-code's
	// Bypass Permissions mode acceptance screen and allowing root), chatd has
	// no --sandbox-defaults equivalent — so a caller asking for `bypass` must
	// either pass IS_SANDBOX=1 in Env or supply an InputPolicy with
	// ByKind{"bypass_acceptance": ...}, otherwise the harness stops on the
	// acceptance screen (surfaced as a bypass_acceptance input request) and root
	// is disallowed. Note the kind: "bypass_acceptance" is the acceptance
	// screen's own kind, distinct from the folder-trust dialog's
	// "trust_prompt" — a policy that answers only trust_prompt does NOT cover
	// this screen (and, deliberately, no longer accepts a bypass launch on a
	// folder-trust entry's behalf).
	PermissionMode string `json:"permission_mode,omitempty"`
	// DisableCodexAutoDismiss disables the built-in auto-dismissal of Codex's
	// choice-free startup interstitials (model migration, menu-less notices).
	// Omitted/false keeps auto-dismiss on.
	DisableCodexAutoDismiss bool `json:"disable_codex_auto_dismiss,omitempty"`
	// AutoSkipCodexUpdateNotice auto-Skips Codex's "Update available!" menu
	// instead of surfacing it as an input_request. Omitted/false surfaces the
	// menu so a remote client can choose Update / Skip.
	AutoSkipCodexUpdateNotice bool `json:"auto_skip_codex_update_notice,omitempty"`
	// Containment requests Landlock containment for the conversation: the
	// complete request object. Chosen at open and fixed for the
	// conversation's life. Omitted opens it uncontained, exactly as before.
	// Clients send it only after GET /v1/capabilities lists the kind.
	Containment *containment.Request `json:"containment,omitempty"`
}

type openResponse struct {
	ID string `json:"id"`
	// Containment echoes the applied containment policy — a caller that
	// sends containment without checking the capability route learns here
	// whether it was honoured. Omitted for an uncontained conversation.
	Containment *containment.Applied `json:"containment,omitempty"`
}

// capabilitiesResponse is GET /v1/capabilities: what this chatd build can
// honour. A chatd built before containment has no such route (404), and the
// clients then refuse to send containment rather than let it be ignored.
type capabilitiesResponse struct {
	Containment containmentCapabilities `json:"containment"`
}

type containmentCapabilities struct {
	// Kinds lists the containment kinds this build supports on this host's
	// platform; empty where none is (anything but Linux). Kernel support is
	// checked per launch, and a launch that cannot enforce is refused.
	Kinds []string `json:"kinds"`
}

type conversationSummary struct {
	ID        string `json:"id"`
	Harness   string `json:"harness"`
	SessionID string `json:"session_id,omitempty"`
	// PermissionMode is the mode the conversation was OPENED with — the value
	// from openRequest, not an observed or live reading of the harness.
	//
	// For every value chatd accepts, requested == effective at launch:
	// wrapper.validateConfig *rejects* (rather than no-ops) an unknown mode, a
	// cross-harness native spelling, `plan` on codex, a non-empty mode on an
	// unsupported harness, and mode/argv contradictions — and chatd maps that
	// rejection to 400 invalid_config, which is what makes this field honest.
	//
	// Two residual gaps keep "requested" from being a guarantee of "effective"
	// for the lifetime of the conversation:
	//
	// 1. Explicit-flag-wins. The wrapper skips injection when Args already
	// carries a permission-axis flag (matched in three spellings: bare token
	// `-s`, attached long `--sandbox=read-only`, clap attached short
	// `-sread-only`). Suppression sets: claude/claude-code —
	// --permission-mode, --dangerously-skip-permissions; codex — -s,
	// --sandbox, -a, --ask-for-approval,
	// --dangerously-bypass-approvals-and-sandbox. The two --dangerously-*
	// arms are reachable only when the requested mode is bypass-class; any
	// other mode paired with them is rejected (400), not silently suppressed.
	// So an explicit permission-axis flag in `args` wins over
	// `permission_mode`, and this field still reports what was requested.
	//
	// 2. In-band mutation. A client holding the control token can POST
	// arbitrary text to /v1/conversations/{id}/messages — claude's
	// /permissions, codex's /approvals — and flip the mode inside the TUI.
	// Nothing validates or observes that; this field keeps reporting the
	// open-time value indefinitely.
	PermissionMode string `json:"permission_mode,omitempty"`
	// Containment is the applied containment policy the conversation runs
	// under; omitted for an uncontained conversation.
	Containment *containment.Applied `json:"containment,omitempty"`
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
	// Containment may restate a contained conversation's policy: it must
	// normalize to the same policy. On an uncontained conversation any
	// containment is refused (400) before the text reaches the harness —
	// containment is chosen at open, never added to a running conversation.
	Containment *containment.Request `json:"containment,omitempty"`
}

type sendResponse struct {
	TurnID string `json:"turn_id"`
}

// answerRequest answers a pending interactive prompt. RequestID is optional
// (empty targets whatever prompt is currently pending). Set OptionID (id or
// alias) for single-select menu/trust prompts, Text for free-text prompts, or
// OptionIDs (each an id or alias) to select one or more options on a
// multi_select prompt. OptionID and OptionIDs are mutually exclusive.
type answerRequest struct {
	Token     string   `json:"token"`
	RequestID string   `json:"request_id,omitempty"`
	OptionID  string   `json:"option_id,omitempty"`
	OptionIDs []string `json:"option_ids,omitempty"`
	Text      string   `json:"text,omitempty"`
}

type inputOptionDTO struct {
	ID          string `json:"id"`
	Alias       string `json:"alias,omitempty"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type inputRequestDTO struct {
	ID          string           `json:"id"`
	Kind        string           `json:"kind"`
	Prompt      string           `json:"prompt"`
	Header      string           `json:"header,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	Options     []inputOptionDTO `json:"options,omitempty"`
}

// eventDTO is the typed SSE envelope. Type discriminates the payload:
// "turn" → Turn set; "input_request"/"input_resolved" → Input set;
// "exited" → Exit set, the last frame. Turn frames keep the same "turn" object
// shape as before, so existing consumers that read .turn remain compatible.
type eventDTO struct {
	Type  string           `json:"type"`
	Turn  *turnDTO         `json:"turn,omitempty"`
	Input *inputRequestDTO `json:"input,omitempty"`
	Exit  *exitDTO         `json:"exit,omitempty"`
	Error string           `json:"error,omitempty"`
}

// exitDTO is how the harness process ended (chat.ExitInfo), in the "exited"
// frame. Class is the wrapper's error class by name ("RateLimited", …),
// omitted when there is none.
type exitDTO struct {
	Status   string    `json:"status"`
	ExitCode int       `json:"exit_code"`
	Signal   string    `json:"signal,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Class    string    `json:"class,omitempty"`
	EndedAt  time.Time `json:"ended_at,omitzero"`
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

// interruptResponse answers POST /v1/conversations/{id}/interrupt: what the
// interrupt did — stopped, cancelled, no_turn or too_late. Error is set only
// with cancelled, when the prompt the harness put back would not clear from its
// composer.
type interruptResponse struct {
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
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
	out := &inputRequestDTO{ID: r.ID, Kind: r.Kind, Prompt: r.Prompt, Header: r.Header, MultiSelect: r.MultiSelect}
	for _, o := range r.Options {
		out.Options = append(out.Options, inputOptionDTO{ID: o.ID, Alias: o.Alias, Label: o.Label, Description: o.Description})
	}
	return out
}

// toEventDTO maps an event to its SSE frame; ok is false for a type this build
// does not know, which must not reach the wire dressed as a turn.
func toEventDTO(ev chat.ConversationEvent) (eventDTO, bool) {
	out := eventDTO{Type: string(ev.Type)}
	if ev.Err != nil {
		out.Error = ev.Err.Error()
	}
	switch ev.Type {
	case chat.EventTurn:
		t := toTurnDTO(ev.Turn)
		out.Turn = &t
	case chat.EventInputRequest, chat.EventInputResolved:
		out.Input = toInputRequestDTO(ev.Input)
	case chat.EventExited:
		if e := ev.Exit; e != nil {
			out.Exit = &exitDTO{
				Status: string(e.Status), ExitCode: e.ExitCode, Signal: e.Signal,
				Reason: e.Reason, EndedAt: e.EndedAt,
			}
			if e.Class != wrapper.ErrNone {
				out.Exit.Class = e.Class.String()
			}
		}
	default:
		return eventDTO{}, false
	}
	return out, true
}

func toSessionDTO(s chat.Session) sessionDTO {
	return sessionDTO{
		ID:               s.ID,
		Harness:          s.Harness,
		WorkingDir:       s.WorkingDir,
		CreatedAt:        s.CreatedAt,
		HarnessSessionID: s.HarnessID(), // containment-aware
	}
}
