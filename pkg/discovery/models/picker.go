package models

import (
	"regexp"
	"strings"
)

// This file is the SCREEN-PARSER half of model discovery: a faithful port of
// parseModelPicker from meta-harness's src/discovery/models.ts. It is a PURE
// function — rendered screen text in, models out — and shares the Info contract
// defined in models.go. The live driver (discoverModels) that feeds it a real
// picker screen lives in a sibling subtask.
//
// REGEX-ENGINE PARITY. The TS row regex uses JS `\s`/`\S`, which match Unicode
// whitespace (NBSP U+00A0, the figure/thin/hair/en/em/ideographic spaces, the
// line/paragraph separators, BOM). Go's regexp is RE2, whose `\s`/`\S` are
// ASCII-only ([\t\n\f\r ]). A picker that renders a non-ASCII space in the
// label→description gap would therefore parse under JS but not under a naive Go
// `\s{2,}`. To keep the two engines byte-for-byte in agreement we spell the
// whitespace classes out explicitly to mirror JS `\s` exactly, rather than rely
// on `\s`. jsWS/jsNonWS are the JS `\s`/`\S` sets. See parity_test.go, which
// pins this equivalence, and the corpus's non-ascii-gap case.
const (
	jsWS    = `[\t\n\x0b\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	jsNonWS = `[^\t\n\x0b\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
)

// rowRe is the Go equivalent of the TS
//
//	rowRe = /^\s*[❯›*]?\s*\d+\.\s+(.+?)\s{2,}(\S.*?)\s*$/u   (models.ts:42)
//
// with `\s`/`\S` replaced by the explicit JS-whitespace classes above so RE2 and
// V8 accept the same lines. A picker row is: optional cursor marker (❯ / › / *),
// an index, then a two-column "label  description" split on a run of 2+ spaces.
// A row without a 2+-space column (a description-less row) does not match and is
// silently dropped — inherited TS behavior, pinned by a corpus case.
var rowRe = regexp.MustCompile(`^` + jsWS + `*[❯›*]?` + jsWS + `*\d+\.` + jsWS + `+(.+?)` + jsWS + `{2,}(` + jsNonWS + `.*?)` + jsWS + `*$`)

// Per-harness marker regexes, ported verbatim from models.ts. Case-insensitive
// where the TS uses the /i flag.
var (
	codexCurrentRe = regexp.MustCompile(`(?i)\(current\)`)
	codexDefaultRe = regexp.MustCompile(`(?i)\(default\)`)
	codexParensRe  = regexp.MustCompile(`\([^)]*\)`)
	claudeDefault  = regexp.MustCompile(`(?i)^Default\b|\(recommended\)`)
)

// pickerHeaderRe returns the header text that must be present for a screen to be
// treated as a `/model` picker, or nil for an unsupported harness. Mirrors TS
// pickerHeader: ONLY claude/claude-code (/Select model/i) and codex
// (/Select Model and Effort/i) are recognized; every other harness (pi,
// opencode, …) returns nil, so parseModelPicker yields [].
func pickerHeaderRe(harness string) *regexp.Regexp {
	switch norm(harness) {
	case "claude", "claude-code":
		return regexp.MustCompile(`(?i)Select model`)
	case "codex":
		return regexp.MustCompile(`(?i)Select Model and Effort`)
	default:
		return nil
	}
}

// ParseModelPicker extracts the model list from a rendered `/model` picker
// screen. text is a screen.Snapshot().Text (one line per row). It returns nil
// for an unsupported harness or a screen that is not a model picker (so a stray
// numbered list elsewhere on screen never yields false positives).
//
// It is a faithful port of the TS parseModelPicker: same header gate, same row
// shape, same per-harness extraction (claude — `✔` marks current,
// `Default`/`(recommended)` marks default, id = lowercased first token; codex —
// `(current)`/`(default)` markers, id = first token with parens stripped).
func ParseModelPicker(text, harness string) []Info {
	header := pickerHeaderRe(harness)
	if header == nil || !header.MatchString(text) {
		return nil
	}
	kind := norm(harness)
	var out []Info
	for _, line := range strings.Split(text, "\n") {
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rawLabel := strings.TrimSpace(m[1])
		description := strings.TrimSpace(m[2])
		if kind == "codex" {
			current := codexCurrentRe.MatchString(rawLabel)
			isDefault := codexDefaultRe.MatchString(rawLabel)
			// "gpt-5.4-mini (current)" → id/label "gpt-5.4-mini".
			id := firstToken(strings.TrimSpace(codexParensRe.ReplaceAllString(rawLabel, "")))
			if id == "" {
				continue
			}
			out = append(out, Info{ID: id, Label: id, Description: description, Current: current, IsDefault: isDefault})
		} else {
			// claude-code: "Opus ✔" (active) / "Default (recommended)" (default).
			current := strings.Contains(rawLabel, "✔")
			cleaned := strings.TrimSpace(strings.ReplaceAll(rawLabel, "✔", ""))
			isDefault := claudeDefault.MatchString(cleaned)
			// The picker's short name is the `--model` alias, case-insensitively.
			id := strings.ToLower(firstToken(cleaned))
			if id == "" {
				continue
			}
			out = append(out, Info{ID: id, Label: cleaned, Description: description, Current: current, IsDefault: isDefault})
		}
	}
	return out
}

// firstToken returns the first whitespace-delimited token of s, or "" if s is
// blank. It mirrors the TS `s.split(/\s+/)[0] ?? ""` on an already-trimmed
// string.
func firstToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
