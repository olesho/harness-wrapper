// Claude Code JSONL → canonical transcript.Event parser.
//
// Ported from github.com/entireio/cli (MIT, (c) 2026 Entire Inc.) via loomcli's
// internal/sessions/transcript/claude. See ../ORIGIN.md. Local adaptations:
// each Event is tagged Source=file, and a dedup-stable NativeID is set (the
// wrapper dedups events by NativeID; loom's Event had neither field). The
// per-line parse also reports the content block each event came from, and why
// a line it could not read was unreadable, for the Follower.
package claudecode

import (
	"cmp"
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
	for _, line := range lines {
		// An unreadable line yields no events, as it always has: Read skips
		// what it cannot parse. The Follower reports it instead.
		blocks, _ := lineEvents(line)
		for _, b := range blocks {
			e := b.Event
			e.Seq = len(events)
			e.NativeID = legacyNativeID(e)
			events = append(events, e)
		}
	}
	return events, nil
}

// UsageFromJSONL sums per-API-call token usage across a Claude Code session's
// JSONL bytes, deduped by message id. Returns (nil, nil) when no line carried
// usage. Ports meta-harness usageFromClaudeJSONL.
//
// Claude writes one JSONL line per content block, and multiple lines from ONE
// API call REPEAT the same message.usage. So we dedup by API call: the key is
// message.id when non-empty, else "line:"+Line.UUID; only the first line for a
// distinct key contributes its usage. A naive per-line sum would over-count.
func UsageFromJSONL(data []byte) (*transcript.Usage, error) {
	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("claudecode transcript: %w", err)
	}

	var (
		total transcript.Usage
		seen  = map[string]bool{}
		any   bool
	)
	for _, line := range lines {
		if line.Type != transcript.TypeAssistant {
			continue
		}
		var env struct {
			ID    string `json:"id"`
			Usage *struct {
				InputTokens              json.Number `json:"input_tokens"`
				OutputTokens             json.Number `json:"output_tokens"`
				CacheReadInputTokens     json.Number `json:"cache_read_input_tokens"`
				CacheCreationInputTokens json.Number `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line.Message, &env); err != nil {
			continue
		}
		if env.Usage == nil {
			continue
		}

		key := env.ID
		if key == "" {
			key = "line:" + line.UUID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		any = true

		total.InputTokens += transcript.ToCount(env.Usage.InputTokens)
		total.OutputTokens += transcript.ToCount(env.Usage.OutputTokens)
		total.CacheReadInputTokens += transcript.ToCount(env.Usage.CacheReadInputTokens)
		total.CacheCreationInputTokens += transcript.ToCount(env.Usage.CacheCreationInputTokens)
	}

	if !any {
		return nil, nil
	}
	return &total, nil
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

// legacyNativeID is the NativeID Events gives e, which Read's callers dedup
// on: kind-qualified tool ids, and for text the line UUID plus the event's Seq.
func legacyNativeID(e transcript.Event) string {
	switch e.Type {
	case transcript.EventToolUse:
		return "tool-use:" + e.ToolUseID
	case transcript.EventToolResult:
		return "tool-result:" + e.ToolUseID
	default:
		return textNativeID(e.UUID, e.Seq)
	}
}

// textNativeID gives a per-source-stable id for a text event, which has no
// shared native id across live/file (review: source-prefixed; the authority
// filter ensures only one source feeds the parent, so this need not be
// cross-source-equal). line.UUID alone would collapse multiple blocks from one
// line, so the per-event seq disambiguates.
func textNativeID(lineUUID string, seq int) string {
	return fmt.Sprintf("%s:text:%s:%d", transcript.SourceFile, lineUUID, seq)
}

// decodeRecord is the Follower's decoder: one JSONL entry to its events, each
// with the index of its content block — the events Events gives for the line.
// An entry that is not JSON, or a user or assistant entry that cannot be read,
// is an error: the follower reports it rather than skipping it.
//
// Only user and assistant entries hold events, so only they must fit Line.
// Other entries are skipped however they are shaped: a system api_error entry
// carries an object in "error", which Line reads as a string, so it fails to
// parse as a Line at all — Read drops it for that, and it held no events.
func decodeRecord(record []byte) ([]transcript.BlockEvent, error) {
	line, err := transcript.ParseLine(record)
	if err == nil {
		return lineEvents(line)
	}
	var kind struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(record, &kind) == nil {
		switch cmp.Or(kind.Type, kind.Role) {
		case transcript.TypeUser, transcript.TypeAssistant:
		default:
			return nil, nil
		}
	}
	return nil, err
}

// lineEvents returns one line's events, without Seq or NativeID: those are
// the caller's, since Read and the Follower number and identify events
// differently. Lines of other types (permission-mode, system, attachment, …)
// carry no conversation and yield none.
func lineEvents(line transcript.Line) ([]transcript.BlockEvent, error) {
	ts := parseLineTimestamp(line.Timestamp)
	switch line.Type {
	case transcript.TypeUser:
		return userLineEvents(line, ts)
	case transcript.TypeAssistant:
		return assistantLineEvents(line, ts)
	}
	return nil, nil
}

func userLineEvents(line transcript.Line, ts time.Time) ([]transcript.BlockEvent, error) {
	var msgEnvelope struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(line.Message, &msgEnvelope); err != nil {
		return nil, fmt.Errorf("user entry: unreadable message: %w", err)
	}

	// Try string content (direct user prompt).
	var str string
	if err := json.Unmarshal(msgEnvelope.Content, &str); err == nil {
		text := transcript.StripIDEContextTags(str)
		if text == "" {
			return nil, nil
		}
		return []transcript.BlockEvent{{Event: transcript.Event{
			Timestamp: ts, Role: transcript.RoleUser, Type: transcript.EventText,
			Text: text, UUID: line.UUID, Source: transcript.SourceFile,
		}}}, nil
	}

	// Array content — text blocks and tool_result blocks.
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgEnvelope.Content, &blocks); err != nil {
		return nil, fmt.Errorf("user entry: content is neither text nor a block array: %w", err)
	}

	var out []transcript.BlockEvent
	for i, b := range blocks {
		switch b.Type {
		case "text":
			txt := transcript.StripIDEContextTags(b.Text)
			if txt == "" {
				continue
			}
			out = append(out, transcript.BlockEvent{Block: i, Event: transcript.Event{
				Timestamp: ts, Role: transcript.RoleUser, Type: transcript.EventText,
				Text: txt, UUID: line.UUID, Source: transcript.SourceFile,
			}})
		case "tool_result":
			out = append(out, transcript.BlockEvent{Block: i, Event: transcript.Event{
				Timestamp: ts, Role: transcript.RoleTool, Type: transcript.EventToolResult,
				Output: extractToolResultText(b.Content), ToolUseID: b.ToolUseID,
				UUID: line.UUID, Source: transcript.SourceFile,
			}})
		}
	}
	return out, nil
}

func assistantLineEvents(line transcript.Line, ts time.Time) ([]transcript.BlockEvent, error) {
	var msg transcript.AssistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		return nil, fmt.Errorf("assistant entry: unreadable message: %w", err)
	}

	// A synthetic API-error line is shaped exactly like an assistant reply —
	// one text block, holding the rendered error — so the tag is the only thing
	// that tells them apart. Carry it onto every event this line produces
	// (in practice one); a consumer that skips tagged events then never reads a
	// failure as a reply. apiErrorTag is "" for the ordinary case, so the
	// serialized event stream for a clean transcript is unchanged.
	apiErrorTag := apiErrorTagOf(line)

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

	var out []transcript.BlockEvent
	for i, block := range msg.Content {
		switch block.Type {
		case transcript.ContentTypeText:
			if block.Text == "" {
				continue
			}
			out = append(out, transcript.BlockEvent{Block: i, Event: transcript.Event{
				Timestamp: ts, Role: transcript.RoleAssistant, Type: transcript.EventText,
				Text: block.Text, UUID: line.UUID, Source: transcript.SourceFile,
				APIError: apiErrorTag,
			}})
		case transcript.ContentTypeToolUse:
			var id string
			if i < len(toolUseIDs) {
				id = toolUseIDs[i].ID
			}
			out = append(out, transcript.BlockEvent{Block: i, Event: transcript.Event{
				Timestamp: ts, Role: transcript.RoleAssistant, Type: transcript.EventToolUse,
				ToolName: block.Name, ToolUseID: id, ToolInput: block.Input,
				UUID: line.UUID, Source: transcript.SourceFile,
				APIError: apiErrorTag,
			}})
		}
	}
	return out, nil
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

// apiErrorTagOf returns the line's machine-readable failure tag, and "" for
// every line that is not a synthetic API error.
//
// isApiErrorMessage is required, not merely preferred: `error` also appears on
// lines that are not API errors at all (hook results carry "warn" / "debug" /
// "policy_denied" there), and treating one of those as a failed turn would
// error a turn the harness completed. An API-error line with an empty tag
// yields "" and therefore no verdict, which is the conservative direction.
func apiErrorTagOf(line transcript.Line) string {
	if !line.IsAPIErrorMessage {
		return ""
	}
	return strings.TrimSpace(line.Error)
}
