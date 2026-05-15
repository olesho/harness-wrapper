# Upgrade Playbook

The drift-detection pipeline (see `Makefile`) is built around the
assumption that **a developer responds when an upstream CLI changes**.
This document is the step-by-step for that response.

The pipeline runs entirely on the developer's machine. There is no CI
cron; nothing detects drift unattended. Use this playbook when any of
the following happen:

1. `make check-versions` reports `drift` for one or more harnesses.
2. `make rebake-corpus-all` exits non-zero because adapter tests fail
   against the freshly-recorded corpus.
3. `make schema-canary-gemini` reports parse failures from the gemini
   transcript reader against a fresh on-disk JSONL.
4. A user files a "the wrapper doesn't detect turn completion anymore"
   bug after a known upstream release.

Cases 2-4 mean an upstream marker or schema actually shifted. Case 1
is the cheapest signal — a new version exists but might still be
backwards-compatible; you confirm by re-baking the corpus and seeing
if the adapter tests pass.

## Suggested cadence

| Check | Cadence | Cost |
|---|---|---|
| `make check-versions` | weekly | free, ~2s |
| `make schema-canary-gemini` | every release-prep | free (gemini local oauth), ~30s + 1 short prompt |
| `make rebake-corpus-all` | monthly OR on a confirmed `drift` from check-versions | ~$0.50 API spend across codex/claude; ~5-10 min |

## When `make check-versions` shows drift

Output looks like:

```
| harness | package | pinned | latest | status |
|---|---|---|---|---|
| claude-code | `@anthropic-ai/claude-code` | 2.1.141 | 2.1.142 | drift |
| codex | `@openai/codex` | 0.130.0 | 0.130.0 | match |
| gemini | `@google/gemini-cli` | — | 0.42.0 | unpinned |

⚠ drift detected — see docs/upgrade-playbook.md when ready
```

A new upstream release exists; the corpus has not yet been verified
against it. Steps:

1. Install the new version locally: e.g. `npm i -g @anthropic-ai/claude-code@2.1.142`.
2. Re-bake the affected harness's canonical corpus and the adapter
   regression in one shot:
   ```sh
   make rebake-corpus-all  # whole corpus
   # or, scoped:
   for s in short-reply long-markdown code-block interrupted-mid-reply tool-call multi-turn; do
       make rebake-corpus HARNESS=claude SCENARIO=$s
   done
   go test -race ./pkg/turns/harness/claudecode/...
   ```
3. If adapter tests stay green: the upstream release is
   backwards-compatible. Just bump the pin (next section).
4. If adapter tests fail: a marker shifted. Continue to "When marker
   drift is real" below.

In both cases, finish with:

5. Update `versions.json`: set `pinned` to the new version and
   `verified_at` to today's date (YYYY-MM-DD).
6. Refresh the package-comment "Verified against …" line in the
   adapter file (`pkg/turns/harness/<name>/<name>.go`).
7. Commit `test/corpus/<harness>/**`, `versions.json`, and any code
   changes locally.

## When marker drift is real

The `rebake-corpus-all` step has exited non-zero. Diagnostic flow:

1. **Identify the failing scenario.** The adapter tests print which
   scenario tripped (e.g. `claudecode_test.go: multi-turn …`).
2. **Diff fresh vs. old bytes.raw.** Use `screenbench` to render both
   to text:
   ```sh
   go run ./internal/screenbench/cmd/screenbench --corpus test/corpus --scenario multi-turn --format markdown
   ```
   Compare against the previous recording (recover via `git diff
   test/corpus/<harness>/<scenario>/bytes.raw`). The visible delta is
   the marker shift — typically a footer line or a status verb that
   changed format.
3. **Pinpoint the regex.** The adapters keep their end-of-turn match
   at a single named regex:
   - `pkg/turns/harness/codex/codex.go:36` — `tokenUsageRE`
   - `pkg/turns/harness/claudecode/claudecode.go:39` — `thinkingRE`
   - `pkg/turns/harness/gemini/gemini.go` — (placeholder; not yet
     anchored on a real marker as of writing)
4. **Update the regex.** Anchor it to the new marker shape. Keep the
   line-anchor discipline (`(?m)^...$` for screen-line markers) so
   the adversarial corpus tests under
   `test/corpus/<harness>/adversarial/` keep passing.
5. **Re-run targeted tests.**
   ```sh
   go test ./pkg/turns/harness/<harness>/...
   ```
   Both the canonical and adversarial cases must pass before moving on.
6. **Refresh the adapter's package-comment "Verified against …" line.**
   Document the new upstream version.
7. **Bump `versions.json`.** Set `pinned` and `verified_at` for the
   updated harness.
8. **Commit locally.**
   ```sh
   git add test/corpus/<harness>/** versions.json pkg/turns/harness/<harness>/<harness>.go
   git commit -m "<harness>: bump pinned to X.Y.Z, refresh corpus"
   ```

## When gemini's transcript schema drifts

`make schema-canary-gemini` ran the canonical short-reply through the
live gemini CLI and then re-ran the transcript reader's real-corpus
smoke test, which failed.

1. **Look at the freshly-written JSONL.** Locate it under
   `~/.gemini/tmp/<project>/chats/session-*.jsonl`. The most recent
   file is the one the canary produced.
2. **Compare against the reader's expectations.** The reader at
   `pkg/transcript/gemini/gemini.go` accepts two line shapes today:
   the Gemini-API style (`role` + `parts[].text`) and the CLI-internal
   `type` + `message` style. If neither matches, extend
   `jsonlLine` / `normalizeRole` / `extractText`.
3. **Add a fixture round-trip test** in
   `pkg/transcript/gemini/gemini_test.go` using a trimmed version of
   the failing JSONL line so the new shape stays covered hermetically.
4. **Verify.**
   ```sh
   go test ./pkg/transcript/gemini/...
   make schema-canary-gemini   # the live re-record must pass again
   ```
5. **Commit locally.**

## When `--max-duration` trips the recording

The recorder kills the harness after `--max-duration` (default 5 min)
even if the script hasn't finished. If `bytes.raw` ends mid-stream the
adapter tests will fail because no end-of-turn marker is present.

Causes seen so far:

- **Auth flow on first launch.** Codex/Claude prompt for `gh
  auth`-style interactive login on a fresh machine; the scripted
  recorder can't survive that. Run the CLI manually once to complete
  auth, then re-run.
- **Slow API response.** Long prompts under a slow model can outrun
  the default. Re-run the failing scenario with a longer cap:
  ```sh
  go run ./internal/screenbench/cmd/screenbench-record \
      --harness codex --bin "$(which codex)" \
      --out test/corpus/codex/long-markdown \
      --script test/scripts/codex/long-markdown.json \
      --auto-version \
      --max-duration 10m
  ```
- **Wrong `wait_for` regex.** Idle-timeout fallback (default 3s) lets
  the script proceed without matching, so the recording captures
  whatever was on screen — usually missing the marker. Inspect
  `bytes.raw` to see what the harness actually rendered, then tighten
  the script's `wait_for` regex.

## When a recording is wrong but adapter tests still pass

This is a quiet failure mode: the new corpus is corrupted (truncated,
contains an auth screen, etc.) but the adapter still finds *some*
marker that satisfies the regex. The corpus is now wrong without you
knowing.

Mitigation: every time `rebake-corpus-all` succeeds, scan a sample of
the fresh corpus by eye:

```sh
go run ./internal/screenbench/cmd/screenbench --corpus test/corpus \
    --format markdown | less
```

Look for obviously-truncated paragraphs, login prompts, or empty
sections. If found, re-record the affected scenario manually
(interactive mode) and copy that recording over.

## Reference: which files are load-bearing for the pipeline

- `Makefile` — orchestrator; `make` targets are documented in `make help`.
- `versions.json` — pinned upstream-version source of truth.
- `pkg/versions/versions.go` — read API for the pin file.
- `internal/cmd/upstream-version-sentry/main.go` — npm registry
  drift check.
- `internal/screenbench/cmd/screenbench-record/{main,script}.go` —
  scripted recording driver.
- `test/scripts/{codex,claude,gemini}/*.json` — the canonical
  scenario scripts replayed by `rebake-corpus`.
- `test/corpus/<harness>/<scenario>/{bytes.raw,meta.json,expected.txt}`
  — the recorded corpus, fed to adapter regression tests.
- `test/corpus/<harness>/adversarial/<scenario>/…` — negative
  recordings; locked in by `TestAdapter_AdversarialNoFire`.
- Adapter files at `pkg/turns/harness/<name>/<name>.go` — TUI marker
  regexes live here; their package comments cite the
  last-verified upstream version.
