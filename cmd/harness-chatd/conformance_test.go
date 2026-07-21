package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/test/conformance/neutral"
)

// Cross-language conformance corpus — canonical side, gateway (chatd wire DTO)
// slice. See test/conformance/README.md for the convention.
//
// The chatd DTOs are unexported types in package main and wireTypes() lives in
// this package's test files, so no external package can reflect over them or
// round-trip fixtures through them — the gateway generator therefore lives HERE
// while the turnresult/cli generator lives in the external test/conformance
// package. Regenerate BOTH in order with: make regen-conformance
// (this alone leaves a stale MANIFEST.sha256 — the external package hashes the
// whole corpus). UPDATE_GOLDEN=1 emits; a plain run round-trips and compares.

// conformanceGatewayDir is the gateway slice of the shared corpus, relative to
// this package directory (cmd/harness-chatd).
const conformanceGatewayDir = "../../test/conformance/gateway"

// gatewayFixture is one language-neutral JSON example instance plus the DTO type
// it must round-trip through. Value carries a real struct value so emit mode
// serializes the canonical bytes and compare mode round-trips the golden back
// through the same type.
type gatewayFixture struct {
	name  string
	value any
}

// gatewayFixtures spans the edge cases the corpus pins: the always-present
// zero-StartedAt turnDTO ("0001-01-01T00:00:00Z" — a Go-ism a TS consumer would
// get wrong), the omitzero-absent completed_at, the input_request superset
// (header / multi_select / option description / option_ids via answerRequest),
// and the not_multi_select errorResponse standing in for HW-49's 400 behavior.
func gatewayFixtures() []gatewayFixture {
	started := mustTime("2026-07-21T10:00:00Z")
	completed := mustTime("2026-07-21T10:00:42Z")
	return []gatewayFixture{
		// Zero StartedAt (no omit option) serializes the Go zero time; CompletedAt
		// (omitzero) is ABSENT. This is the shape a TS consumer must tolerate.
		{"turnDTO.zero_started_at", turnDTO{
			ID: "turn-zero", SessionID: "sess-1", Role: "assistant", State: "running",
		}},
		// Both timestamps present — the populated completed turn.
		{"turnDTO.completed", turnDTO{
			ID: "turn-done", SessionID: "sess-1", Role: "assistant", State: "complete",
			Text: "the reply", StartedAt: started, CompletedAt: completed,
		}},
		// A blocked api-error turn pins http_code / retry_after presence.
		{"turnDTO.blocked_api", turnDTO{
			ID: "turn-blocked", SessionID: "sess-1", Role: "assistant", State: "blocked",
			Reason: "api_error", StartedAt: started, HTTPCode: 429, RetryAfter: "30s",
		}},
		{"inputRequestDTO.question", inputRequestDTO{
			ID: "req-1", Kind: "question", Prompt: "Pick one",
			Options: []inputOptionDTO{
				{ID: "1", Label: "Yes"},
				{ID: "2", Alias: "n", Label: "No"},
			},
		}},
		{"inputRequestDTO.question_review", inputRequestDTO{
			ID: "req-2", Kind: "question_review", Prompt: "Approve this change?",
			Header: "Review",
			Options: []inputOptionDTO{
				{ID: "1", Label: "Approve", Description: "Accept the pending diff"},
				{ID: "2", Label: "Reject", Description: "Discard the pending diff"},
			},
		}},
		{"inputRequestDTO.multi_select", inputRequestDTO{
			ID: "req-3", Kind: "question", Prompt: "Select all that apply",
			Header: "Features", MultiSelect: true,
			Options: []inputOptionDTO{
				{ID: "a", Label: "Alpha", Description: "the first"},
				{ID: "b", Label: "Beta"},
				{ID: "c", Label: "Gamma"},
			},
		}},
		// answerRequest carries option_ids (multi-select answer) vs option_id
		// (single-select). Both are pinned so a TS client emits the right one.
		{"answerRequest.multi_select", answerRequest{
			Token: "tok", RequestID: "req-3", OptionIDs: []string{"a", "c"},
		}},
		{"answerRequest.single_select", answerRequest{
			Token: "tok", RequestID: "req-1", OptionID: "1",
		}},
		// The not_multi_select 400 is HTTP behavior, not a DTO shape; it is
		// represented as the errorResponse chatd emits via writeError, plus a
		// README behavior row. The HTTP status itself is HW-49's handler test.
		{"errorResponse.not_multi_select", errorResponse{
			Error: "option_ids is only valid for multi_select prompts",
			Code:  "not_multi_select",
		}},
	}
}

// TestConformance_GatewayFields freezes the neutral field contract for every
// chatd wire DTO, reflected over the same wireTypes() list TestContract_WireTypes
// maintains (including omitzero-tag parsing). UPDATE_GOLDEN=1 regenerates.
func TestConformance_GatewayFields(t *testing.T) {
	got, err := neutral.Marshal(neutral.FieldsFor(wireTypes()...))
	if err != nil {
		t.Fatalf("marshal neutral fields: %v", err)
	}
	assertConformanceGolden(t, filepath.Join(conformanceGatewayDir, "fields.json"), got)
}

// TestConformance_GatewayFixtures emits each fixture's canonical JSON under
// UPDATE_GOLDEN=1, and otherwise round-trips every golden back through its DTO
// type asserting STRUCTURAL equality (never a byte compare — Go and TS emit keys
// in different orders; §2). Structural round-trip proves the type neither drops
// nor invents keys and preserves optional presence/absence.
func TestConformance_GatewayFixtures(t *testing.T) {
	for _, fx := range gatewayFixtures() {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			path := filepath.Join(conformanceGatewayDir, fx.name+".json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				data, err := json.MarshalIndent(fx.value, "", "  ")
				if err != nil {
					t.Fatalf("marshal fixture: %v", err)
				}
				writeConformanceFile(t, path, append(data, '\n'))
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (regenerate with make regen-conformance): %v", path, err)
			}
			assertRoundTripsStructurally(t, want, fx.value)
		})
	}
}

// mustTime parses an RFC3339 timestamp for fixture construction.
func mustTime(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}

// assertRoundTripsStructurally parses golden into a fresh instance of value's
// type, re-serializes it, and asserts the re-serialized JSON is structurally
// identical to the golden (key set, types, values, presence/absence). The
// prototype is only used for its type.
func assertRoundTripsStructurally(t *testing.T, golden []byte, prototype any) {
	t.Helper()
	var goldenGeneric any
	if err := json.Unmarshal(golden, &goldenGeneric); err != nil {
		t.Fatalf("golden is not valid JSON: %v", err)
	}
	dst := reflect.New(reflect.TypeOf(prototype)).Interface()
	if err := json.Unmarshal(golden, dst); err != nil {
		t.Fatalf("golden does not round-trip through %T: %v", prototype, err)
	}
	reser, err := json.Marshal(dst)
	if err != nil {
		t.Fatalf("re-serialize: %v", err)
	}
	var reserGeneric any
	if err := json.Unmarshal(reser, &reserGeneric); err != nil {
		t.Fatalf("re-serialized JSON invalid: %v", err)
	}
	if !reflect.DeepEqual(goldenGeneric, reserGeneric) {
		t.Errorf("structural drift round-tripping through %T\n--- golden ---\n%s\n--- re-serialized ---\n%s",
			prototype, golden, reser)
	}
}

// assertConformanceGolden is the corpus twin of assertGolden: emit under
// UPDATE_GOLDEN=1, else byte-compare. fields.json IS deterministically generated
// (sorted DTO names, declaration-order fields), so a byte compare is valid here
// — it is the emitting side that owns the canonical bytes.
func assertConformanceGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		writeConformanceFile(t, path, got)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (regenerate with make regen-conformance): %v", path, err)
	}
	if string(want) != string(got) {
		t.Errorf("conformance drift in %s — if intentional, regenerate with make regen-conformance\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
}

// writeConformanceFile creates parent dirs and writes a corpus file.
func writeConformanceFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("updated %s", path)
}
