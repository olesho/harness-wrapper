package codex

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Entry is what a rollout line says beyond its events: the facts a turn's
// input and its end are read from. FollowRollout attaches one to each event
// as its Meta.
type Entry struct {
	// Type is the line's type (session_meta, event_msg, response_item, …)
	// and Kind its payload's (task_started, user_message, task_complete,
	// turn_aborted, message, function_call, …).
	Type, Kind string
	// TurnID is the turn the line belongs to, when it names one.
	TurnID string
	// ClientID is the clientUserMessageId a user message was sent with:
	// codex 0.144 records it on user_message, 0.157 on the UserMessage of
	// an item_completed.
	ClientID string
	// MessageID is an assistant message's API id.
	MessageID string
	// LastAgentMessage is a task_complete's final reply; nil when the turn
	// ended with none.
	LastAgentMessage *string
	// AbortReason is a turn_aborted's reason: interrupted, ….
	AbortReason string
	// ErrorInfo and ErrorMessage are a failed turn's error, which codex
	// records on its task_complete from 0.157 (error.codex_error_info:
	// server_overloaded, usage_limit_exceeded, …). Before, a failed turn's
	// task_complete is one with no reply.
	ErrorInfo, ErrorMessage string
}

// EventEntry is the Type of the one event FollowRollout gives a line that
// holds facts and no events: a turn's start or end.
const EventEntry = "entry"

// Rollout is the path of a thread's rollout under codexHome, the CODEX_HOME
// codex ran with: sessions/<YYYY>/<MM>/<DD>/rollout-<timestamp>-<thread>.jsonl.
// It is fs.ErrNotExist until codex writes it, with the thread's first turn.
func Rollout(codexHome, threadID string) (string, error) {
	root := filepath.Join(codexHome, "sessions")
	if threadID == "" || strings.ContainsAny(threadID, `/\`) {
		return "", fmt.Errorf("codex: %q is not a thread id", threadID)
	}
	suffix := "-" + threadID + ".jsonl"
	var found string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), suffix) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	switch {
	case err != nil:
		return "", err
	case found == "":
		return "", fmt.Errorf("codex: no rollout of thread %s under %s: %w", threadID, root, fs.ErrNotExist)
	}
	return found, nil
}

// FollowRollout follows the rollout at path from a checkpoint, with
// DecodeEntry: every event carries its line's Entry.
func FollowRollout(path, threadID string, from transcript.Checkpoint) (*transcript.Follower, error) {
	return transcript.NewFollower(path, threadID, from, DecodeEntry)
}

// DecodeEntry decodes one rollout line into its events, each with the line's
// *Entry as Meta: a user message sent with a client id, an assistant
// message's text blocks, a tool call and its output, and one event of Type
// EventEntry for a turn's start or end. Other lines have no events.
func DecodeEntry(record []byte) ([]transcript.BlockEvent, error) {
	var env Envelope
	if err := json.Unmarshal(record, &env); err != nil {
		return nil, err
	}
	e := &Entry{Type: env.Type}
	ts := parseCodexTime(env.Timestamp)
	one := func(block int, ev transcript.Event) transcript.BlockEvent {
		ev.Timestamp, ev.Source = ts, transcript.SourceFile
		return transcript.BlockEvent{Block: block, Event: ev, Meta: e}
	}
	switch env.Type {
	case "event_msg":
		var p struct {
			Type             string          `json:"type"`
			TurnID           string          `json:"turn_id"`
			ClientID         string          `json:"client_id"`
			Message          string          `json:"message"`
			LastAgentMessage *string         `json:"last_agent_message"`
			Reason           string          `json:"reason"`
			Error            *eventError     `json:"error"`
			Item             json.RawMessage `json:"item"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			return nil, nil
		}
		e.Kind, e.TurnID = p.Type, p.TurnID
		switch p.Type {
		case "user_message":
			e.ClientID = p.ClientID
			return []transcript.BlockEvent{one(0, transcript.Event{Role: transcript.RoleUser, Type: transcript.EventText, Text: p.Message})}, nil
		case "item_completed":
			var it struct {
				Type     string         `json:"type"`
				ClientID string         `json:"client_id"`
				Content  []contentBlock `json:"content"`
			}
			if json.Unmarshal(p.Item, &it) != nil || it.Type != "UserMessage" {
				return nil, nil
			}
			var text []string
			for _, c := range it.Content {
				text = append(text, c.Text)
			}
			e.ClientID = it.ClientID
			return []transcript.BlockEvent{one(0, transcript.Event{Role: transcript.RoleUser, Type: transcript.EventText, Text: strings.Join(text, "\n")})}, nil
		case "task_complete":
			e.LastAgentMessage = p.LastAgentMessage
			if p.Error != nil {
				e.ErrorInfo, e.ErrorMessage = p.Error.info(), p.Error.Message
			}
		case "turn_aborted":
			e.AbortReason = p.Reason
		case "task_started":
		default:
			return nil, nil
		}
		return []transcript.BlockEvent{one(0, transcript.Event{Type: EventEntry})}, nil
	case "response_item":
		var item struct {
			responseItem
			ID string `json:"id"`
		}
		if json.Unmarshal(env.Payload, &item) != nil {
			return nil, nil
		}
		e.Kind = item.Type
		switch item.Type {
		case "message":
			if item.Role != transcript.RoleAssistant {
				return nil, nil
			}
			e.MessageID = item.ID
			var out []transcript.BlockEvent
			for i, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					out = append(out, one(i, transcript.Event{Role: transcript.RoleAssistant, Type: transcript.EventText, Text: c.Text}))
				}
			}
			return out, nil
		case "function_call", "custom_tool_call":
			input := json.RawMessage(item.Arguments)
			if item.Type == "custom_tool_call" {
				input = customToolInput(item.Input)
			}
			if !json.Valid(input) {
				b, _ := json.Marshal(string(input))
				input = b
			}
			return []transcript.BlockEvent{one(0, transcript.Event{
				Role: transcript.RoleAssistant, Type: transcript.EventToolUse, ToolName: item.Name, ToolUseID: item.CallID, ToolInput: input,
			})}, nil
		case "function_call_output", "custom_tool_call_output":
			out := decodeFunctionOutput(item.Output)
			if item.Type == "custom_tool_call_output" {
				out = decodeCustomToolOutput(item.Output)
			}
			return []transcript.BlockEvent{one(0, transcript.Event{
				Role: transcript.RoleTool, Type: transcript.EventToolResult, ToolUseID: item.CallID, Output: out,
			})}, nil
		}
	}
	return nil, nil
}

// eventError is a failed turn's error on its task_complete.
type eventError struct {
	Message string          `json:"message"`
	Info    json.RawMessage `json:"codex_error_info"`
}

// info names the error: codex_error_info's value, or the one key of its
// object form ({"response_stream_disconnected": {…}}).
func (e *eventError) info() string {
	var s string
	if json.Unmarshal(e.Info, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(e.Info, &m) == nil {
		for k := range m {
			return k
		}
	}
	return ""
}
