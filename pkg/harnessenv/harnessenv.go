// Package harnessenv is the single home of the Claude Code SESSION-NESTING env
// policy: the predicate that says which environment variables mark a process as
// running *inside* a Claude Code session, and the filter that removes them from
// an environment about to launch a harness.
//
// Why it exists as a package. When harness-wrapper (or a test, or any embedder)
// itself runs inside a Claude Code session, its environment carries CLAUDECODE /
// CLAUDE_CODE_* markers. A `claude` spawned with them inherited disables session
// persistence — it paints "⚠ Transcript saving is off" and writes no JSONL
// rollout. Everything downstream that reads that rollout then degrades silently:
// pkg/harness's transcript-backed History falls back to the chat store, and
// pkg/chat/swallowed.go loses the only evidence that can overturn a screen-only
// "prompt not accepted" verdict, so a lagged TUI repaint becomes a hard
// ErrTurnErrored. A top-level (scrubbed) env makes claude persist normally.
//
// The policy used to live unexported in cmd/harness-wrapper, which put it out of
// reach of every library entrypoint that needs it. (PUPPET-671)
//
// # The credential exemption
//
// CLAUDE_CODE_OAUTH_TOKEN is claude's long-lived headless credential, minted by
// `claude setup-token`. It is the equivalent of a ~/.claude login, not something
// a running claude exports into its children — it just happens to share the
// prefix. Stripping it made the spawned harness start UNAUTHENTICATED anywhere
// the token is the only working auth (a fresh host, a container, a CI runner):
// claude paints the login wall, no turn ever completes, and the run dies at the
// deadline (PUPPET-317; meta-harness saw the same as PUPPET-309, measured as
// ~285s deadline deaths in LIVE_CLAUDE=1 live_question.test.ts).
//
// The exemption is an EXACT match. CLAUDE_CODE_OAUTH_TOKEN_FILE,
// CLAUDE_CODE_OAUTH_TOKENX and friends are still stripped.
//
// Precedent for exempting a credential from an env filter: meta-harness
// src/chat/env.ts NESTING_EXEMPT (commit 379d162), loomcli
// internal/cli/envfilter/envfilter.go (exact allowlist) and
// internal/driver/env.go trustedLocalProviderCredentials.
//
// # Boundary
//
// This is a NESTING predicate, not a containment filter. It says nothing about
// what may cross into a guest or sandbox — internal/env/daytona/leak_probe.go
// independently and correctly lists CLAUDE_CODE_OAUTH_TOKEN in
// CredentialSensitiveEnvNames. Do not reuse this set there, and do not
// "reconcile" the two: they answer different questions and both answers are
// right.
package harnessenv

import (
	"os"
	"strings"
)

// nestingEnvKey is Claude Code's exact-match session-nesting marker;
// nestingEnvPrefix is the family prefix every other marker shares.
const (
	nestingEnvKey    = "CLAUDECODE"
	nestingEnvPrefix = "CLAUDE_CODE_"
)

// nestingExemptEnvKeys are keys that match nestingEnvPrefix but are NOT nesting
// markers, so they must survive the scrub. See the package doc for why
// CLAUDE_CODE_OAUTH_TOKEN is in here and why the match is exact.
var nestingExemptEnvKeys = map[string]bool{
	"CLAUDE_CODE_OAUTH_TOKEN": true,
}

// IsNestingKey reports whether key is one of Claude Code's session nesting
// markers. The exemption set is consulted first; see nestingExemptEnvKeys.
func IsNestingKey(key string) bool {
	if nestingExemptEnvKeys[key] {
		return false
	}
	return key == nestingEnvKey || strings.HasPrefix(key, nestingEnvPrefix)
}

// Filter returns src minus the Claude Code nesting markers, keeping the
// relative order of the survivors (a later duplicate key wins in exec, so
// reordering would be observable). Pure — it takes the environment as an
// argument so the policy is testable without mutating the process environment.
//
// The result is always non-nil, including for a nil or empty src: a nil Env
// means "inherit everything" to pkg/wrapper, which is the exact opposite of
// what a caller asking for a scrub wants.
func Filter(src []string) []string {
	out := make([]string, 0, len(src))
	for _, kv := range src {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if IsNestingKey(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Cleaned returns the current environment minus Claude Code's nesting markers,
// so a spawned `claude` runs as a top-level (persisting) session.
// CLAUDE_CODE_OAUTH_TOKEN is deliberately preserved — see the package doc.
func Cleaned() []string { return Filter(os.Environ()) }
