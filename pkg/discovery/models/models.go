// Package models is the curated static model registry for the supported
// harnesses — the offline half of meta-harness's src/discovery/models.ts.
//
// Neither `claude` nor `codex` exposes a machine-readable "list models" CLI
// flag; the only enumerator is the interactive `/model` slash command. This
// package deliberately avoids launching a CLI: it ships a curated, embedded
// list of known model ids/aliases per harness (models.json) so a chosen model
// can be validated offline. The live `/model`-picker parser and driver (the
// ModelInfo-producing half of models.ts) live in sibling subtasks and consume
// the Info contract defined here.
//
// This package is a sibling of pkg/discovery (which is scoped to PATH/version
// probing) and does not import it: the Info/DiscoverModels surface must not
// collide with discovery.Info/discovery.Discover.
//
// Schema of models.json:
//
//	{
//	  "claude-code": {"default": "opus",    "models": ["default", "opus", ...]},
//	  "codex":       {"default": "gpt-5.5", "models": ["gpt-5.5", "gpt-5.4", ...]}
//	}
package models

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed models.json
var embedded []byte

// Info is one model as listed by a harness's `/model` picker. It is the shared
// contract that both the picker parser and the live discovery driver produce;
// it is named Info (not ModelInfo) so the package qualifier reads naturally as
// models.Info and does not collide with discovery.Info. The json tags match the
// canonical serialization the corpus encodes.
type Info struct {
	// ID is the value to pass to the wrapper's model selection
	// (`--model` / `-c model=`).
	ID string `json:"ID"`
	// Label is the human-facing label shown in the picker
	// (e.g. "Opus", "gpt-5.4-mini").
	Label string `json:"Label"`
	// Description is the one-line description shown beside the label,
	// when present.
	Description string `json:"Description"`
	// Current is true for the harness's currently-active model.
	Current bool `json:"Current"`
	// IsDefault is true for the model the picker marks as the default /
	// recommended pick.
	IsDefault bool `json:"IsDefault"`
}

// registryEntry is one harness's curated static registry row.
type registryEntry struct {
	Default string   `json:"default"`
	Models  []string `json:"models"`
}

// registry is the parsed, embedded curated model registry, keyed by canonical
// registry key (see registryKey).
var registry = mustParse(embedded)

func mustParse(data []byte) map[string]registryEntry {
	out, err := parse(data)
	if err != nil {
		panic(err)
	}
	return out
}

func parse(data []byte) (map[string]registryEntry, error) {
	var out map[string]registryEntry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("models: parse: %w", err)
	}
	return out, nil
}

// norm lowercases and trims a harness name. It mirrors TS normHarness
// (strings.ToLower(strings.TrimSpace(...))) and deliberately does NOT map
// claude → claude-code — that alias lives only in registryKey.
func norm(harness string) string {
	return strings.ToLower(strings.TrimSpace(harness))
}

// registryKey is the canonical registry key for a harness: norm plus the
// "claude" → "claude-code" alias, so both resolve to the same entry. Mirrors TS
// registryKey.
func registryKey(harness string) string {
	h := norm(harness)
	if h == "claude" {
		return "claude-code"
	}
	return h
}

// KnownModels returns the curated list of known model ids/aliases for a
// harness, or nil if the harness is unknown. Mirrors TS knownModels.
func KnownModels(harness string) []string {
	e, ok := registry[registryKey(harness)]
	if !ok {
		return nil
	}
	out := make([]string, len(e.Models))
	copy(out, e.Models)
	return out
}

// DefaultModel returns the curated default model id for a harness, or "" if the
// harness is unknown. Mirrors TS defaultModel.
func DefaultModel(harness string) string {
	return registry[registryKey(harness)].Default
}

// IsKnownModel reports whether model is in the curated list for harness
// (case-insensitive). Like the TS isKnownModel, it is a validation helper, not a
// gate: the wrapper still forwards any free-form model string, so a brand-new
// model id absent from the list is not rejected — callers opt into this check
// when they want it.
func IsKnownModel(harness, model string) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return false
	}
	for _, m := range KnownModels(harness) {
		if strings.ToLower(m) == want {
			return true
		}
	}
	return false
}
