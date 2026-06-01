// Package harness is the per-harness orchestration + capability layer for
// harness-wrapper. It sits ABOVE the thin pkg/wrapper supervisor: a caller
// resolves a per-harness Profile to a run-specific ResolvedProfile (after any
// runtime capability detection) and dispatches on the capabilities that are
// actually available for that run.
//
// This package is DISTINCT from pkg/turns/harness/* (the per-harness TUI
// turn adapters); this one owns resume + transcript-acquisition capabilities.
//
// Capability model (see ResolvedProfile): rather than static type-asserts on a
// fat interface — which would wrongly treat a probe-gated harness (Codex) as
// available before detection — Profile.Resolve runs detection ONCE and returns
// a ResolvedProfile whose capability fields are non-nil only when CONFIRMED for
// this run. Callers check `rp.<Cap> != nil`, never the static profile.
//
// The ResolvedProfile starts (P1) with only the SessionID + Resume capabilities
// populated for Claude; later phases ADD fields (Stream/Hooks/Reader/Export) —
// an additive change, not a breaking re-migration of the Profile shape.
package harness

import (
	"fmt"
	"sort"
	"sync"
)

// Profile is the per-harness entry point. Implementations live in
// pkg/harness/<name> and self-register via Register in their init().
type Profile interface {
	// Name is the wrapper harness key ("claude", "codex", "gemini", ...).
	Name() string

	// Resolve runs any runtime capability detection for this run and returns
	// the capabilities CONFIRMED available. It must not mutate harness config.
	Resolve(ctx ResolveContext) ResolvedProfile
}

// ResolveContext carries the RUN-DETECTION inputs Resolve needs — the binary,
// its args/env, the working dir, and config roots to probe. It is deliberately
// SEPARATE from HookContext (the hook-subprocess environment) and ReadContext
// (the transcript read/export environment), both introduced in later phases.
type ResolveContext struct {
	// BinaryPath is the resolved path to the harness executable.
	BinaryPath string
	// Args/Env are what the harness will be launched with.
	Args []string
	Env  []string
	// Cwd is the harness working directory.
	Cwd string
	// ConfigRoots are directories where the harness's config/hook files live
	// (e.g. <worktree>/.claude, ~/.codex) — probed non-destructively.
	ConfigRoots []string
}

// ResolvedProfile is the post-detection capability set for ONE run. A field is
// non-nil only when that capability is available this run. The orchestrator
// dispatches on these fields, never on the static Profile.
//
// P1 populates SessionID + Resume (Claude). Later phases add capability fields
// (StreamParser, HookProvider, TranscriptReader, Exporter) additively.
type ResolvedProfile struct {
	// SessionID extracts the harness's session UUID from a line of headless
	// stream output. Used for resume (the captured id) and, later, to key the
	// on-disk transcript file. nil ⇒ unavailable this run.
	SessionID SessionIDExtractor
	// Resume produces the CLI arg prefix that resumes a captured session.
	// nil ⇒ resume unavailable (caller falls back to checkpoint).
	Resume Resumer
}

// SessionIDExtractor recovers the harness-assigned session UUID from a single
// line of the harness's headless stream output. Stateless and idempotent:
// callers invoke it per line and keep the first non-empty result.
type SessionIDExtractor interface {
	// ExtractSessionID returns the session id if this line carries it, else
	// ("", false).
	ExtractSessionID(line string) (string, bool)
}

// Resumer produces the resume-specific CLI argument prefix for a given session
// id. The caller appends its own policy flags (output format, prompt, etc.).
type Resumer interface {
	// ResumeArgs returns the argv fragment that resumes sessionID (e.g.
	// {"--resume","--session-id",id}). Returns nil for an empty id.
	ResumeArgs(sessionID string) []string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Profile{}
)

// Register adds a Profile under name. Per-harness packages call this from their
// init(); panics on a duplicate to catch double-registration at startup.
func Register(name string, p Profile) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("harness: Register called twice for %q", name))
	}
	registry[name] = p
}

// For returns the registered Profile for the named harness. ok=false means the
// harness has no profile (caller degrades: resume→checkpoint, transcript→floor).
// The relevant pkg/harness/<name> package must be imported for its init() to
// run — blank-import pkg/harness/all to register all built-ins.
func For(name string) (Profile, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	return p, ok
}

// Registered returns the sorted names of all registered profiles (for tests
// and diagnostics).
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
