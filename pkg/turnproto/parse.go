package turnproto

import (
	"bytes"
	"encoding/json"
)

// ParseLastJSONLine returns the LAST line of stdout that parses as a JSON object,
// or (nil, false) when no line does. It is the host-side inverse of the guest's
// structured-run emit(): the producer emits EXACTLY ONE JSON object line, but
// real stdout is noisy — harness banners or warnings can print BEFORE it, a stray
// non-JSON line can trail AFTER it, and a killed process can leave a TRUNCATED
// final line.
//
// So it scans lines from the LAST backward and returns the first that parses as a
// JSON OBJECT — tolerating leading noise, a non-JSON tail, a truncated tail, and
// (by taking the last object) multiple JSON lines. Bare JSON scalars/arrays are
// rejected — the protocol payload is always an object. Zero JSON objects ⇒
// (nil, false).
func ParseLastJSONLine(data []byte) (*StructuredTurnResult, bool) {
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		// Only a JSON object is the payload. Reject bare scalars and arrays
		// cheaply by the first non-space byte before attempting a full decode.
		if line[0] != '{' {
			continue
		}
		var out StructuredTurnResult
		if err := json.Unmarshal(line, &out); err != nil {
			// Non-JSON tail or a truncated final line — keep scanning backward.
			continue
		}
		return &out, true
	}
	return nil, false
}
