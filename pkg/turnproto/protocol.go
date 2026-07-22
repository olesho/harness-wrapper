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

// ExitCode is the ONE canonical status → process-exit-code table: the single
// function every producer of the coarse orchestration signal delegates to, so
// the mapping cannot drift across the guest structured-run subcommand and any
// host-side turn client. Only a deadline changes the code to 124; a clean
// completion exits 0; errored / startup_error / any unexpected status exit 1.
//
// It lives HERE, in the package that already owns the exit vocabulary
// (ExitOK/ExitError/ExitDeadline) and the TurnStatus type — a leaf importing
// only pkg/transcript — so homing it costs no importer a dependency on the
// in-process PTY/turn runtime (pkg/harness). pkg/oneshot never returns an exit
// code; it returns the status and the caller maps it here.
func ExitCode(s TurnStatus) int {
	switch s {
	case StatusCompleted:
		return ExitOK
	case StatusDeadline:
		return ExitDeadline
	default:
		// errored / startup_error / any unexpected status.
		return ExitError
	}
}

// StructuredTurnResult is the single JSON line the guest structured-run
// subcommand emits on stdout, and the shape a host turn client parses back
// (meta-harness design §7 step 3).
//
// FROZEN schema: the five required keys are ALWAYS present with these types; the
// four optional keys are present-with-type when set and ABSENT otherwise
// (encoding/json omits them via omitempty — an exact-string match would be
// flaky). The `usage` field from MH is an EMITTED optional key: present when a
// usage reader yielded counts, absent when unset, and its five inner keys always
// serialize (including zeros) to stay byte-identical with MH's usageToPublicJSON.
// The JSON tag space is kept additively compatible so further fields can be
// reintroduced without a schema break.
//
// JSON tag spellings are load-bearing for cross-repo fidelity: harnessSessionID
// is camelCase; transcript_entries / working_dir / transcript_error /
// permission_mode are snake_case.
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
	// Usage is optional token accounting; present only when a usage reader
	// yielded counts (encoding/json omits it via omitempty when nil). Its five
	// inner keys always serialize (no inner omitempty) to stay byte-identical
	// with meta-harness's usageToPublicJSON.
	Usage *transcript.Usage `json:"usage,omitempty"`
	// PermissionMode is the canonical permission rung the turn was LAUNCHED at
	// ("plan" | "manual" | "ask" | "auto" | "bypass"), resolved by
	// wrapper.EffectiveLaunchRung from the FINAL launch args + the requested
	// mode. Read the caveats before consuming it — they are as load-bearing as
	// the value:
	//
	//   - LAUNCH-TIME, not observed. It is what the harness was spawned with, not
	//     a live reading of the harness and not a guarantee for the lifetime of
	//     the process.
	//   - NEVER populated on startup_error: no harness was launched, so there is
	//     no rung to report. Echoing a mode back on a run that failed BECAUSE the
	//     mode was rejected would be actively misleading. deadline and errored DO
	//     carry it — those turns reached the harness.
	//   - Presence of "bypass" is trustworthy for turns that reached the harness:
	//     every unrestricted launch path reports it, including the ones carrying
	//     no canonical --permission-mode at all (--sandbox-defaults' injected
	//     --dangerously-skip-permissions, a raw --dangerously-skip-permissions
	//     after --, codex's -s danger-full-access in every spelling).
	//   - ABSENT NEVER MEANS "SAFE". It means no canonical rung could be named:
	//     the harness default, an unsupported harness, a harness-native spelling
	//     with no canonical equivalent (claude's dontAsk), a codex argv setting
	//     only the -a approval axis, or a present-but-unreadable flag.
	//   - A RESTRICTIVE RUNG IS A LAUNCH ARGUMENT, NOT AN ENFORCEMENT CLAIM —
	//     least of all on this path. Under structured-run, claude's per-tool
	//     permission dialog is not detected at all (no InputRequest is emitted, so
	//     plan/manual/ask stall an unattended turn to the deadline), and
	//     OnInputRequest is wired unconditionally to the catch-all
	//     oneshot.AutoAcceptAnswer (pkg/oneshot/oneshot.go:179-185, :206), which
	//     auto-approves codex's KindApproval — so -a on-request / -a untrusted
	//     bind nothing here. Only codex's -s sandbox axis is enforced by codex
	//     itself. See pkg/oneshot/permission_pin_test.go and
	//     docs/md/internal/wrapper.md §"Runtime enforcement per path". Do NOT read
	//     "manual" off a structured run as "this turn was gated".
	//   - The argv/rung half ONLY. The IS_SANDBOX=1 env half that
	//     --sandbox-defaults also sets (permits root, suppresses claude's Bypass
	//     Permissions acceptance screen) is not representable in a rung string and
	//     is not reflected here.
	//   - In-band mutation (claude's /permissions, codex's /approvals) is not
	//     observed — the same residual gap chatd documents.
	//
	// DIVERGENCE — same key name, two readings, by design.
	// cmd/harness-chatd's conversationSummary.permission_mode
	// (cmd/harness-chatd/types.go:104-137) reports what was REQUESTED — the value
	// echoed verbatim from openRequest — and isSupportedPermissionMode
	// (pkg/wrapper/wrapper.go:560-588) accepts harness-native spellings, so that
	// field can legitimately emit acceptEdits, dontAsk or read-only on the wire
	// today (see also the PermissionMode Literal in
	// clients/python/harness_chat.py:43-56). THIS field reports the EFFECTIVE
	// CANONICAL RUNG and never emits a native spelling. Do not reuse one
	// parser/type/zod schema for both.
	PermissionMode string `json:"permission_mode,omitempty"`
}
