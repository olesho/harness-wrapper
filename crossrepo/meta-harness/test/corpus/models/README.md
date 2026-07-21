# model-picker conformance corpus

Cross-language conformance fixtures for the `/model` picker parser
(`ParseModelPicker()` in `pkg/discovery/models/picker.go` and
`parseModelPicker()` in `src/discovery/models.ts`).

This corpus is **vendored byte-identically into both repos** (`harness-wrapper`
and `meta-harness`). `harness-wrapper` is the canonical source; mirror it with
`scripts/sync-models-corpus.sh`. `MANIFEST.sha256` pins the tree; each repo's
offline conformance test (`pkg/discovery/models/models_corpus_test.go`,
`test/discovery/models_corpus.test.ts`) asserts:

1. `screen.render(bytes.raw)` byte-equals the pinned `expected.txt` (so a
   screen-render drift is caught), and
2. `parseModelPicker(expected.txt, harness)` deep-equals `meta.json.expected`
   for every case (so a parser drift is caught), and
3. the recomputed corpus hash matches `MANIFEST.sha256`.

The two repos are in sync **iff** their committed `MANIFEST.sha256` are equal —
a one-line cross-repo drift check. Both implementations of the parser must
therefore agree on every fixture.

## Layout

    <harness>/<case>/
      bytes.raw     verbatim recorded terminal byte stream (real capture, never synthetic)
      expected.txt  canonical rendered screen snapshot (pkg/screen render of bytes.raw)
      meta.json     { harness, binary_version, recorded_at, cols, rows, notes, expected }

`meta.json.expected` is the canonical ordered `[]Info` the parser must produce
(`{ID, Label, Description, Current, IsDefault}` per model).

## Canonical-direction contract

`harness-wrapper` is CANONICAL: this repo owns the fixtures, and
`scripts/sync-models-corpus.sh --to <meta-harness>` mirrors the tree outward.
The manifest check catches THIS repo drifting; the meta-harness side re-runs the
identical corpus through its own `parseModelPicker` and asserts the same
`MANIFEST.sha256`, so a fixture edit that forgets to re-sync fails the unit gate
in BOTH repos.

## Whitespace normalization

`pkg/screen` (the vt10x emulator) renders every gap as ASCII space (U+0020), so
`expected.txt` never contains a non-ASCII whitespace column. The parser still
handles Unicode whitespace in the label→description gap (JS `\s` semantics),
because the regex classes are spelled out explicitly rather than relying on
RE2's ASCII-only `\s`; that engine-parity is proven directly against crafted
inputs in `pkg/discovery/models/picker_test.go` (`TestRE2JSWhitespaceParity`),
not via a fixture.

## Provenance

Every `bytes.raw` is a **real capture** recorded live over a PTY:

- `claude-code/model-picker` — claude-code 2.1.204, recorded 2026-07-08.
- `codex/model-picker` — codex 0.143.0, recorded 2026-07-08.

Both were vendored from meta-harness's `test/corpus/{claude-code,codex}/model-picker`
fixtures; the `expected.txt` and `meta.json.expected` fields are authored here
(the seed shipped `bytes.raw` + `meta.json` without them). No fixture is
invented.

## Refresh cadence

Model ids churn faster than the auth signal. When a harness ships a new model or
reformats its picker, re-capture the `/model` picker and, in this repo, update
all of: `pkg/discovery/models/models.json`, the case's `bytes.raw`,
`expected.txt`, and `meta.json.expected`. Then run
`scripts/sync-models-corpus.sh` (optionally `--to <sibling meta-harness>`) and
commit the regenerated `MANIFEST.sha256`.
