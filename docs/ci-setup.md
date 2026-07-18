# CI setup

This project's CI (`.github/workflows/ci.yml`) runs `harness ci`, the **authoritative**
gate: lock verification, per-project lint + tests, documentation freshness, and
cross-project isolation checks against the exact commit.

The local git hooks and agent hooks are fast, advisory backstops only — they are
bypassable (`git commit --no-verify`) and do not run for changes pushed from
another machine. **Only CI is authoritative.**

## Make CI required (external, one-time)

GitHub does not enforce a workflow as required just because it exists. In your
repository settings, add a branch protection rule (or ruleset) for your default
branch and mark the `harness-ci` check **Required**. Until you do, a red CI run
does not block merges.

## SonarQube in CI (optional)

If the `sonar` feature is enabled, `harness ci` attempts a SonarQube scan. It
needs a reachable server (`SONAR_HOST_URL`) and a `SONAR_TOKEN` in the runner's
environment. When either is absent, the scan **soft-skips** (warns, does not
fail), so CI stays green even where the self-hosted server isn't reachable.
