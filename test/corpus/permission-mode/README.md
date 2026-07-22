# permission-mode conformance corpus

Captured footer screens for the permission-posture signal —
`turns.PermissionModeDetector.PermissionMode(snap)`, implemented by the
`claude-code` adapter (`pkg/turns/harness/claudecode/permmode.go`) and the
`codex` adapter (`pkg/turns/harness/codex/permmode.go`).

`harness-wrapper` is **canonical** for shared corpora
(`crossrepo/meta-harness/APPLY.md`): screens are captured **here** and mirrored
**out** to meta-harness with `scripts/sync-permission-mode-corpus.sh --to DIR`.
`MANIFEST.sha256` pins the tree, and the two repos are in sync **iff** their
committed manifests are byte-equal — a `--check` CI step runs on both sides.
This is not "the Go side reusing the TS side's bytes"; the direction is
harness-wrapper → meta-harness.

The offline test (`pkg/chat/permission_mode_corpus_test.go`) asserts:

1. `PermissionMode(screen.txt) == meta.mode` for every case, read through the
   real adapter (the per-harness parsers are unexported, so the test
   type-asserts the adapter to `turns.PermissionModeDetector` — the same shape
   as `pkg/chat/auth_corpus_test.go` driving fixtures through `authRequired`),
   and
2. the recomputed corpus hash matches `MANIFEST.sha256`.

## Layout

    <harness>/<mode>/
      screen.txt   verbatim pkg/screen render of the captured footer screen
      bytes.raw    the raw capture, when the rig produced one (optional)
      meta.json    { harness, binary_version, recorded_at, cols, rows, mode, notes }

`screen.txt`, **not** `expected.txt`: the screenbench tree
(`test/corpus/claude-code/*/expected.txt`) uses that name for a *normalized
emulator snapshot*, and these are not that.

These are not new conversation scenarios, so they are deliberately **not** in
`Makefile`'s `SCENARIOS` list (the six-scenario `rebake-corpus-all` loop kept in
sync with `test/scripts/<harness>/*.json`).

## What this corpus does NOT guard

**Screen-render drift.** How claude's bytes render through `pkg/screen` is
already pinned by the screenbench corpus (`test/corpus/claude-code/*`) and the
models corpus. This corpus pins exactly one thing: the footer → posture
mapping. Stating that explicitly so nobody adds a redundant render assertion
here.

## Signals already pinned elsewhere (deliberately not re-captured)

| posture | where it already lives |
| --- | --- |
| claude `auto` | `test/corpus/claude-code/{multi-turn,tool-call,interrupted-mid-reply}/bytes.raw`, asserted by rendering through `pkg/screen` (`pkg/screen/screen.go:98`) |
| claude `manual`, suffix-less | `test/corpus/auth/claude-code/not-logged-in-churned/screen.txt:15` and `not-logged-in-brewed/screen.txt:18` — both `  ⏸ manual mode on · ← for agents` |
| codex `default` | every existing codex scenario; codex's default paints no collaboration marker at all |

Those fixtures are referenced, not copied — duplicating them here would create a
second place to re-record the same bytes.

## Live footer strings (claude-code 2.1.217)

```
⏵⏵ auto mode on (shift+tab to cycle) · ← for agents
⏸ manual mode on · ← for agents
⏵⏵ accept edits on (shift+tab to cycle) · ← for agents
⏸ plan mode on (shift+tab to cycle) · ← for agents
⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
```

## Open capture list

<!-- OPEN-CAPTURES -->

The following still need a **real** capture from a live rig. No fixture here is
ever invented — inventing a screen is precisely the failure mode the auth corpus
README calls out (`test/corpus/auth/README.md`), and a conformance test that
skips over an empty-but-well-formed tree is strictly better than one that passes
against fabricated bytes.

- claude `plan`
- claude `accept edits` (rung `ask`)
- claude `bypass permissions`
- a `--permission-mode dontAsk` probe capture (see the `dontAsk` contract below)
- one codex `Plan mode` collaboration capture

### Capture questions to answer while the rig is up

These belong in the capture notes AND in the doc comments on `pkg/chat`'s
`shiftTabForHarness` and `SetPermissionMode`, which are the other homes for
them.

1. **Is the 5th rung (`bypass`) reachable on a bypass-enabled launch _before_
   the acceptance dialog is accepted?**
   _Answer: UNRECORDED — needs a live rig._
2. **Does cycling _into_ bypass mid-session re-raise a confirmation?**
   _Answer: UNRECORDED — needs a live rig._
3. **What is the Shift+Tab ring's membership and order at 2.1.217, with and
   without `--dangerously-skip-permissions`?** Nothing in this repo pins it
   today.
   _Answer: UNRECORDED — needs a live rig._

## The `dontAsk` contract

`dontAsk` is accepted at launch as a claude-native passthrough
(`isSupportedPermissionMode`, `pkg/wrapper/wrapper.go:557`) but is **not** a
canonical rung, and it paints no distinct footer word in the observed set.
Whichever footer word a `--permission-mode dontAsk` session paints is the rung
the parser reports for it.

_Observed mapping: UNRECORDED — needs a `--permission-mode dontAsk` capture._

If a capture ever shows `dontAsk` painting a **genuinely distinct** footer, that
is a parser change, not a corpus change: the closed alternation in
`permissionModeFromFooter` would need a sixth branch, and that follow-up belongs
to the parser ticket. Recording the answer here either way is the point — the
contract is written down, not left undefined.

## Corpus-version tolerance

Nothing enforces a version match between a capture and the current pin
(`internal/screenbench/scenario/scenario_test.go:35` asserts only that
`meta.json`'s `binary_version` is non-empty), so the rule is written down here:

> **An existing capture stays valid while the signal it pins still renders
> identically at the pinned version. Re-recording is required only when a signal
> changes.**

The 2.1.217 / 0.144.5 pin bump therefore deliberately leaves four corpora at
older versions: screenbench claude 2.1.185, auth 2.1.215, models 2.1.204, codex
approval 0.144.4. The "every capture's `meta.json` records `binary_version`
matching the new pin" rule is scoped to **the captures this corpus makes**, not
applied retroactively to the tree.

## `scripts/sync-versions.sh --check` is expected red here

`pkg/versions/versions.json` pins `claude-code 2.1.217` / `codex 0.144.5`, and
`pkg/versions/testdata/meta-harness-versions.json` is hand-edited to match so
the data-driven `pkg/versions/parity_test.go` stays green.
`scripts/sync-versions.sh` is a **dev-machine** tool that reads a sibling
meta-harness checkout (`META_HARNESS_DIR`) and is never invoked by `make test`;
its `--check` is **expected red on a dev machine** until the paired meta-harness
pin bump lands. Per `parity_test.go`'s own doc comment ("Coverage is
one-directional by design… If meta-harness moves its pins first, nothing here
notices"), the vendored snapshot deliberately front-runs the sibling. A red
`--check` in that window is not drift.
