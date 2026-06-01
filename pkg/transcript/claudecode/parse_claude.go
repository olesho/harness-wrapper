// Claude Code JSONL → canonical transcript.Event parser.
//
// Ported from github.com/entireio/cli (MIT, (c) 2026 Entire Inc.) via loomcli's
// internal/sessions/transcript/claude. See ../ORIGIN.md. Local adaptations:
// each Event is tagged Source=file, and a dedup-stable NativeID is set (the
// wrapper dedups events by NativeID; loom's Event had neither field).
package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Events parses a Claude Code JSONL transcript and returns the canonical
// event stream (one event per content block, tool-aware). Malformed lines are
// skipped. This is the file-source counterpart equivalent to loomcli's
// claude.Events, so the wrapper can replace loom's per-harness parser.
func Events(data []byte) ([]transcript.Event, error) {
	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("claudecode transcript: %w", err)
	}

	events := make([]transcript.Event, 0, len(lines))
	seq := 0

	for _, line := range lines {
		ts := parseLineTimestamp(line.Timestamp)
		switch line.Type {
		case transcript.TypeUser:
			events = append(events, userLineEvents(line, ts, &seq)...)
		case transcript.TypeAssistant:
			events = append(events, assistantLineEvents(line, ts, &seq)...)
		}
	}
	return events, nil
}

// parseLineTimestamp parses the optional top-level timestamp string Claude
// writes on every JSONL line. Returns the zero time if empty or malformed.
func parseLineTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

// textNativeID gives a per-source-stable id for a text event, which has no
// shared native id across live/file (review: source-prefixed; the authority
// filter ensures only one source feeds the parent, so this need not be
// cross-source-equal). line.UUID alone would collapse multiple blocks from one
// line, so the per-event seq disambiguates.
func textNativeID(lineUUID string, seq int) string {
	return fmt.Sprintf("%s:text:%s:%d", transcript.SourceFile, lineUUID, seq)
}

func userLineEvents(line transcript.Line, ts time.Time, seq *int) []transcript.Event {
	var msgEnvelope struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(line.Message, &msgEnvelope); err != nil {
		return nil
	}

	// Try string content (direct user prompt).
	var str string
	if err := json.Unmarshal(msgEnvelope.Content, &str); err == nil {
		text := transcript.StripIDEContextTags(str)
		if text == "" {
			return nil
		}
		e := transcript.Event{
			Seq: *seq, Timestamp: ts, Role: transcript.RoleUser, Type: transcript.EventText,
			Text: text, UUID: line.UUID, Source: transcript.SourceFile,
			NativeID: textNativeID(line.UUID, *seq),
		}
		*seq++
		return []transcript.Event{e}
	}

	// Array content — text blocks and tool_result blocks.
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgEnvelope.Content, &blocks); err != nil {
		return nil
	}

	var out []transcript.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			txt := transcript.StripIDEContextTags(b.Text)
			if txt == "" {
				continue
			}
			out = append(out, transcript.Event{
				Seq: *seq, Timestamp: ts, Role: transcript.RoleUser, Type: transcript.EventText,
				Text: txt, UUID: line.UUID, Source: transcript.SourceFile,
				NativeID: textNativeID(line.UUID, *seq),
			})
			*seq++
		case "tool_result":
			out = append(out, transcript.Event{
				Seq: *seq, Timestamp: ts, Role: transcript.RoleTool, Type: transcript.EventToolResult,
				Output: extractToolResultText(b.Content), ToolUseID: b.ToolUseID,
				UUID: line.UUID, Source: transcript.SourceFile,
				NativeID: "tool-result:" + b.ToolUseID,
			})
			*seq++
		}
	}
	return out
}

func assistantLineEvents(line transcript.Line, ts time.Time, seq *int) []transcript.Event {
	var msg transcript.AssistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		return nil
	}

	// Find the block-level tool_use id (Claude emits it on the block, mirroring
	// the user's later tool_result.tool_use_id).
	var toolUseIDs []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(line.Message, &struct {
		Content *[]struct {
			ID string `json:"id"`
		} `json:"content"`
	}{Content: &toolUseIDs})

	var out []transcript.Event
	for i, block := range msg.Content {
		switch block.Type {
		case transcript.ContentTypeText:
			if block.Text == "" {
				continue
			}
			out = append(out, transcript.Event{
				Seq: *seq, Timestamp: ts, Role: transcript.RoleAssistant, Type: transcript.EventText,
				Text: block.Text, UUID: line.UUID, Source: transcript.SourceFile,
				NativeID: textNativeID(line.UUID, *seq),
			})
			*seq++
		case transcript.ContentTypeToolUse:
			var id string
			if i < len(toolUseIDs) {
				id = toolUseIDs[i].ID
			}
			out = append(out, transcript.Event{
				Seq: *seq, Timestamp: ts, Role: transcript.RoleAssistant, Type: transcript.EventToolUse,
				ToolName: block.Name, ToolUseID: id, ToolInput: block.Input,
				UUID: line.UUID, Source: transcript.SourceFile,
				NativeID: "tool-use:" + id,
			})
			*seq++
		}
	}
	return out
}

// extractToolResultText pulls the text out of a tool_result block's content,
// which is either an array of text blocks or a plain string.
func extractToolResultText(raw json.RawMessage) string {
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
