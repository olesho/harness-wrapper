package pi

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// The Harness Adapter's Pi profile reads a session as pi writes it: a
// follower over the session's file (transcript.Follower), and DecodeEntry,
// which gives each line's events with the facts an input's place and its
// run's end are read from. Read, above, keeps the whole-file view.

// TagType is the customType of the entry the Pi profile's tag extension
// writes before each input's user message, with the input's tag as data.id.
const TagType = "hw.input"

// EventEntry is the Type of the one event DecodeEntry gives a line that
// holds facts and no content: a tag, a system message, a context edit, an
// assistant message with neither text nor a tool call.
const EventEntry = "entry"

// Entry is what a session line says beyond its events. FollowSession
// attaches one to each of a line's events as its Meta.
type Entry struct {
	// Type is the line's type (message, custom, context_edit, …), and Role a
	// message's role (user, assistant, toolResult, system).
	Type, Role string
	// ID and ParentID are the entry's id and its parent's. A session is a
	// tree: an input's tag is its user message's parent, or its grandparent
	// through the system message pi writes when the system prompt changed.
	ID, ParentID string
	// Tag is the input tag a custom entry of TagType carries.
	Tag string
	// StopReason and ErrorMessage are an assistant message's: stop, toolUse,
	// error, aborted, length.
	StopReason, ErrorMessage string
	// ToolCallID and IsError are a tool result's.
	ToolCallID string
	IsError    bool
	// TargetID is the entry a context_edit edits: a failed attempt pi
	// retried drops out of the context that way.
	TargetID string
}

// SessionFile is session sessionID's file in dir, the --session-dir pi ran
// with: <dir>/<timestamp>_<id>.jsonl, confirmed by its header. It is
// fs.ErrNotExist until pi writes it, with the session's first user message.
func SessionFile(dir, sessionID string) (string, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return "", fmt.Errorf("pi: %q is not a session id", sessionID)
	}
	path, found, err := findInDir(dir, sessionID)
	switch {
	case err != nil:
		return "", err
	case !found:
		return "", fmt.Errorf("pi: no session file for %s in %s: %w", sessionID, dir, fs.ErrNotExist)
	}
	return path, nil
}

// FollowSession follows the session file at path from a checkpoint, with
// DecodeEntry: every event carries its line's *Entry as Meta.
func FollowSession(path, sessionID string, from transcript.Checkpoint) (*transcript.Follower, error) {
	return transcript.NewFollower(path, sessionID, from, DecodeEntry)
}

// sessionLine is a session line as DecodeEntry reads it.
type sessionLine struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	ParentID   *string         `json:"parentId"`
	Timestamp  string          `json:"timestamp"`
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
	TargetID   string          `json:"targetId"`
	Message    *struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		ToolCallID   string          `json:"toolCallId"`
		ToolName     string          `json:"toolName"`
		IsError      bool            `json:"isError"`
	} `json:"message"`
}

// block is one content block of a message.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// blocks reads a message's content: a bare string is one text block.
func blocks(content json.RawMessage) []block {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var out []block
	_ = json.Unmarshal(content, &out)
	return out
}

// DecodeEntry decodes one session line into its events, each with the
// line's *Entry as Meta, and the entry's id as each event's UUID, so the
// follower knows an event by pi's own id:
//
//   - a user message's text;
//   - an assistant message's text blocks and tool calls;
//   - a tool result's output;
//   - one event of Type EventEntry for a line that holds facts and no
//     content: an input's tag, a system message, a context edit, and an
//     assistant message with neither text nor a tool call (a failed or
//     aborted one, say).
//
// The header and pi's other entries (model and thinking-level changes,
// usage, labels, …) have no events.
func DecodeEntry(record []byte) ([]transcript.BlockEvent, error) {
	var ln sessionLine
	if err := json.Unmarshal(record, &ln); err != nil {
		return nil, err
	}
	e := &Entry{Type: ln.Type, ID: ln.ID, TargetID: ln.TargetID}
	if ln.ParentID != nil {
		e.ParentID = *ln.ParentID
	}
	ts, _ := time.Parse(time.RFC3339Nano, ln.Timestamp)
	one := func(i int, ev transcript.Event) transcript.BlockEvent {
		ev.Timestamp, ev.Source, ev.UUID = ts, transcript.SourceFile, ln.ID
		return transcript.BlockEvent{Block: i, Event: ev, Meta: e}
	}
	entry := []transcript.BlockEvent{one(0, transcript.Event{Type: EventEntry})}
	switch ln.Type {
	case "custom":
		e.Role = "custom"
		if ln.CustomType != TagType {
			return nil, nil
		}
		var d struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(ln.Data, &d)
		e.Tag = d.ID
		return entry, nil
	case "context_edit":
		return entry, nil
	case "message":
	default:
		return nil, nil
	}
	if ln.Message == nil {
		return nil, nil
	}
	m := ln.Message
	e.Role, e.StopReason, e.ErrorMessage = m.Role, m.StopReason, m.ErrorMessage
	var out []transcript.BlockEvent
	switch m.Role {
	case "user":
		var text []string
		for _, b := range blocks(m.Content) {
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		out = append(out, one(0, transcript.Event{Role: transcript.RoleUser, Type: transcript.EventText, Text: strings.Join(text, "\n")}))
	case "assistant":
		for i, b := range blocks(m.Content) {
			switch b.Type {
			case "text":
				if b.Text != "" {
					out = append(out, one(i, transcript.Event{Role: transcript.RoleAssistant, Type: transcript.EventText, Text: b.Text}))
				}
			case "toolCall":
				input := b.Arguments
				if !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				out = append(out, one(i, transcript.Event{
					Role: transcript.RoleAssistant, Type: transcript.EventToolUse, ToolName: b.Name, ToolUseID: b.ID, ToolInput: input,
				}))
			}
		}
	case "toolResult":
		e.ToolCallID, e.IsError = m.ToolCallID, m.IsError
		var text []string
		for _, b := range blocks(m.Content) {
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		out = append(out, one(0, transcript.Event{
			Role: transcript.RoleTool, Type: transcript.EventToolResult, ToolName: m.ToolName, ToolUseID: m.ToolCallID, Output: strings.Join(text, ""),
		}))
	case "system":
		return entry, nil
	default:
		return nil, nil
	}
	if len(out) == 0 {
		return entry, nil
	}
	return out, nil
}
