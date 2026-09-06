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
  "codex":       {"package": "@openai/codex",              "binary": "codex",    "pinned": "0.144.5", "verified_at": "2026-07-22"},

> Both files carry claude-code `2.1.263` as of 2026-09-06, verified live against the installed
> 2.1.263 binary via `TestRunTurn_RealClaudeDogfood{,KeepAlive}` (end-of-turn detection, reply
> extraction and the multi-turn keep-alive path only — the `interruptMarker`, tool-call and
> permission-mode surfaces still rest on recorded corpora, and the blocking startup dialogs are
> seeded away by the release-check harness and stay unverified). ALL FOUR claude scenarios —
> `settled-after-turn`, `multi-turn`, `tool-call` and `interrupted-mid-reply` — are re-baked at
> `2.1.251`, so the corpus trails the pin by design rather than by neglect. meta-harness's own pin
> file is still at `2.1.218` and has to follow — until it does, `scripts/sync-versions.sh --check`
> against a sibling checkout reports drift by design, the snapshot is a parity *target* rather than
> a mirror of what meta-harness ships today, and the no-args mode would drag this repo's pin
> *backwards* by 43 releases.
  "opencode":    {"package": "opencode-ai",                "binary": "opencode", "pinned": "",        "verified_at": ""},
  "pi":          {"package": "@earendil-works/pi-coding-agent", "binary": "pi",  "pinned": "0.76.0",  "verified_at": "2026-06-27"}
}
```

An empty `pinned` means "no corpus captured yet".

**Pin/corpus skew.** Pins may legitimately lead the corpus's `binary_version` when they are adopted
for cross-repo parity with meta-harness (the TS port) rather than from a local re-bake — e.g. codex
self-updates to latest on launch, so a targeted re-bake at the exact parity version isn't possible.
The vendored snapshot `pkg/versions/testdata/meta-harness-versions.json` mirrors meta-harness's pin
file; the hermetic parity test in `pkg/versions/parity_test.go` keeps this repo's pins semantically
equal to it, and `scripts/sync-versions.sh` (no args: refresh the snapshot from a sibling checkout;
`--check`: format-insensitive drift check) keeps the snapshot itself current.

> **Bumping a pin here bumps the snapshot too.** The parity test is hermetic, so a pin raised in
> `pkg/versions/versions.json` without the matching edit to the vendored snapshot fails `make test`.
> Both files carry claude-code `2.1.263` as of 2026-09-06, verified live against the installed
> 2.1.263 binary via `TestRunTurn_RealClaudeDogfood{,KeepAlive}` (end-of-turn detection, reply
> extraction and the multi-turn keep-alive path only — the `interruptMarker` and tool-call surfaces
> still rest on corpora recorded at 2.1.251, the permission-mode footers remain anchored at 2.1.217,
> and the blocking startup dialogs are seeded away by the release-check harness and stay
> unverified). meta-harness's own pin file is still at `2.1.218` (verified 2026-07-23) and has to
> follow — until it does, `scripts/sync-versions.sh --check` against a sibling checkout reports
> drift by design, and the no-args mode would drag this repo's pin *backwards* by 43 releases.

The read API:

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
make rebake-corpus HARNESS=<name> SCENARIO=<name>   # refresh one scenario (paid for codex/claude)
make rebake-corpus-all     # walks the Makefile cross-product, then runs adapter tests
```

The canonical lists live in the `Makefile`:
`SCENARIOS = short-reply long-markdown code-block interrupted-mid-reply tool-call multi-turn`;
`HARNESSES = codex claude`.

> **The cross-product is codex-shaped; `rebake-corpus-all`'s "12 live recordings (6 × 2)" banner is
> wrong for claude.** Under `test/corpus/claude-code/` the recorded scenarios are `adversarial`,
> `interrupted-mid-reply`, `multi-turn`, `settled-after-turn` and `tool-call`. Of those,
> `adversarial/thinking-line-mid-reply` is a hand-authored 2.1.141 fixture with no
> `test/scripts/claude/adversarial.json`, so it is **not** rebakeable. **A claude rebake is 4
> scenarios, not 6:** `interrupted-mid-reply multi-turn settled-after-turn tool-call`.
> `short-reply`, `long-markdown` and `code-block` have scripts but **no claude corpus directory** —
> rebaking them only creates new corpora that nothing replays.

`make check-versions` runs `cmd/check-versions`, which compares each pin against
`https://registry.npmjs.org/<package>/latest`. Exit codes: **0** all pins current, **1** drift
detected, **2** registry unreachable — but read the **verdict line** the command prints, not the
status a wrapper saw. `go run` collapses a non-zero child status to 1, so anything invoking this
through `go run` (as the target itself once did) cannot tell an outage from real drift; the program
prints `✓ all pins match latest` / `⚠ drift detected …` / `✗ could not query the npm registry`
itself for exactly that reason. A `✗` means **no signal** — it is not evidence the pins are fine.

The table it prints has six columns — `harness | package | pinned | latest | status | detail`. The
`detail` column is populated only on `status: error` rows and carries the probe failure, flattened to
one line and truncated to 120 characters; `--format=json` carries the same text untruncated in each
row's `error` field.

Those exit codes are **contractual**, and the target is safe to invoke from a script: the recipe
builds the binary into a tmpdir and runs it rather than using `go run`, so `make check-versions`
exits **0** for current-or-drift (drift is the normal state at every upstream release and must not
fail the target) and **2** when the sentry could not answer at all. Any other status is passed
through unchanged with a `✗ check-versions failed unexpectedly` line, so an unrecognised failure is
never mistaken for a clean run. Operator tooling that watches for new releases (the release-check
cron some hosts run, which lives outside this repository) depends on exactly that split. The recipe
also accepts `CHECK_VERSIONS_ARGS`, which is passed straight to the binary; it exists **for tests
only** — `cmd/check-versions/makefile_exit_test.go` uses it to point the target at a local fake
registry and replay current pins, drift, outages and a mix of the two — and should be left empty in
operator and CI use.

## When the table shows `error`

An `error` row means **no comparison happened**. `latest` is `—` because the registry was never read,
so the row is *not* drift and says nothing at all about whether that pin is current — read the
`detail` column for the cause. Only a `drift` row is drift.

This distinction is not academic: a run in which all four rows read `error` was filed as a P1
pin-drift bug, because the target reported `⚠ drift detected` and exited 0 (`go run` had collapsed
the command's exit 2 to 1) and the table dropped the cause on the floor. Both halves are fixed — the
verdict now reads `✗ could not query the npm registry` and the target exits 2 — but the reflex to
check is still the `status` column, not the verdict alone.

The signature seen on macOS:

```
tls: failed to verify certificate: x509: “certificate” certificate is not trusted (OSStatus -26276)
```

That is a **host toolchain** problem, not a repo one, and it has three properties worth knowing before
you spend an hour on it:

- Go on darwin uses the platform (Security.framework) verifier, so **`SSL_CERT_FILE` is ignored** —
  pointing it at a CA bundle changes nothing.
- **`curl` and `npm` succeeding does not clear Go.** They use their own bundled roots and will fetch
  `registry.npmjs.org` happily while every Go `http.Get` on the same machine fails.
- It reproduces with a stock ten-line `http.Get`, so it is not something `cmd/check-versions` can fix.

Until the host trust store is repaired, `make check-versions` cannot produce a verdict on this
machine; verify a pin by hand (`npm view <package> version`) and treat the `✗` as what it says.

## When `check-versions` shows drift

A new release exists; the corpus hasn't been verified against it yet — it may still be
backwards-compatible.

1. Install it locally (e.g. `npm i -g @anthropic-ai/claude-code@<ver>`).
2. Re-bake the affected harness and run its adapter regression:
   ```bash
   for s in interrupted-mid-reply multi-turn settled-after-turn tool-call; do
       make rebake-corpus HARNESS=claude SCENARIO=$s
   done
   go test -race ./pkg/turns/harness/claudecode/...
   ```
3. **Green** → it's backwards-compatible. **Red** → a marker shifted (next section).
4. Re-verify the **shapes the six canonical scenarios do not cover** (below).
5. Either way, finish by setting `pinned`/`verified_at` in `versions.json`, refreshing the adapter's
   "Verified against …" package comment, and committing `test/corpus/<harness>/**` + the version bump.

### Shapes the canonical scenarios miss

The six scenarios all record a harness that is *already running in a trusted directory*, so a bump can
shift a startup screen without a single corpus test going red.

- **claude's folder-trust dialog** (*"Do you trust the files in this folder?"* / *"Yes, I trust this
  folder"*). This is the one that fails **invisibly**: nothing is reported as a dialog, the agent just
  sits at a screen it does not recognise until the watchdog kills it. Two things move between releases
  — the dialog's wording, and **which option is highlighted by default** (2.1.247 defaulted to *Yes, I
  trust this folder*; 2.1.251 defaults to *No, exit*, so a script that answers with a bare Enter now
  quits claude at startup and bakes a dead recording). So no script hard-codes the answer: every
  claude script starts with `{"answer_dialog": "Yes, I trust this folder"}`. If the dialog paints, the
  recorder answers it with keys the production parser derives from the rendered screen, writing the
  arrows first and Enter only once the highlight sits on the target row; in a trusted directory the
  step finds no dialog and does nothing. A dialog with no such option, a highlight that never lands,
  or a dialog that will not clear stops the recording instead of baking the wrong screen.
- To record a scenario the way a fresh checkout starts it, point `screenbench-record --workdir` at a
  freshly `git init`-ed directory claude has never trusted — the Makefile otherwise runs the recorder
  from inside this repo, which claude already trusts:
  ```bash
  d=$(mktemp -d) && git -C "$d" init -q
  make rebake-corpus HARNESS=claude SCENARIO=settled-after-turn WORKDIR="$d"
  ```
  The Makefile records the directory in the scenario's `meta.json` notes so the rebake is reproducible.
  The two trust-dialog corpora (`trust-dialog-unnumbered`, `trust-dialog-confirmed`) are not rebaked
  this way: a recording of the dialog itself must hold its frames before any answer, so they were
  captured under tmux against a real claude — see their `meta.json` notes.

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

## When a transcript schema drifts

If a corpus canary runs a live short reply through a harness, re-parses the fresh JSONL, and the reader
fails, the harness has changed its on-disk line shape. Where a reader must accept more than one shape
(e.g. an API-style `role`+`parts[].text` line versus a CLI-internal `type`+`message` line), extend its
`jsonlLine` / `normalizeRole` / `extractText` helpers, add a trimmed fixture round-trip test, and
re-run the canary.

## Recording gotchas

- **Auth on first launch** — codex/claude prompt for interactive login on a fresh machine; the
  scripted recorder can't survive it. Authenticate by hand once, then re-record.
- **codex 0.142+ has no on-screen end-of-turn marker** — it dropped the `Token usage:` footer (and the
  `codex resume <uuid>` hint), so `wait_for "Token usage:"` is dead and completion is purely the
  recorder's idle-timeout. Record codex with `--idle-timeout 8s` (longer for `long-markdown`/`code-block`);
  the scripts now `wait_for "esc to interrupt"` to confirm the turn started, then let idle close it. The
  adapter regression `TestCodexAdapter_NoFireOnRealRecording` asserts OnScreen stays silent on these
  recordings (idle-driven completion), not that it fires.
- **codex auto-updates on launch** — a stale codex silently runs `npm install -g @openai/codex` on first
  launch (e.g. 0.142.0→0.142.2), polluting the first recording and moving the pin target. There is no
  config key to disable it; instead update to latest by hand first (`codex --version` twice), then bake.
- **codex environment noise** — the user's `~/.codex/config.toml` (MCP servers like `codex_apps`, model
  NUX, usage notices) bleeds into recordings. Bake with an isolated `CODEX_HOME=<tmp>` holding a copied
  `auth.json` and an empty `config.toml`, plus `-- -a never -s read-only` so `tool-call` runs its command
  without an approval prompt. The isolated `CODEX_HOME/sessions` also makes `expected.txt` extraction
  unambiguous.
- **Full-screen TUIs need a sized PTY** — the recorder now calls `Session.Resize(--cols,--rows)` after
  start (scripted mode has no controlling TTY to inherit a size from). Without it a ratatui TUI (codex
  0.142) renders into a ~0×0 PTY and replays blank.
- **Directory trust screens** — claude paints its folder-trust dialog only in a directory it has not
  trusted before, and the recorder inherits its own CWD (inside this repo) unless told otherwise. Use
  `--workdir` / `make rebake-corpus … WORKDIR=<dir>` to record it; see
  [Shapes the canonical scenarios miss](#shapes-the-canonical-scenarios-miss).
- **Slow API** — raise `--max-duration` (default 5m) for long scenarios. Prefer the script's own
  top-level `"idle_timeout"` / `"max_duration"` fields over the flags: the budget then travels with
  the scenario, is versioned in the same file, and `rebake-corpus-all` — which cannot pass
  per-scenario flags — picks it up. Both are Go duration strings and fail at load if malformed. A
  tool-using turn needs a far longer idle tolerance than a "what is 2+2?" turn; `tool-call` asks for
  `20s`.
- **`wait_for` matches the RAW PTY BYTE STREAM, not the rendered screen.** The buffer it searches
  holds SGR colour changes and cursor moves interleaved with the text, so an anchor must be short and
  contiguous *within one styled run* — a pattern spanning a colour change can fail against a screen
  that visibly shows it. This is why the old `wait_for "> "` composer anchor was doubly dead: the
  prompt renders as U+276F `❯`, and the plain `>` only ever matched incidentally.
- **Wrong `wait_for`** — the idle-timeout fallback lets the script proceed without matching, capturing
  a screen with no marker. Inspect `bytes.raw` and tighten the script's `wait_for` regex. A green bake
  is *not* proof the anchor matched: `grep -a` the fresh `bytes.raw` for the marker before trusting it.
- **`{"send": "…\n"}` types AND submits** — the trailing newline is replaced by the harness's
  enhanced-keyboard Enter, written as a second PTY write after a bounded wait for the composer to echo
  the text (mirroring `pkg/chat/submit.go`). Without that wait Claude Code reads the burst as a paste
  and swallows the submit key, leaving the prompt sitting unsent in the composer.
- **Interrupting claude: Esc, not Ctrl-C** — on 2.1.251 Esc (`0x1b`) interrupts a streaming reply and
  paints `⎿  Interrupted · What should Claude do instead?`, while Ctrl-C (`0x03`) stops the turn,
  paints nothing and restores the prompt into the composer. An `interrupt` step therefore takes an
  `"interrupt_key"` of `"ctrl-c"` (the default, which the codex corpus was baked with) or `"esc"`.
  Timing matters too: an Esc that lands before the first token is a cancel and is likewise painted as
  nothing, so wait for the reply to start streaming (the `⏺` bullet) before pressing it.
- **Quiet corruption** — a truncated/auth-screen recording that still satisfies the regex is wrong
  without failing. After a successful `rebake-corpus-all`, eyeball a sample
  (`screenbench --corpus test/corpus --format markdown | less`).

## Load-bearing files

`Makefile` (orchestrator) · `pkg/versions/{versions.json,versions.go}` (pins + read API) ·
`cmd/check-versions/main.go` (npm check) ·
`internal/screenbench/cmd/screenbench-record/` (scripted recorder) · `test/scripts/<harness>/*.json`
(canonical scenarios) · `test/corpus/<harness>/<scenario>/{bytes.raw,meta.json,expected.txt}` (the
recorded [corpus](testing/corpus.md)) · `pkg/turns/harness/<name>/<name>.go` (marker regexes; package
comment cites the last-verified version).
