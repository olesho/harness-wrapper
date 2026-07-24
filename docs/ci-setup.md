# CI setup

This project's CI (`.github/workflows/ci.yml`) runs `veracity ci`, the **authoritative**
gate: lock verification, per-project lint + tests, documentation freshness, and
cross-project isolation checks against the exact commit.

The local git hooks and agent hooks are fast, advisory backstops only — they are
bypassable (`git commit --no-verify`) and do not run for changes pushed from
another machine. **Only CI is authoritative.**

## Make CI required (external, one-time)

GitHub does not enforce a workflow as required just because it exists. In your
repository settings, add a branch protection rule (or ruleset) for your default
branch and mark the `veracity-ci` check **Required**. Until you do, a red CI run
does not block merges.

## SonarQube is local-only (not run in CI)

The `sonar` verifier targets a **self-hosted** SonarQube reachable only from a
developer's machine, so it is **skipped on CI runners** (detected via `CI=true`).
It runs on the local `pre-push` hook and on a local `veracity ci` invocation only —
remote CI never attempts a scan, and CI stays green regardless.
