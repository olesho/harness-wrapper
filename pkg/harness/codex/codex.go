// Package codex implements the harness.Profile for the OpenAI Codex CLI.
//
// SCOPE (resume-only): only the resume capability is registered. Codex's other
// capabilities are deliberately NOT implemented yet because their contracts are
// under-documented / unreliable and cannot be validated against a real CLI here:
//   - `codex exec --json` is documented-as-flaky (silently ignored when MCP /
//     tools are active, and the JSON docs are out of date — openai/codex #15451,
//     #4776), so a StreamParser would be a guess that could silently capture
//     nothing. Deferred until validatable.
//   - Codex shell hooks are undocumented (openai/codex #16226/#21990), so NO
//     HookProvider is registered — the orchestrator never installs codex hooks
//     on a guess (the plan's probe-driven, never-assume stance).
//   - Rollout/export parsing is likewise deferred.
//
// The resume arg form IS documented (`codex exec resume <id>`); reliability is
// version/timing-dependent, but loom falls back to checkpoint injection when a
// resume fails, so registering it is safe and useful.
//
// Importing this package registers the "codex" profile (see init).
package codex

import "github.com/olesho/harness-wrapper/pkg/harness"

// Profile is the Codex CLI harness profile.
type Profile struct{}

// Name implements harness.Profile.
func (Profile) Name() string { return "codex" }

// Resolve populates only the resume capability (see package doc for why the
// others are deferred). ctx is unused.
func (Profile) Resolve(_ harness.ResolveContext) harness.ResolvedProfile {
	return harness.ResolvedProfile{
		Resume: resumer{},
	}
}

// resumer produces Codex's resume fragment. Codex resumes via the `exec resume`
// SUBCOMMAND, so the fragment is {"resume", id} — the caller composes it into
// its `codex exec ...` invocation (placed right after `exec`). Returns nil for
// an empty id (cold start).
type resumer struct{}

func (resumer) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"resume", sessionID}
}

func init() {
	harness.Register("codex", Profile{})
}
