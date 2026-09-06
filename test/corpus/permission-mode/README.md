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
      bytes.raw    the raw capture (see "How the captures were made")
      meta.json    { harness, binary_version, recorded_at, cols, rows, mode, notes }

`screen.txt`, **not** `expected.txt`: the screenbench tree
(`test/corpus/claude-code/*/expected.txt`) uses that name for a *normalized
emulator snapshot*, and these are not that.

These are not new conversation scenarios, so they are deliberately **not** in
`Makefile`'s `SCENARIOS` list (the six-scenario `rebake-corpus-all` loop kept in
sync with `test/scripts/<harness>/*.json`).

## Cases

| case | cols | expected `mode` | status |
| --- | --- | --- | --- |
| `claude-code/plan` | 120 | `plan` | enforcing |
| `claude-code/ask` | 120 | `ask` | enforcing (launched `--permission-mode acceptEdits`) |
| `claude-code/bypass` | 120 | `bypass` | enforcing |
| `claude-code/dont-ask` | 120 | `manual` | enforcing (launched `--permission-mode dontAsk`; see the `dontAsk` contract below) |
| `codex/plan` | 200 | `plan` | enforcing (collaboration axis, not a rung) |
| `codex/plan-narrow-gutter` | 120 | `plan` | **pending parser** — see the gutter finding below |

### `meta.pending_parser`

A capture whose observed truth the **shipped parser does not yet report** keeps
its real `mode` and carries a `pending_parser` string saying why. The
conformance test inverts such a case: it asserts the parser still *disagrees*,
and **fails** if the parser starts agreeing — so the marker cannot outlive the
fix it names. Removing the field is how the case is promoted to enforcing.
This exists so a real capture of a real gap is neither fabricated into a
passing expectation nor dropped on the floor.

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

## How the captures were made

Every screen here is a **real capture**, never synthetic. Recorded on
2026-07-22 with the repo's own rig — `internal/screenbench/cmd/screenbench-record`
driving the live binaries (claude-code **2.1.217**, codex **0.144.5**, the
versions this change pins) under a scripted `wait_for` / `send` / `sleep`
sequence, at 120×40 unless the case table says otherwise. No prompt was ever
submitted: each session launches, paints its footer, and is torn down, so the
captures cost no API tokens.

`bytes.raw` is the capture **truncated at the end of the last frame in which the
marker is painted** — for claude at the alt-screen exit (`ESC[?1049l`), for codex
at the synchronized-output end (`ESC[?2026l`) following the marker. The
truncation is what makes the fixture useful: a harness's teardown clears the
screen, so the untruncated stream renders to a blank snapshot. Rendering the
committed `bytes.raw` through `pkg/screen` at the case's `cols`×`rows` reproduces
`screen.txt` exactly; that is how each `screen.txt` was produced.

Two artefacts of the recording environment are visible in the claude screens and
are deliberately kept rather than edited out (an edited capture is a synthetic
capture): a `⚠ Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION
marker` warning row, because the rig ran nested inside another Claude Code
session, and a scratch-directory cwd row.

## Live footer strings (claude-code 2.1.217, observed)

```
⏵⏵ auto mode on (shift+tab to cycle) · ← for agents
⏸ manual mode on · ← for agents
⏵⏵ accept edits on (shift+tab to cycle) · ← for agents
⏸ plan mode on (shift+tab to cycle) · ← for agents
⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
⏵⏵ don't ask on (shift+tab to cycle)                 ← SIXTH word, FIFTH rung; see below
```

## Capture questions — answered live at 2.1.217

Answered on 2026-07-22 against claude-code 2.1.217 on macOS, by scripted
shift+tab probes (`ESC[Z`) with the footer read out of the raw stream after each
press. `pkg/chat`'s `shiftTabForHarness` and `SetPermissionMode` doc comments are
the other homes for these answers.

**1. Is the 5th rung reachable on a bypass-enabled launch _before_ the
acceptance dialog is accepted?**
Not answered in the strong form, and the caveat matters. On this rig both
`--permission-mode bypassPermissions` and `--dangerously-skip-permissions`
launched **straight into a painted `⏵⏵ bypass permissions on` footer with no
acceptance dialog at all** — but the machine had accepted bypass at some earlier
point, and that acceptance persists in the user's claude config. So what is
pinned is: *on an already-accepted machine the 5th rung is live and painted at
launch, with no dialog.* The never-accepted case needs a rig with a pristine
claude config (which also lands you on the login/onboarding wall, so it needs a
logged-in-but-never-accepted profile to be conclusive). Still open.

**2. Does cycling _into_ bypass mid-session re-raise a confirmation?**
**No — and on a default launch you cannot get there at all.** The shift+tab ring
on a default launch never visits bypass (see question 3), so there is no
mid-session transition into it to confirm. On a bypass-enabled launch, cycling
all the way around from `⏸ plan mode on` back to `⏵⏵ bypass permissions on`
repaints the bypass footer directly, with **no** confirmation dialog.

**3. What is the Shift+Tab ring's membership and order at 2.1.217, with and
without `--dangerously-skip-permissions`?**
Two different rings, both observed over multiple full laps:

```
default launch (4 rungs):
  auto mode → manual mode → accept edits → plan mode → (back to auto)

--dangerously-skip-permissions, and --permission-mode bypassPermissions (5 rungs):
  bypass permissions → auto mode → manual mode → accept edits → plan mode → (back to bypass)
```

Bypass is therefore **launch-gated, not ring-gated**: the bypass rung is spliced
into the ring only when the session was launched bypass-enabled, and the two
bypass-enabling flags produce an identical ring. `dontAsk` is in **neither**
ring — it is reachable only via the launch flag.

## The `dontAsk` contract — a SIXTH footer word on the EXISTING `manual` rung

**Finding (claude-code 2.1.217): `--permission-mode dontAsk` paints its own
footer word, `⏵⏵ don't ask on (shift+tab to cycle)`.** The capture is at
`claude-code/dont-ask/`.

**RESOLVED.** The parser's closed alternation carries a **sixth row** for it, and
that row reports the **existing `manual` rung** — no sixth canonical rung was
added. The evidence, from claude's own bundle:

- its permissiveness rank table reads
  `{plan:0, bubble:1, default:1, dontAsk:1, acceptEdits:2, auto:3, bypassPermissions:4}`
  — `dontAsk` and `default` (claude's spelling of `manual`) share rank 1.
  `wrapper.PermissionRungs()` is a **strict total order** consumed by
  `rungIndex`/`MorePermissive`; a genuine sixth rung would have to be a TIE with
  manual, which that model cannot express. `dontAsk` is a second spelling of an
  existing rung, exactly as `acceptEdits` is a second spelling of `ask`.
- the SDK schema string reads
  `'dontAsk' - Don't prompt for permissions, deny if not pre-approved.` — strictly
  MORE restrictive than manual, so reporting `manual` can never UNDER-report
  permissiveness.
- the bundle's ring function returns `["plan","default","acceptEdits"]` (plus
  auto/bypass when enabled), corroborating question 3 below: `dontAsk` is absent
  from the ring, so mapping it to manual cannot corrupt the cycle driver's ring
  model.

`dontAsk` remains accepted at launch by `isSupportedPermissionMode`
(`pkg/wrapper/wrapper.go`) and remains **absent** from `wrapper.PermissionRungs()`
— `rungIndex("dontAsk")` still returns -1, so `MorePermissive` keeps failing
closed on the native spelling.

The closed-alternation property is preserved: the sixth row is one more
explicitly enumerated word, not a generic `<words> on` capture, so a SEVENTH,
future mode still degrades to `("", false)`.

Observed mapping, for the record:

| launch flag | footer word painted | parser today |
| --- | --- | --- |
| `--permission-mode dontAsk` | `⏵⏵ don't ask on` | `("manual", true)` |

## Codex finding: the right-alignment gutter can collapse to ONE space

`collaborationPlanRE` requires the `Plan mode` marker to be preceded by
start-of-row or **two or more** horizontal spaces, which is what separates the
footer marker from the same words in ordinary reply prose. At 120 cols with a
long cwd, codex 0.144.5 truncates the left half of that row with `…` and leaves a
**single** space before the marker:

```
  gpt-5.6-sol medium · /private/tmp/…-3b24-4c07… Plan mode
```

so the parser misses it and reports `default` — a **wrong axis value**, not an
unreadable screen, which is the more dangerous failure of the two. Re-rendering
that identical screen with one extra space flips the answer to `plan`, which
pins the gutter rule as the cause. Both widths are committed:
`codex/plan` (200 cols, wide gutter, enforcing) and `codex/plan-narrow-gutter`
(120 cols, pending). Widening the rule belongs to the codex parser ticket.

## Still open (needs a rig)

- A never-accepted-bypass claude profile, to answer capture question 1 in its
  strong form.
- codex `Plan mode` at 120 cols from a **short** cwd, where the hint row is not
  truncated — the third point on the gutter curve, distinct from both committed
  codex cases.

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
matching the new pin" rule is scoped to **the captures in this corpus**, not
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
