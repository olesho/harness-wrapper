// Claude Code live stream-json → canonical event parser (the StreamParser
// capability). This is the LIVE counterpart to the on-disk file parser in
// pkg/transcript/claudecode: it reads `claude -p --output-format stream-json`
// stdout, whose schema DIFFERS from the on-disk JSONL —
//
//	live:   {"type":"assistant","session_id":"...","message":{"id":"msg_..","content":[...]}}
//	on-disk:{"type":"assistant","uuid":"..","timestamp":"..","message":{"content":[...]}}
//
// — so it is a distinct parser feeding the same transcript.Event. Lines that
// are not parseable conversation events (system/result, ANSI noise, non-JSON)
// yield nil, because the durable line tap delivers RAW PTY bytes.
package claude

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// streamParser implements harness.StreamParser for Claude's stream-json stdout.
type streamParser struct{}

// streamLine is the minimal shape of one stream-json record. session_id is
// top-level in the live schema (unlike the on-disk file, which keys the session
// by filename); message carries the content blocks.
type streamLine struct {
	Type      string         `json:"type"`
	SessionID string         `json:"session_id"`
	Message   *streamMessage `json:"message"`
}

type streamMessage struct {
	ID      string        `json:"id"` // API message id (msg_...) — disambiguates text blocks
	Role    string        `json:"role"`
	Content []streamBlock `json:"content"`
}

// streamBlock is a content block. Unlike the on-disk ContentBlock, the live
// tool_use block carries its id inline, so no second pass is needed.
type streamBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`          // tool_use id
	Name      string          `json:"name"`        // tool_use name
	Input     json.RawMessage `json:"input"`       // tool_use input
	ToolUseID string          `json:"tool_use_id"` // tool_result → references the tool_use
	Content   json.RawMessage `json:"content"`     // tool_result payload
}

// ParseStreamLine implements harness.StreamParser. It returns the conversation
// events carried by a single stream-json line (assistant text/tool_use, user
// tool_result/text). system/init and result lines carry no conversation event
// (session id is captured separately via ExtractSessionID), so they yield nil,
// as do non-JSON / ANSI-polluted lines.
func (streamParser) ParseStreamLine(line string) []transcript.ParsedEvent {
	var sl streamLine
	if err := json.Unmarshal([]byte(line), &sl); err != nil || sl.Message == nil {
		return nil
	}
	switch sl.Type {
	case "assistant":
		return blockEvents(sl, transcript.RoleAssistant)
	case "user":
		return blockEvents(sl, transcript.RoleUser)
	default:
		return nil
	}
}

// blockEvents projects a stream message's content blocks into ParsedEvents.
// defaultRole is the message role; tool_result blocks are always role=tool. The
// per-line index disambiguates text NativeIDs (the live stream has no shared
// native id for text vs. the on-disk file, so text ids are source-prefixed; the
// authority filter means only one source feeds the parent per mode, so cross-
// source text equality is not required — reviews #1/#6).
func blockEvents(sl streamLine, defaultRole string) []transcript.ParsedEvent {
	var out []transcript.ParsedEvent
	for i, b := range sl.Message.Content {
		switch b.Type {
		case "text":
			text := b.Text
			if defaultRole == transcript.RoleUser {
				text = transcript.StripIDEContextTags(text)
			}
			if text == "" {
				continue
			}
			out = append(out, parsed(sl.SessionID, transcript.Event{
				Seq: i, Role: defaultRole, Type: transcript.EventText, Text: text,
				Source: transcript.SourceLive, NativeID: liveTextNativeID(sl.Message.ID, i),
			}))
		case "tool_use":
			out = append(out, parsed(sl.SessionID, transcript.Event{
				Seq: i, Role: transcript.RoleAssistant, Type: transcript.EventToolUse,
				ToolName: b.Name, ToolUseID: b.ID, ToolInput: b.Input,
				Source: transcript.SourceLive, NativeID: "tool-use:" + b.ID,
			}))
		case "tool_result":
			out = append(out, parsed(sl.SessionID, transcript.Event{
				Seq: i, Role: transcript.RoleTool, Type: transcript.EventToolResult,
				Output: streamToolResultText(b.Content), ToolUseID: b.ToolUseID,
				Source: transcript.SourceLive, NativeID: "tool-result:" + b.ToolUseID,
			}))
		}
	}
	return out
}

// parsed tags an Event with the live session id (top-level in stream-json). The
// parent session has no ParentSessionID; subagent nesting comes from the file
// path, not the live stream.
func parsed(sessionID string, e transcript.Event) transcript.ParsedEvent {
	return transcript.ParsedEvent{HarnessSessionID: sessionID, Event: e}
}

// liveTextNativeID builds the source-prefixed identity for a live text block.
// msgID disambiguates across messages; idx across blocks within a message.
func liveTextNativeID(msgID string, idx int) string {
	return transcript.SourceLive + ":text:" + msgID + ":" + strconv.Itoa(idx)
}

// streamToolResultText pulls the text out of a tool_result block's content,
// which is either an array of text blocks or a plain string (mirrors the
// on-disk parser's extractToolResultText for output parity).
func streamToolResultText(raw json.RawMessage) string {
	var textBlocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &textBlocks); err == nil {
		var sb strings.Builder
		for _, tb := range textBlocks {
			if tb.Type == "text" {
				sb.WriteString(tb.Text)
				sb.WriteByte('\n')
			}
		}
		return sb.String()
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return ""
}
