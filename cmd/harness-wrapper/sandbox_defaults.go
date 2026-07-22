package main

import (
	"strings"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// skipPermissionsFlag is the claude-code arg --sandbox-defaults injects. It is
// an alias, not a second copy: pkg/wrapper recognizes the same token when it
// reconciles Args against PermissionMode, so the string has exactly one home.
const skipPermissionsFlag = wrapper.SkipPermissionsFlag

// sandboxEnvKey is the env key --sandbox-defaults sets (to "1") for claude.
// IS_SANDBOX=1 suppresses claude-code's "Bypass Permissions mode" acceptance
// screen entirely and allows running as root — relevant for container
// workspaces under internal/env.
const sandboxEnvKey = "IS_SANDBOX"

// applySandboxDefaults implements the --sandbox-defaults policy: for the
// "claude" harness it appends --dangerously-skip-permissions to the harness
// args and IS_SANDBOX=1 to the env; for any other harness both slices are
// returned unchanged (a documented no-op, matching meta-harness's
// --sandbox-defaults semantics — same argv, same behavior across the two
// implementations).
//
// The injection lives HERE, at the CLI boundary, and deliberately NOT in
// pkg/harness.RunTurn: TurnConfig.Args documents verbatim passthrough, and a
// danger-carrying policy toggle stays trivially auditable when it is confined
// to cmd/harness-wrapper. The cost is this small duplicated harness-name
// check — the library's canonical dispatch is turnHarnessName
// (pkg/harness/run_turn.go).
//
// Idempotence:
//   - Args: nothing is appended when the caller already passed the flag —
//     matched as the exact token OR the "--dangerously-skip-permissions="
//     prefix form (claude accepts --flag=value spellings; exact-token-only
//     dedup would double-inject on --dangerously-skip-permissions=true).
//   - Env: IS_SANDBOX=1 is not appended when the env already defines the
//     IS_SANDBOX key, whatever its value (containers may set it). The key
//     match is exact at the '=' boundary — a different key sharing the prefix
//     (IS_SANDBOXED=…) does not suppress injection.
//
// Composition with --permission-mode: the two halves are NOT interchangeable,
// which is why the pairing composes instead of being mutually excluded.
// --permission-mode bypass delivers only the ARGS half (pkg/wrapper emits
// --permission-mode bypassPermissions); IS_SANDBOX=1 — the half that permits
// running as root and suppresses claude's Bypass Permissions acceptance screen
// — is delivered by --sandbox-defaults alone. bypass + --sandbox-defaults is
// therefore exactly the recipe a root container needs, and a blanket mutual
// exclusion would have outlawed the one legitimate combination.
//
// So when permissionMode is bypass-class (wrapper.IsBypassPermissionMode) this
// function contributes the ENV HALF ONLY: it skips the arg append and lets
// pkg/wrapper own the permission directive in argv, leaving exactly one such
// directive there instead of two spellings of the same intent. The injected env
// is byte-identical either way — this is not a change to --sandbox-defaults
// semantics. Every OTHER mode never reaches here: parseHarnessWrapperArgs
// rejects the pairing up front.
//
// The env/args split is load-bearing and stays that way: the ARG half must be
// reachable by every wrapper.Start caller (passthrough included), so it lives in
// pkg/wrapper; the ENV half grants root and must stay auditable in this one CLI
// file, so it lives here and nowhere else.
//
// The compose path is reachable ONLY for the literal harness name "claude":
// the guard below is an exact string match with NO normHarness normalization
// (unlike pkg/wrapper, which does normalize). That is deliberate and
// load-bearing, not an oversight — the CLI's supported names are exactly
// claude, codex, opencode, pi, so the alias "claude-code" never reaches this
// function from the CLI. Do not "fix" this into a normalized match: it would
// quietly widen the set of invocations that receive the root-enabling env half.
func applySandboxDefaults(harnessName, permissionMode string, args, env []string) ([]string, []string) {
	if harnessName != "claude" {
		return args, env
	}
	if !wrapper.IsBypassPermissionMode(permissionMode) && !hasSkipPermissionsFlag(args) {
		args = append(args[:len(args):len(args)], skipPermissionsFlag)
	}
	if !hasEnvKey(env, sandboxEnvKey) {
		env = append(env[:len(env):len(env)], sandboxEnvKey+"=1")
	}
	return args, env
}

// hasSkipPermissionsFlag reports whether args already carries
// --dangerously-skip-permissions, in either the bare-token or the =value form.
func hasSkipPermissionsFlag(args []string) bool {
	for _, a := range args {
		if a == skipPermissionsFlag || strings.HasPrefix(a, skipPermissionsFlag+"=") {
			return true
		}
	}
	return false
}

// hasEnvKey reports whether env defines key, matching exactly at the '='
// boundary (the cleanedEnv key-extraction idiom — NOT a raw prefix match,
// which would wrongly treat IS_SANDBOXED=… as defining IS_SANDBOX).
func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if k == key {
			return true
		}
	}
	return false
}
