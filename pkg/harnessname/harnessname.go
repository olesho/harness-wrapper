// Package harnessname is the single home of the built-in harness identifiers
// and their alias table. It imports only the standard library and names no
// harness's code, so any layer — the cores, the per-harness packages, the CLIs
// and outside consumers — may depend on it without linking a harness.
//
// Two spellings are accepted for Claude Code: the canonical id "claude-code"
// (the chat adapter, discovery and versions key) and the short alias "claude"
// (its binary name, and the key of the headless pkg/harness profile registry
// and the CLI). Canonical folds the alias onto the id; Short goes the other way
// for a layer keyed by short names.
//
// The helpers come in two strengths, because existing call sites differ and
// must keep doing so:
//
//   - Canonical, Short and IsClaude trim and lower-case first, for call sites
//     that match names case-insensitively.
//   - Unalias maps an exact alias spelling only, leaving every other string
//     byte-for-byte unchanged, for call sites that match names exactly.
package harnessname

import (
	"slices"
	"strings"
)

// Built-in harness identifiers (canonical ids).
const (
	// ClaudeCode is Claude Code's canonical id.
	ClaudeCode = "claude-code"
	// Codex is the OpenAI Codex CLI's id.
	Codex = "codex"
	// OpenCode is the OpenCode CLI's id.
	OpenCode = "opencode"
	// Pi is the Pi coding agent's id.
	Pi = "pi"
	// Cursor is the Cursor agent CLI's id (classifier patterns only).
	Cursor = "cursor"
)

// Claude is the short alias of ClaudeCode: the name of its binary, and the key
// the headless profile registry (pkg/harness) and the harness-wrapper CLI use.
const Claude = "claude"

// aliases is the single alias table: alias spelling → canonical id. Every key
// and value is already normalized (trimmed, lower-case).
var aliases = map[string]string{
	Claude: ClaudeCode,
}

// shorts maps a canonical id to its short key, for a layer keyed by short
// names, where the two differ.
var shorts = map[string]string{
	ClaudeCode: Claude,
}

// Normalize trims surrounding space and lower-cases name. It maps no alias.
func Normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Canonical returns the canonical id for name: Normalize, then the alias table
// ("claude" → "claude-code"). An unknown name is returned normalized and
// otherwise unchanged.
func Canonical(name string) string {
	n := Normalize(name)
	if id, ok := aliases[n]; ok {
		return id
	}
	return n
}

// Unalias maps an exact alias spelling to its canonical id and returns every
// other string unchanged — no trimming, no case folding. It is for call sites
// that match harness names exactly.
func Unalias(name string) string {
	if id, ok := aliases[name]; ok {
		return id
	}
	return name
}

// Short returns the short key for name: Canonical, then the canonical id's
// short form where it has one ("claude-code" and "claude" → "claude"). Other
// names are returned as Canonical returns them.
func Short(name string) string {
	id := Canonical(name)
	if s, ok := shorts[id]; ok {
		return s
	}
	return id
}

// IsClaude reports whether name names Claude Code, in either spelling,
// case-insensitively and ignoring surrounding space.
func IsClaude(name string) bool { return Canonical(name) == ClaudeCode }

// Spellings returns id followed by every alias that resolves to it, the
// aliases sorted — the names a name-keyed registry that does not fold aliases
// itself must register under. The slice is freshly allocated.
func Spellings(id string) []string {
	var extra []string
	for alias, target := range aliases {
		if target == id {
			extra = append(extra, alias)
		}
	}
	slices.Sort(extra)
	return append([]string{id}, extra...)
}
