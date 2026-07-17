package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EnvYieldFile names the yield file in the harness launch env. The yield-guard
// hook (a PreToolUse hook that fires on ALL tools) checks it before each tool;
// when present it blocks the tool so the agent stops within a turn — sub-minute
// cooperative preemption for any hook-capable harness (the absorbed yield
// capability).
const EnvYieldFile = "HW_YIELD_FILE"

// YieldControl is the caller's handle to request cooperative preemption of a
// running harness. The caller creates it, passes it in harness.Config.Yield
// (the orchestrator wires its path into the harness env as HW_YIELD_FILE), and
// calls Request mid-run to make the next tool block.
//
// It is safe to construct before the run and call from another goroutine while
// the run is in flight (Request/Clear are single filesystem ops).
type YieldControl struct {
	dir  string
	path string
}

// NewYieldControl allocates a private yield file under a fresh temp dir. The
// caller owns the lifecycle and should Close it when the run is done.
func NewYieldControl() (*YieldControl, error) {
	dir, err := os.MkdirTemp("", "hw-yield-")
	if err != nil {
		return nil, fmt.Errorf("harness: create yield dir: %w", err)
	}
	return &YieldControl{dir: dir, path: filepath.Join(dir, "yield.json")}, nil
}

// Request signals a yield: the next tool the harness attempts is blocked, with
// reason surfaced in the block message. Idempotent (re-requesting overwrites the
// reason). Written atomically so the guard never reads a partial file.
func (y *YieldControl) Request(reason string) error {
	data, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return fmt.Errorf("harness: marshal yield: %w", err)
	}
	return atomicWriteFile(y.path, data)
}

// FilePath is the yield file's path. The orchestrator wires it into the harness
// env as HW_YIELD_FILE; exposed so callers/tests can reference it directly.
func (y *YieldControl) FilePath() string { return y.path }

// Clear cancels a pending yield (removes the file). A nonexistent file is fine.
func (y *YieldControl) Clear() error {
	if err := os.Remove(y.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("harness: clear yield: %w", err)
	}
	return nil
}

// Close removes the yield file and its temp dir. Safe to call more than once.
func (y *YieldControl) Close() error {
	if y == nil || y.dir == "" {
		return nil
	}
	return os.RemoveAll(y.dir)
}

// HookOutcome is the result of HandleHookEvent. For capture events it is the
// zero value. For the yield-guard control hook it may direct the caller to BLOCK
// the tool: print BlockOutput to stdout and exit with a non-zero code the harness
// interprets as "block" (Claude: exit 2).
type HookOutcome struct {
	Block       bool
	BlockOutput string
}

// checkYield inspects the yield file and, if a yield was requested, returns a
// blocking HookOutcome carrying the harness's block-decision JSON. The protocol
// (decision:block + exit 2) is the shared shell-hook contract.
func checkYield(yieldFile string) HookOutcome {
	if yieldFile == "" {
		return HookOutcome{}
	}
	data, err := os.ReadFile(yieldFile) //nolint:gosec // path from the wrapper-set env
	if err != nil {
		return HookOutcome{} // no file ⇒ no yield ⇒ tool proceeds
	}
	reason := "unknown"
	var req struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(data, &req) == nil && req.Reason != "" {
		reason = req.Reason
	}
	out, _ := json.Marshal(map[string]string{
		"decision": "block",
		"reason":   fmt.Sprintf("Yield requested (%s) — please stop and exit immediately.", reason),
	})
	return HookOutcome{Block: true, BlockOutput: string(out)}
}
