// Ported from github.com/entireio/cli cmd/entire/cli/transcript/parse.go
// (MIT, (c) 2026 Entire Inc.) via loomcli's internal/sessions/transcript.
// See ORIGIN.md.

package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// ParseFromBytes parses transcript content from a byte slice.
// Uses bufio.Reader to handle arbitrarily long lines. Malformed lines skipped.
func ParseFromBytes(content []byte) ([]Line, error) {
	return parseTranscriptLines(bufio.NewReader(bytes.NewReader(content)), 0)
}

// ParseFromFileAtLine reads and parses a transcript file starting from a
// specific line. startLine is 0-indexed. Malformed lines are skipped.
func ParseFromFileAtLine(path string, startLine int) ([]Line, error) {
	file, err := os.Open(path) //nolint:gosec // path is a controlled transcript file path
	if err != nil {
		return nil, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	return parseTranscriptLines(bufio.NewReader(file), startLine)
}

// parseTranscriptLines reads newline-delimited JSON from reader, skipping lines
// before startLine (0-indexed) and any malformed lines, returning the parsed
// Lines. It handles arbitrarily long lines and empty lines.
func parseTranscriptLines(reader *bufio.Reader, startLine int) ([]Line, error) {
	var lines []Line
	totalLines := 0
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read transcript: %w", err)
		}

		if len(lineBytes) > 0 {
			if totalLines >= startLine {
				lines = appendParsedLine(lines, lineBytes)
			}
			totalLines++
		}

		if err == io.EOF {
			break
		}
	}

	return lines, nil
}

// appendParsedLine unmarshals lineBytes into a Line and appends it to lines.
// Malformed lines are skipped, returning lines unchanged.
func appendParsedLine(lines []Line, lineBytes []byte) []Line {
	var line Line
	if err := json.Unmarshal(lineBytes, &line); err != nil {
		return lines
	}
	normalizeLineType(&line)
	return append(lines, line)
}

// normalizeLineType ensures line.Type is populated for all transcript formats.
// Claude Code uses "type" while Cursor uses "role" for the same purpose.
func normalizeLineType(line *Line) {
	if line.Type == "" && line.Role != "" {
		line.Type = line.Role
	}
}

// SliceFromLine returns content starting at line number startLine (0-indexed).
// Returns empty slice if startLine exceeds the number of lines.
func SliceFromLine(content []byte, startLine int) []byte {
	if len(content) == 0 || startLine <= 0 {
		return content
	}

	lineCount := 0
	offset := 0
	for i, b := range content {
		if b == '\n' {
			lineCount++
			if lineCount == startLine {
				offset = i + 1
				break
			}
		}
	}

	if lineCount < startLine {
		return nil
	}
	if offset >= len(content) {
		return nil
	}

	return content[offset:]
}

// ExtractUserContent extracts user prompt text from a raw message. Handles both
// string and array content formats. Strips IDE/system context tags from the
// result. Returns empty string when the message cannot be parsed or has no text.
func ExtractUserContent(message json.RawMessage) string {
	var msg UserMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return ""
	}

	if str, ok := msg.Content.(string); ok {
		return StripIDEContextTags(str)
	}

	if arr, ok := msg.Content.([]interface{}); ok {
		if texts := collectContentTexts(arr); len(texts) > 0 {
			return StripIDEContextTags(strings.Join(texts, "\n\n"))
		}
	}

	return ""
}

// collectContentTexts gathers the "text" fields of text-typed content blocks
// from an array-format message content value.
func collectContentTexts(arr []interface{}) []string {
	var texts []string
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok || m["type"] != ContentTypeText {
			continue
		}
		if text, ok := m["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return texts
}
