// Package scenario loads recorded bake-off scenarios from disk.
//
// A scenario is a directory laid out as:
//
//	scenarios/<harness>/<name>/
//	    bytes.raw          required: raw PTY byte stream captured from the harness
//	    meta.json          required: harness, recorded_at, terminal dims, binary version
//	    expected.txt       optional: ground-truth final assistant text
//	    transcript.jsonl   optional: copy of the harness's own session log
//
// The bench replays bytes.raw through each emulator and compares the
// resulting screen snapshot against expected.txt.
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Meta is the parsed contents of meta.json.
type Meta struct {
	Harness       string    `json:"harness"`        // e.g. "codex", "claude-code"
	BinaryVersion string    `json:"binary_version"` // e.g. "codex 0.42.1"
	RecordedAt    time.Time `json:"recorded_at"`
	Cols          int       `json:"cols"`
	Rows          int       `json:"rows"`
	Notes         string    `json:"notes,omitempty"`
}

// Scenario is one loaded corpus entry.
type Scenario struct {
	Name     string
	Path     string
	Meta     Meta
	Bytes    []byte
	Expected string // contents of expected.txt; empty if missing
}

// Load loads a single scenario directory.
func Load(dir string) (*Scenario, error) {
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("read meta.json: %w", err)
	}
	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("parse meta.json: %w", err)
	}
	if meta.Cols == 0 {
		meta.Cols = 120
	}
	if meta.Rows == 0 {
		meta.Rows = 40
	}
	rawBytes, err := os.ReadFile(filepath.Join(dir, "bytes.raw"))
	if err != nil {
		return nil, fmt.Errorf("read bytes.raw: %w", err)
	}
	expected, err := os.ReadFile(filepath.Join(dir, "expected.txt"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read expected.txt: %w", err)
	}
	return &Scenario{
		Name:     filepath.Base(dir),
		Path:     dir,
		Meta:     meta,
		Bytes:    rawBytes,
		Expected: string(expected),
	}, nil
}

// Discover walks root and returns every scenario directory found.
// A directory qualifies as a scenario iff it contains meta.json.
func Discover(root string) ([]*Scenario, error) {
	var out []*Scenario
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(path, "meta.json")); err != nil {
			return nil
		}
		s, err := Load(path)
		if err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Meta.Harness != out[j].Meta.Harness {
			return out[i].Meta.Harness < out[j].Meta.Harness
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// WriteMeta is a convenience used by the recorder to emit meta.json.
func WriteMeta(dir string, m Meta) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
}
