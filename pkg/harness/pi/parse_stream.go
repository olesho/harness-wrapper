// pi live `--mode json` → canonical event parser (the StreamParser capability).
//
// Validated against pi 0.76.0 (cerebras/gpt-oss-120b). The `pi -p --mode json`
// stream is a sequence of typed JSON lines:
//
//	session → agent_start → turn_start
//	  → message_start → message_update* → message_end   (one per message)
//	  → tool_execution_start → tool_execution_end        (per tool call)
//	  → turn_end → agent_end
//
// The parser keys on `message_end` ALONE, which carries the COMPLETE message:
//   - message_update lines are streaming deltas (partial) — skipped.
//   - turn_end carries a verbatim copy of the turn's final message_end — skipping
//     it is what prevents a doubled final message (the de-dup question that
//     gated this parser until a live capture settled it).
//   - tool_execution_* are redundant with the role:"toolResult" message_end,
//     which already carries the result text plus its toolCallId linkage.
//
// Message roles: "user", "assistant", "toolResult". Content blocks: "text"
// (kept), "thinking" (reasoning — dropped, matching the on-disk reader), and
// "toolCall" ({id,name,arguments} — an assistant tool invocation). The session
// id is NOT on message lines (it rides the session header, captured separately
// by sessionIDExtractor), so ParsedEvent.HarnessSessionID is left empty and the
// orchestrator backfills it from the captured header id.
//
// Lines that are not parseable message_end events (other types, ANSI-polluted,
// non-JSON) yield nil, because the durable line tap delivers RAW PTY bytes.
package pi

import (
	"encoding/json"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// streamParser implements harness.StreamParser for pi's `--mode json` stdout.
type streamParser struct{}

// streamLine is the minimal shape of one `--mode json` record.
type streamLine struct {
	Type    string         `json:"type"`
	Message *streamMessage `json:"message"`
}

// streamMessage is the message payload on a message_end line. ToolCallID/ToolName
// are populated only on role:"toolResult" messages, linking the result to its
// originating toolCall block.
type streamMessage struct {
	Role       string        `json:"role"`
	Content    []streamBlock `json:"content"`
	ToolCallID string        `json:"toolCallId"`
	ToolName   string        `json:"toolName"`
}

// streamBlock is one content block. Fields are a union across block types: text
// carries Text; toolCall carries ID/Name/Arguments; thinking carries neither and
// is dropped.
type streamBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParseStreamLine implements harness.StreamParser. It returns the conversation
// events carried by a single `--mode json` line — but only for message_end (the
// complete-message event); every other line type yields nil (see package doc).
func (streamParser) ParseStreamLine(line string) []transcript.ParsedEvent {
	var sl streamLine
	if err := json.Unmarshal([]byte(line), &sl); err != nil || sl.Message == nil {
		return nil
	}
	if sl.Type != "message_end" {
		return nil
	}
	switch sl.Message.Role {
	case "user":
		return textEvents(sl.Message.Content, transcript.RoleUser)
	case "assistant":
		return assistantEvents(sl.Message.Content)
	case "toolResult":
		return toolResultEvents(sl.Message)
	default:
		return nil
	}
}

// textEvents projects the text blocks of a user/assistant message to EventText.
// thinking and any non-text blocks are dropped (matching the on-disk reader).
func textEvents(content []streamBlock, role string) []transcript.ParsedEvent {
	var out []transcript.ParsedEvent
	for i, b := range content {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		out = append(out, parsed(transcript.Event{
			Seq: i, Role: role, Type: transcript.EventText, Text: b.Text,
			Source: transcript.SourceLive,
		}))
	}
	return out
}

// assistantEvents projects an assistant message's blocks: text → EventText,
// toolCall → EventToolUse (id/name/arguments), thinking → dropped.
func assistantEvents(content []streamBlock) []transcript.ParsedEvent {
	var out []transcript.ParsedEvent
	for i, b := range content {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, parsed(transcript.Event{
				Seq: i, Role: transcript.RoleAssistant, Type: transcript.EventText, Text: b.Text,
				Source: transcript.SourceLive,
			}))
		case "toolCall":
			out = append(out, parsed(transcript.Event{
				Seq: i, Role: transcript.RoleAssistant, Type: transcript.EventToolUse,
				ToolName: b.Name, ToolUseID: b.ID, ToolInput: b.Arguments,
				Source: transcript.SourceLive, NativeID: "tool-use:" + b.ID,
			}))
		}
	}
	return out
}

// toolResultEvents projects a role:"toolResult" message to one EventToolResult,
// concatenating its text blocks as the output and carrying the toolCallId back to
// the originating tool_use so consumers can pair them.
func toolResultEvents(m *streamMessage) []transcript.ParsedEvent {
	var output string
	for _, b := range m.Content {
		if b.Type == "text" {
			output += b.Text
		}
	}
	return []transcript.ParsedEvent{parsed(transcript.Event{
		Role: transcript.RoleTool, Type: transcript.EventToolResult,
		Output: output, ToolName: m.ToolName, ToolUseID: m.ToolCallID,
		Source: transcript.SourceLive, NativeID: "tool-result:" + m.ToolCallID,
	})}
}

// parsed wraps an Event as a ParsedEvent. pi's message lines carry no session id
// (it rides the session header, captured by sessionIDExtractor), so
// HarnessSessionID is left empty for the orchestrator to backfill. pi has no
// subagent nesting on the live stream, so ParentSessionID is empty.
func parsed(e transcript.Event) transcript.ParsedEvent {
	return transcript.ParsedEvent{Event: e}
}
