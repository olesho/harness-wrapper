package harness

import "github.com/olesho/harness-wrapper/pkg/transcript"

// HookProvider is the capability for hook-driven transcript acquisition. A
// ResolvedProfile carries a non-nil Hooks only when hook support is confirmed
// for the run (statically firm for Claude; runtime-probed for Codex).
//
// The two methods serve the two sides of the hook lifecycle:
//   - HookSpec() describes WHAT to install (which native events map to which
//     `loom hooks <harness> <arg>` subcommand) so the orchestrator can
//     idempotently ensure the per-worktree hook config.
//   - ParseHookPayload() runs inside the fired hook SUBPROCESS: it parses the
//     harness's stdin payload (which HANDS OVER the transcript_path + session
//     id — no path reconstruction) and returns the canonical events, reading
//     the handed-over native transcript file on the file-bearing phases.
//
// ParseHookPayload is harness-STATIC (callable without Resolve): the hook
// subprocess is a fresh process that trusts it was invoked because the main
// run's resolution already passed, so it parses rather than re-detects.
type HookProvider interface {
	// HookSpec returns the static spec the orchestrator ensures in the
	// harness's per-worktree hook config. Non-nil ⇒ hooks are usable.
	HookSpec() *HookSpec

	// ParseHookPayload parses one fired hook's stdin payload into canonical
	// events, session-tagged. ctx carries the ENVIRONMENT (cwd/home/config/
	// spool, all wrapper-set); the payload carries the native transcript
	// LOCATION. It may return partial results with an error.
	ParseHookPayload(ctx HookContext, event string, stdin []byte) ([]transcript.ParsedEvent, error)

	// EnsureConfig idempotently + atomically installs the hook entries into the
	// harness's PER-WORKTREE config under worktreePath, rendering each command
	// from loomArgv (the loom binary path + "hooks", e.g. {"/abs/loom","hooks"})
	// via RenderHookCommand. It preserves the user's existing hooks, marks loom's
	// entries (owner marker) so they can be refreshed/removed, refreshes the
	// absolute loom path each call (a moved binary self-heals), and is safe under
	// concurrent same-worktree callers (flock-guarded, atomic temp+rename).
	EnsureConfig(worktreePath string, loomArgv []string) error
}

// StaticHookProfile is an OPTIONAL interface a Profile implements when the
// harness has a (static) HookProvider. It lets the fired hook SUBPROCESS obtain
// the payload parser WITHOUT running Resolve: static hook availability is a
// harness fact, distinct from per-run capability resolution — which the
// subprocess must not re-run (it would re-probe; review #1). The main run still
// gates the DECISION to install/use hooks on the resolved ResolvedProfile.Hooks.
type StaticHookProfile interface {
	StaticHookProvider() HookProvider
}

// HookContext is the hook-subprocess ENVIRONMENT, populated from the wrapper-set
// HW_* env (never the subprocess cwd) — the authority for environment. It is
// distinct from ResolveContext (run-detection inputs) and ReadContext (the
// read/export environment).
type HookContext struct {
	// Cwd is the harness working dir (the worktree), from HW_HOOK_CWD.
	Cwd string
	// Home is the user home, from HW_HOME — the base of the harness transcript
	// root when ConfigDir is unset.
	Home string
	// ConfigDir is the harness config dir (e.g. ~/.claude), from
	// HW_HARNESS_CONFIG_DIR. Empty ⇒ derive from Home.
	ConfigDir string
	// SpoolDir is where the hook subprocess writes parsed events, from
	// HW_EVENT_SPOOL. Empty ⇒ the handler is inert (the run is not a wrapper
	// run). Used by HandleHookEvent, not ParseHookPayload.
	SpoolDir string
}

// HookSpec describes the hook entries the orchestrator idempotently + atomically
// ensures in the harness's hook-config file. Per-harness variation is ONLY the
// config path + native event names + arg mapping — the shell+stdin-JSON contract
// is shared across Claude/Codex.
type HookSpec struct {
	// ConfigPath is the WORKTREE-RELATIVE path to the hook config (e.g.
	// ".claude/settings.json"); the orchestrator resolves it against the run's
	// working dir. Preferring per-worktree config keeps the global config
	// untouched (reviews #5/#10).
	ConfigPath string
	// Events are the lifecycle hooks loom manages.
	Events []HookEntry
	// Yield is the optional pre-tool guard enabling cooperative preemption.
	Yield *HookEntry
	// Owner is the loom owner/version marker stamped on managed entries so they
	// can be identified / upgraded / removed without disturbing user hooks.
	Owner string
}

// HookEntry maps one native hook event to its loom subcommand.
type HookEntry struct {
	// NativeEvent is the harness-native hook name written to the config (e.g.
	// "SessionStart", "PreToolUse").
	NativeEvent string
	// Matcher is the tool matcher (e.g. "Task" for the subagent hooks); empty
	// means match all.
	Matcher string
	// Arg is the `loom hooks <harness> <Arg>` subcommand the fired hook invokes
	// (e.g. "session-start", "stop", "pre-task").
	Arg string
}
