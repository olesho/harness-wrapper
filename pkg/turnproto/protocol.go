// Package turnproto is the ONE source of truth for the neutral structured-turn
// protocol: the exit codes, the deadline anchor line, and the result-schema type
// shared by the producer (the guest structured-run subcommand) and any host-side
// turn client.
//
// Faithful standalone backport of meta-harness src/turnproto/protocol.ts +
// src/turnproto/parse.ts. Every exported name and JSON tag here is a stable
// cross-repo contract (meta-harness design §7 "Protocol ownership") — a later
// subtask (the guest structured-run subcommand) imports these types and
// constants, so do not rename fields or change JSON tag spellings.
package turnproto

import "github.com/olesho/harness-wrapper/pkg/transcript"

// Exit codes — the coarse orchestration signal. The JSON payload on stdout is
// the source of truth; these mirror the orchestrator's headless reply() parser.
const (
	ExitOK    = 0
	ExitError = 1
	// ExitUsage is carried for protocol fidelity with meta-harness ONLY. This
	// ticket drops the MH `usage` machinery, so nothing in Go emits exit 2; there
	// is no emit path for it here. A future Go usage reader would reintroduce it.
	ExitUsage    = 2
	ExitDeadline = 124
)

// DeadlineLine is the literal stderr anchor the orchestrator's deadline regex
// matches on a 124 exit. FROZEN string — do not reword. It must match MH and the
// text cmd/harness-wrapper/run.go already prints for a context.DeadlineExceeded
// error (`fmt.Fprintln(os.Stderr, "harness-wrapper run:", err)`).
const DeadlineLine = "harness-wrapper run: context deadline exceeded"

// TurnStatus is the turn's coarse status, mirroring meta-harness's OneShotOutcome
// union.
type TurnStatus string

// The four frozen TurnStatus values.
const (
	StatusCompleted    TurnStatus = "completed"
	StatusErrored      TurnStatus = "errored"
	StatusDeadline     TurnStatus = "deadline"
	StatusStartupError TurnStatus = "startup_error"
)

// StructuredTurnResult is the single JSON line the guest structured-run
// subcommand emits on stdout, and the shape a host turn client parses back
// (meta-harness design §7 step 3).
//
// FROZEN schema: the five required keys are ALWAYS present with these types; the
// two optional keys are present-with-type when set and ABSENT otherwise
// (encoding/json omits them via omitempty — an exact-string match would be
// flaky). The `usage` field from MH is intentionally DROPPED for this ticket (Go
// has no usage/token reader); the JSON tag space is kept additively compatible so
// a later `usage` field can be reintroduced without a schema break.
//
// JSON tag spellings are load-bearing for cross-repo fidelity: harnessSessionID
// is camelCase; transcript_entries / working_dir / transcript_error are
// snake_case.
type StructuredTurnResult struct {
	// Status is the coarse status of the turn.
	Status TurnStatus `json:"status"`
	// Reply is the clean reply text; "" on any non-completed status.
	Reply string `json:"reply"`
	// HarnessSessionID is the harness's own session id ("" when unrecoverable).
	HarnessSessionID string `json:"harnessSessionID"`
	// TranscriptEntries is the canonical transcript, read in-guest.
	TranscriptEntries []transcript.Event `json:"transcript_entries"`
	// WorkingDir is the guest working directory the turn ran in.
	WorkingDir string `json:"working_dir"`
	// Reason is failure detail; present on errored/startup_error.
	Reason string `json:"reason,omitempty"`
	// TranscriptError is a best-effort transcript read failure; present only when
	// the read failed.
	TranscriptError string `json:"transcript_error,omitempty"`
}
