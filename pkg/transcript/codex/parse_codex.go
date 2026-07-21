// Codex rollout JSONL → canonical, TOOL-AWARE transcript.Event parser.
//
// Ported from loomcli's tool-aware codex parser so Codex parsing lives in one
// place (the wrapper). Local adaptations vs. loom: each Event is tagged
// Source=file with a dedup-stable, kind-qualified NativeID (the wrapper dedups
// by NativeID; loom's Event had neither field). The PUBLIC fields
// (Seq/Timestamp/Role/Type/Text/Tool*/Output) match loom's output exactly, so
// loom can delegate to this without a serving regression.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Envelope is Codex's top-level rollout line: {timestamp, type, payload}.
type Envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"` // session_meta, event_msg, response_item, turn_context
	Payload   json.RawMessage `json:"payload"`
}

// responseItem is the payload when Envelope.Type == "response_item".
type responseItem struct {
	Type      string          `json:"type"` // message, function_call, function_call_output, custom_tool_call, custom_tool_call_output, reasoning
	Role      string          `json:"role,omitempty"`
	Content   []contentBlock  `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`      // tool name (function_call / custom_tool_call)
	Arguments string          `json:"arguments,omitempty"` // function args, a JSON string (function_call)
	Input     json.RawMessage `json:"input,omitempty"`     // freeform tool input, a JSON value (custom_tool_call)
	CallID    string          `json:"call_id,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // function_call_output / custom_tool_call_output payload
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseRollout reads Codex rollout JSONL bytes into envelope lines. Malformed
// lines are skipped. Uses bufio.ReadBytes (not Scanner) so there is no
// line-length cap — Codex tool I/O lines can be large.
func ParseRollout(data []byte) ([]Envelope, error) {
	var out []Envelope
	reader := bufio.NewReader(bytes.NewReader(data))
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("codex transcript: read rollout: %w", err)
		}
		if len(line) > 0 {
			var env Envelope
			if json.Unmarshal(line, &env) == nil {
				out = append(out, env)
			}
		}
		if err == io.EOF {
			break
		}
	}
	return out, nil
}

// Events parses Codex rollout JSONL bytes into the canonical, tool-aware event
// stream. Only response_item entries are surfaced (message / function_call /
// function_call_output / custom_tool_call / custom_tool_call_output); the rest
// are operational noise. custom_tool_call(_output) is the schema newer codex-cli
// (>= 0.144) records for its freeform `exec` tool, alongside the legacy
// function_call schema.
func Events(data []byte) ([]transcript.Event, error) {
	envelopes, err := ParseRollout(data)
	if err != nil {
		return nil, err
	}
	out := make([]transcript.Event, 0, len(envelopes))
	seq := 0
	for _, env := range envelopes {
		if env.Type != "response_item" {
			continue
		}
		ts := parseCodexTime(env.Timestamp)
		var item responseItem
		if err := json.Unmarshal(env.Payload, &item); err != nil {
			continue
		}
		switch item.Type {
		case "message":
			seq = appendMessageEvents(&out, item, ts, seq)
		case "function_call":
			out = append(out, transcript.Event{
				Seq: seq, Timestamp: ts, Role: transcript.RoleAssistant, Type: transcript.EventToolUse,
				ToolName: item.Name, ToolUseID: item.CallID, ToolInput: json.RawMessage(item.Arguments),
				Source: transcript.SourceFile, NativeID: "tool-use:" + item.CallID,
			})
			seq++
		case "function_call_output":
			out = append(out, transcript.Event{
				Seq: seq, Timestamp: ts, Role: transcript.RoleTool, Type: transcript.EventToolResult,
				ToolUseID: item.CallID, Output: decodeFunctionOutput(item.Output),
				Source: transcript.SourceFile, NativeID: "tool-result:" + item.CallID,
			})
			seq++
		case "custom_tool_call":
			// Freeform tool call (e.g. codex-cli's `exec`). Input is a JSON value
			// (a JSON string wrapping a script for exec), already valid JSON, so
			// it becomes ToolInput directly — no re-encoding needed.
			out = append(out, transcript.Event{
				Seq: seq, Timestamp: ts, Role: transcript.RoleAssistant, Type: transcript.EventToolUse,
				ToolName: item.Name, ToolUseID: item.CallID, ToolInput: customToolInput(item.Input),
				Source: transcript.SourceFile, NativeID: "tool-use:" + item.CallID,
			})
			seq++
		case "custom_tool_call_output":
			out = append(out, transcript.Event{
				Seq: seq, Timestamp: ts, Role: transcript.RoleTool, Type: transcript.EventToolResult,
				ToolUseID: item.CallID, Output: decodeCustomToolOutput(item.Output),
				Source: transcript.SourceFile, NativeID: "tool-result:" + item.CallID,
			})
			seq++
		}
	}
	return out, nil
}

// codexTokenCount is the payload of an event_msg envelope whose type is
// "token_count". info can legitimately be null on early events; only events
// carrying a non-null total_token_usage object contribute usage.
type codexTokenCount struct {
	Type string `json:"type"`
	Info *struct {
		TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
	} `json:"info"`
}

// codexTokenUsage is Codex's cumulative token accounting. Numeric fields are
// json.Number so transcript.ToCount can clamp them. Note input_tokens ALREADY
// includes cached_input_tokens (a subset, not an addition), and Codex has no
// cache-creation field.
type codexTokenUsage struct {
	InputTokens         json.Number `json:"input_tokens"`
	CachedInputTokens   json.Number `json:"cached_input_tokens"`
	OutputTokens        json.Number `json:"output_tokens"`
	ReasoningOutputToks json.Number `json:"reasoning_output_tokens"`
}

// UsageFromJSONL returns the last token_count event's cumulative token usage
// from a Codex rollout's JSONL bytes, or (nil, nil) when no token_count event
// carried usage. Ports meta-harness usageFromCodexJSONL.
//
// This is a SEPARATE pass from Events: Codex usage lives in event_msg →
// payload.type == "token_count" envelopes, which Events deliberately skips.
// The aggregation contract is to keep the LAST token_count event whose
// info.total_token_usage is a non-null object (info is null on early events),
// using total_token_usage (the cumulative session total), not last_token_usage.
func UsageFromJSONL(data []byte) (*transcript.Usage, error) {
	envelopes, err := ParseRollout(data)
	if err != nil {
		return nil, err
	}
	var last *codexTokenUsage
	for _, env := range envelopes {
		if env.Type != "event_msg" {
			continue
		}
		var tc codexTokenCount
		if err := json.Unmarshal(env.Payload, &tc); err != nil {
			continue
		}
		if tc.Type != "token_count" || tc.Info == nil || tc.Info.TotalTokenUsage == nil {
			continue
		}
		last = tc.Info.TotalTokenUsage
	}
	if last == nil {
		return nil, nil
	}
	// Codex spells the cached figure "cached_input_tokens" and has no
	// cache-creation field, so CacheCreationInputTokens stays 0. input_tokens is
	// mapped straight across (it already includes the cached subset).
	return &transcript.Usage{
		InputTokens:           transcript.ToCount(last.InputTokens),
		OutputTokens:          transcript.ToCount(last.OutputTokens),
		CacheReadInputTokens:  transcript.ToCount(last.CachedInputTokens),
		ReasoningOutputTokens: transcript.ToCount(last.ReasoningOutputToks),
	}, nil
}

// appendMessageEvents emits one text event per non-empty content block (user
// text has IDE/system context tags stripped, matching loom + claudecode).
func appendMessageEvents(out *[]transcript.Event, item responseItem, ts time.Time, seq int) int {
	role := canonicalRole(item.Role)
	for _, block := range item.Content {
		text := block.Text
		if role == transcript.RoleUser {
			text = transcript.StripIDEContextTags(text)
		}
		if text == "" {
			continue
		}
		*out = append(*out, transcript.Event{
			Seq: seq, Timestamp: ts, Role: role, Type: transcript.EventText, Text: text,
			Source: transcript.SourceFile, NativeID: transcript.SourceFile + ":text:" + strconv.Itoa(seq),
		})
		seq++
	}
	return seq
}

// decodeFunctionOutput pulls the text out of a function_call_output payload,
// which is usually a JSON string and occasionally a structured object.
func decodeFunctionOutput(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	return string(raw)
}

// customToolInput returns the ToolInput for a custom_tool_call. The input is
// already a JSON value (typically a JSON string wrapping the freeform script),
// so it is used verbatim; nil/empty input yields nil so ToolInput is omitted.
func customToolInput(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return raw
}

// decodeCustomToolOutput flattens a custom_tool_call_output "output" value into
// readable text. codex records it as an array of {type,text} content blocks; it
// falls back to a bare JSON string, then to the raw JSON.
func decodeCustomToolOutput(raw json.RawMessage) string {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
		var b bytes.Buffer
		for _, blk := range blocks {
			b.WriteString(blk.Text)
		}
		return b.String()
	}
	return decodeFunctionOutput(raw)
}

func canonicalRole(r string) string {
	switch r {
	case "user":
		return transcript.RoleUser
	case "assistant":
		return transcript.RoleAssistant
	default: // developer, system, tool, unknown → system
		return transcript.RoleSystem
	}
}

func parseCodexTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}
