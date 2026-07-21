package transcript

import "encoding/json"

// Usage is the normalized token accounting for a session — the union of both
// harnesses' token fields, keyed by the five acceptance wire keys.
//
// All five keys ALWAYS serialize: none of the inner fields carry omitempty, so
// numeric zeros stay on the wire (a Codex usage always emits
// "cache_creation_input_tokens":0; a Claude usage always emits
// "reasoning_output_tokens":0). This keeps the Go object byte-identical with
// meta-harness's usageToPublicJSON, which emits all five keys unconditionally.
// omitempty belongs ONLY on the top-level *Usage field of a containing struct
// (e.g. StructuredTurnResult), never on these inner fields.
//
// input_tokens semantics legitimately differ per harness and must NOT be
// "fixed": Claude's input_tokens EXCLUDES cache reads/creates (they live in the
// separate cache_* fields, matching Anthropic API semantics), while Codex's
// input_tokens INCLUDES cached_input_tokens (the cached count is a subset, not
// an addition — nobody should re-add it).
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens"`
}

// ToCount clamps a raw token count to a non-negative integer: a non-finite,
// non-numeric, or non-positive value becomes 0. Mirrors meta-harness toCount.
//
// The value is parsed as an integer (via json.Number) rather than a bare
// float64, so a stray fractional or garbage field truncates toward its integer
// part predictably instead of silently diverging on the wire.
func ToCount(v json.Number) int {
	// Prefer an exact integer parse; a fractional or garbage field falls back
	// to a float parse so it truncates predictably before the clamp.
	if n, err := v.Int64(); err == nil {
		if n > 0 {
			return int(n)
		}
		return 0
	}
	f, err := v.Float64()
	if err != nil || f <= 0 {
		return 0
	}
	return int(f)
}
