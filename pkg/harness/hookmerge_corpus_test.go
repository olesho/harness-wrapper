package harness

// Behavioral-parity test for the shared hook-merge dedup/authority corpus
// (test/corpus/hook-merge). It runs every fixture's input event sequence
// through Go's REAL identity + authority path — transcript.Event.ID() for
// dedup and admitParent for source authority — normalizes the surviving set as
// documented in the corpus README (sort by stable identity, drop Seq), and
// diffs against the committed golden.
//
// It deliberately does NOT introduce a MergeHookEvents dedup ladder: the ladder
// already lives on transcript.Event.ID(). The corpus goldens are authored to
// match Go's own Event.ID()-dedup + admitParent output; aligning them against
// meta-harness's TS mergeHookEvents (which is not in this worktree) is a
// separate cross-repo step, per the corpus README.
//
// Regenerate goldens after an intentional fixture change:
//
//	go test ./pkg/harness -run TestHookMergeCorpus -update
//
// then eyeball the diff and commit.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// updateHookMerge rewrites every golden.json from the live Go path. Off by
// default so CI asserts against the committed goldens.
var updateHookMerge = flag.Bool("update", false, "rewrite hook-merge corpus goldens from the live Go path")

const hookMergeCorpusRoot = "../../test/corpus/hook-merge"

// hookMergeInputEvent is one input event as authored in a fixture's input.json.
// It is a flat DTO because transcript.Event hides Source and NativeID behind
// json:"-" (they are durable-store metadata, not the public wire shape), yet
// both drive the dedup/authority path and so must be author-controllable here.
type hookMergeInputEvent struct {
	// Envelope-level acquisition metadata.
	Source           string `json:"source"`             // "live" | "file"
	HarnessSessionID string `json:"harness_session_id"` // dedup scope; parent vs subagent
	ParentSessionID  string `json:"parent_session_id"`  // non-empty ⇒ subagent (admitted in any mode)

	// transcript.Event fields (native_id maps to Event.NativeID, json:"-").
	NativeID  string          `json:"native_id"`
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Timestamp string          `json:"timestamp"` // RFC3339; NATIVE timestamp (folded into the content-hash rung)
	Text      string          `json:"text"`
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
	Output    string          `json:"output"`
	UUID      string          `json:"uuid"`
}

// hookMergeInput is one fixture's input.json: a latched acquisition mode plus an
// ordered event sequence (arrival order).
type hookMergeInput struct {
	Mode   string                `json:"mode"` // "hooks" | "stream" (latched effective mode)
	RunID  string                `json:"run_id"`
	Notes  string                `json:"notes,omitempty"`
	Events []hookMergeInputEvent `json:"events"`
}

// hookMergeGoldenEvent is one row of a golden.json: the stable identity plus the
// content-bearing fields of a surviving event. Seq and arrival order are
// intentionally absent — the golden is a SET keyed by (harness_session_id, id).
type hookMergeGoldenEvent struct {
	ID               string `json:"id"` // transcript.Event.ID() — the dedup identity
	HarnessSessionID string `json:"harness_session_id"`
	Source           string `json:"source"`
	Type             string `json:"type"`
	Role             string `json:"role"`
	Timestamp        string `json:"timestamp"`
	Text             string `json:"text,omitempty"`
	ToolName         string `json:"tool_name,omitempty"`
	ToolUseID        string `json:"tool_use_id,omitempty"`
	Output           string `json:"output,omitempty"`
	UUID             string `json:"uuid,omitempty"`
}

// hookMergeGolden is the committed deduped/normalized output for a fixture.
type hookMergeGolden struct {
	Events []hookMergeGoldenEvent `json:"events"`
}

// parseMode maps the fixture's latched-mode string onto the Mode admitParent
// expects. Only the two latched parent strategies are legal in a fixture; Auto
// is resolved before any event reaches the filter.
func parseMode(t *testing.T, s string) Mode {
	t.Helper()
	switch s {
	case "hooks":
		return TranscriptHooks
	case "stream":
		return TranscriptStreamParse
	default:
		t.Fatalf("fixture mode %q must be %q or %q", s, "hooks", "stream")
		return TranscriptOff
	}
}

// toEvent builds a transcript.Event from an input DTO, wiring the json:"-"
// fields (Source, NativeID) the DTO carries explicitly.
func (ie hookMergeInputEvent) toEvent(t *testing.T) transcript.Event {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, ie.Timestamp)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", ie.Timestamp, err)
	}
	return transcript.Event{
		Timestamp: ts,
		Role:      ie.Role,
		Type:      ie.Type,
		Text:      ie.Text,
		ToolName:  ie.ToolName,
		ToolUseID: ie.ToolUseID,
		ToolInput: ie.ToolInput,
		Output:    ie.Output,
		UUID:      ie.UUID,
		Source:    ie.Source,
		NativeID:  ie.NativeID,
	}
}

// mergeHookCorpus is the behavior under test, assembled from the SAME two
// primitives production uses: admitParent (source authority, per emit()) and
// the (RunID, HarnessSessionID, Event.ID()) consumer dedup documented on
// EventEnvelope. First admitted copy of an identity wins (stable, order-
// independent after normalization). The result is normalized: sorted by
// (HarnessSessionID, ID) and stripped of Seq, so it is a set, not a stream.
func mergeHookCorpus(t *testing.T, in hookMergeInput) []hookMergeGoldenEvent {
	t.Helper()
	mode := parseMode(t, in.Mode)

	type dedupKey struct{ hsid, id string }
	seen := make(map[dedupKey]bool)
	var out []hookMergeGoldenEvent

	for _, ie := range in.Events {
		ev := ie.toEvent(t)
		isSubagent := ie.ParentSessionID != ""
		// Authority filter — identical predicate to emit() in run.go.
		if !admitParent(mode, ev.Source, ev.Type, isSubagent) {
			continue
		}
		hsid := ie.HarnessSessionID
		if hsid == "" {
			hsid = in.RunID // mirror emit()'s fallback to the run/session id
		}
		key := dedupKey{hsid: hsid, id: ev.ID()}
		if seen[key] {
			continue // (RunID, HarnessSessionID, Event.ID()) consumer dedup
		}
		seen[key] = true
		out = append(out, hookMergeGoldenEvent{
			ID:               ev.ID(),
			HarnessSessionID: hsid,
			Source:           ev.Source,
			Type:             ev.Type,
			Role:             ev.Role,
			Timestamp:        ev.Timestamp.Format(time.RFC3339Nano),
			Text:             ev.Text,
			ToolName:         ev.ToolName,
			ToolUseID:        ev.ToolUseID,
			Output:           ev.Output,
			UUID:             ev.UUID,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].HarnessSessionID != out[j].HarnessSessionID {
			return out[i].HarnessSessionID < out[j].HarnessSessionID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// loadHookMergeInput reads and parses one fixture's input.json.
func loadHookMergeInput(t *testing.T, dir string) hookMergeInput {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "input.json"))
	if err != nil {
		t.Fatalf("read input.json: %v", err)
	}
	var in hookMergeInput
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("parse input.json: %v", err)
	}
	return in
}

// TestHookMergeCorpus is the behavioral-parity gate: every fixture's input,
// run through Go's Event.ID()-dedup + admitParent and normalized as documented,
// must equal the committed golden.json.
func TestHookMergeCorpus(t *testing.T) {
	entries, err := os.ReadDir(hookMergeCorpusRoot)
	if err != nil {
		t.Fatalf("read corpus root %s: %v", hookMergeCorpusRoot, err)
	}
	var cases []string
	for _, e := range entries {
		if e.IsDir() {
			cases = append(cases, e.Name())
		}
	}
	if len(cases) == 0 {
		t.Fatalf("no hook-merge corpus cases under %s", hookMergeCorpusRoot)
	}
	sort.Strings(cases)

	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(hookMergeCorpusRoot, name)
			in := loadHookMergeInput(t, dir)
			got := mergeHookCorpus(t, in)

			goldenPath := filepath.Join(dir, "golden.json")
			if *updateHookMerge {
				writeHookMergeGolden(t, goldenPath, got)
				return
			}

			wantBytes, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden.json (run with -update to create): %v", err)
			}
			var want hookMergeGolden
			if err := json.Unmarshal(wantBytes, &want); err != nil {
				t.Fatalf("parse golden.json: %v", err)
			}
			if !reflect.DeepEqual(got, want.Events) {
				gotJSON, _ := json.MarshalIndent(hookMergeGolden{Events: got}, "", "  ")
				t.Errorf("deduped/normalized set != golden\n--- got ---\n%s\n--- want ---\n%s", gotJSON, wantBytes)
			}
		})
	}
}

// writeHookMergeGolden serializes the computed set to golden.json with the same
// 2-space indentation + trailing newline the committed goldens use.
func writeHookMergeGolden(t *testing.T, path string, events []hookMergeGoldenEvent) {
	t.Helper()
	b, err := json.MarshalIndent(hookMergeGolden{Events: events}, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}
