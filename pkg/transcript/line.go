// Transcript line types + content blocks for parsing JSONL transcripts written
// by AI coding agents (Claude Code, Cursor).
//
// Ported from github.com/entireio/cli (MIT, (c) 2026 Entire Inc.) via loomcli's
// internal/sessions/transcript. See ORIGIN.md for attribution and
// LICENSE.upstream for the upstream notice.

package transcript

import "encoding/json"

// Message type constants for transcript lines.
const (
	TypeUser      = "user"
	TypeAssistant = "assistant"
)

// Content type constants for content blocks within messages.
const (
	ContentTypeText    = "text"
	ContentTypeToolUse = "tool_use"
)

// Line represents a single line in a Claude Code or Cursor JSONL transcript.
// Claude Code uses "type" to distinguish user/assistant messages; Cursor uses
// "role" for the same purpose (see normalizeLineType).
type Line struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	UUID    string          `json:"uuid"`
	Message json.RawMessage `json:"message"`
	// Timestamp is the top-level RFC3339 timestamp Claude writes on each line
	// (e.g. "2026-04-17T18:43:16.594Z"). Optional — not every backend provides it.
	Timestamp string `json:"timestamp,omitempty"`

	// IsAPIErrorMessage marks a SYNTHETIC assistant line: Claude Code writes
	// one when an API call failed, with model "<synthetic>" and the rendered
	// error text as its only content block. It is not a reply, and reading it
	// as one is how a failed turn comes back as a success whose "answer" is
	// "API Error: 529 Overloaded".
	IsAPIErrorMessage bool `json:"isApiErrorMessage,omitempty"`

	// Error is the harness's OWN machine-readable verdict for that failure,
	// written beside IsAPIErrorMessage: authentication_failed,
	// oauth_org_not_allowed, account_on_hold, verification_required,
	// billing_error, rate_limit, overloaded, invalid_request, model_not_found,
	// server_error, max_output_tokens, cloud_credential_error, unknown.
	//
	// It is the reason this parser bothers to decode these two fields at all.
	// Everything downstream that wants to know why a turn failed — screen
	// regexes, residual patterns, an operator reading a log — is re-deriving a
	// fact the harness already wrote down here and every layer then dropped.
	// The vocabulary is Claude Code's, stable across 2.1.181 → 2.1.278; a value
	// outside it is carried verbatim, never guessed at.
	Error string `json:"error,omitempty"`
}

// UserMessage represents a user message in the transcript.
type UserMessage struct {
	Content interface{} `json:"content"`
}

// AssistantMessage represents an assistant message in the transcript.
type AssistantMessage struct {
	Content []ContentBlock `json:"content"`
}

// ContentBlock represents a block within an assistant message.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolInput represents the input to various tools. Used to extract file paths
// and descriptions from tool calls.
type ToolInput struct {
	FilePath     string `json:"file_path,omitempty"`
	NotebookPath string `json:"notebook_path,omitempty"`
	Description  string `json:"description,omitempty"`
	Command      string `json:"command,omitempty"`
	Pattern      string `json:"pattern,omitempty"`
	// Skill tool fields
	Skill string `json:"skill,omitempty"`
	// WebFetch tool fields
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}
