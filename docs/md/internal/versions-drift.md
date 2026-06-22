# Versions & Drift

Adapters screen-scrape tools we don't control. When an upstream CLI ships a new version, its TUI
markers, classifier strings, or transcript schema can shift and silently break detection. This page
covers the **pin file**, the **discovery** probe, and the developer-on-demand **drift pipeline** that
catches breakage before users do.

## The pin file

`pkg/versions/versions.json` pins each harness to the upstream version its adapter was last verified
against. It is embedded into `pkg/versions` at build time.

```json
{
  "codex":       {"package": "@openai/codex",              "binary": "codex",    "pinned": "0.141.0", "verified_at": "2026-06-21"},
  "claude-code": {"package": "@anthropic-ai/claude-code",  "binary": "claude",   "pinned": "2.1.185", "verified_at": "2026-06-21"},
  "gemini":      {"package": "@google/gemini-cli",         "binary": "gemini",   "pinned": "",        "verified_at": ""},
  "opencode":    {"package": "opencode-ai",                "binary": "opencode", "pinned": "",        "verified_at": ""},
  "pi":          {"package": "@earendil-works/pi-coding-agent", "binary": "pi",  "pinned": "",        "verified_at": ""}
}
```

An empty `pinned` means "no corpus captured yet". The read API:

```go
versions.All() (map[string]Entry, error)        // every entry (embedded)
versions.Pinned(harness string) (string, bool)  // pinned version, or ("", false)
versions.ReadFrom(path string) (...)            // read an explicit file (rebake pipeline)
```

`pkg/discovery` answers "is harness X installed, at what version?" — a `semverDashVProbe` runs
`<binary> --version` and extracts the first `X.Y.Z[-suffix]`, behind an mtime-keyed cache.

## The pipeline

Drift detection runs entirely on the developer's machine — there is no CI cron. `check-versions` is the
cheap offline signal; a real shift is confirmed by re-baking the corpus and seeing the adapter tests
go red.

![Drift-detection pipeline](../diagrams/drift-pipeline.svg)

```bash
make check-versions        # offline: pinned vs npm registry /latest (~2s, free)
make schema-canary-gemini  # re-record gemini short-reply + re-parse the fresh JSONL
make rebake-corpus HARNESS=<name> SCENARIO=<name>   # refresh one scenario (paid for codex/claude)
make rebake-corpus-all     # refresh all 18 (6 scenarios × 3 harnesses), then run adapter tests
```

The canonical lists live in the `Makefile`:
`SCENARIOS = short-reply long-markdown code-block interrupted-mid-reply tool-call multi-turn`;
`HARNESSES = codex claude gemini`.

`make check-versions` runs `internal/cmd/upstream-version-sentry`, which compares each pin against
`https://registry.npmjs.org/<package>/latest`. Exit codes: **0** all pins current, **1** drift
detected, **2** registry unreachable.

## When `check-versions` shows drift

A new release exists; the corpus hasn't been verified against it yet — it may still be
backwards-compatible.

1. Install it locally (e.g. `npm i -g @anthropic-ai/claude-code@<ver>`).
2. Re-bake the affected harness and run its adapter regression:
   ```bash
   for s in short-reply long-markdown code-block interrupted-mid-reply tool-call multi-turn; do
       make rebake-corpus HARNESS=claude SCENARIO=$s
   done
   go test -race ./pkg/turns/harness/claudecode/...
   ```
3. **Green** → it's backwards-compatible. **Red** → a marker shifted (next section).
4. Either way, finish by setting `pinned`/`verified_at` in `versions.json`, refreshing the adapter's
   "Verified against …" package comment, and committing `test/corpus/<harness>/**` + the version bump.

## When marker drift is real

`rebake-corpus-all` exited non-zero. Diagnose:

1. **Find the failing scenario** from the adapter test output.
2. **Diff fresh vs. old bytes** — render both with `screenbench` and compare against the previous
   `bytes.raw` (`git diff`); the visible delta is the marker shift (usually a footer line or status
   verb format).
3. **Pinpoint the regex** — each adapter keeps its end-of-turn match at one named regex
   (`tokenUsageRE` in codex, `thinkingRE` in claude-code).
4. **Update + re-anchor** it, keeping the line-anchor discipline (`(?m)^…$`) so the
   [adversarial corpus](testing/corpus.md) tests keep passing.
5. **Re-run** `go test ./pkg/turns/harness/<harness>/...` — canonical and adversarial must both pass.
6. Bump the pin and commit.

## When gemini's transcript schema drifts

`make schema-canary-gemini` ran a live short reply through gemini then re-parsed the fresh JSONL, and
the reader failed. The reader accepts two line shapes (Gemini-API `role`+`parts[].text` and the
CLI-internal `type`+`message`); extend `jsonlLine` / `normalizeRole` / `extractText`, add a trimmed
fixture round-trip test, and re-run the canary.

## Recording gotchas

- **Auth on first launch** — codex/claude prompt for interactive login on a fresh machine; the
  scripted recorder can't survive it. Authenticate by hand once, then re-record.
- **Slow API** — raise `--max-duration` (default 5m) for long scenarios.
- **Wrong `wait_for`** — the idle-timeout fallback lets the script proceed without matching, capturing
  a screen with no marker. Inspect `bytes.raw` and tighten the script's `wait_for` regex.
- **Quiet corruption** — a truncated/auth-screen recording that still satisfies the regex is wrong
  without failing. After a successful `rebake-corpus-all`, eyeball a sample
  (`screenbench --corpus test/corpus --format markdown | less`).

## Load-bearing files

`Makefile` (orchestrator) · `pkg/versions/{versions.json,versions.go}` (pins + read API) ·
`internal/cmd/upstream-version-sentry/main.go` (npm check) ·
`internal/screenbench/cmd/screenbench-record/` (scripted recorder) · `test/scripts/<harness>/*.json`
(canonical scenarios) · `test/corpus/<harness>/<scenario>/{bytes.raw,meta.json,expected.txt}` (the
recorded [corpus](testing/corpus.md)) · `pkg/turns/harness/<name>/<name>.go` (marker regexes; package
comment cites the last-verified version).
