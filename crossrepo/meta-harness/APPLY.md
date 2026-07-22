# HARNESS-WRAPPER-66 — cross-repo deliverable for meta-harness

> This file covers **HARNESS-WRAPPER-66 only**; the permission-mode cross-repo handoff is its sibling file [`HARNESS-WRAPPER-77-permission-mode.md`](HARNESS-WRAPPER-77-permission-mode.md), and the `--sandbox-defaults` argv tripwire is [`HARNESS-WRAPPER-78-sandbox-defaults-argv.md`](HARNESS-WRAPPER-78-sandbox-defaults-argv.md) — neither sibling is this file's bundle, and neither is the other's.

This directory is a **byte-exact, ready-to-apply bundle** for the sibling
`meta-harness` repo (`~/Work/aether/meta-harness`, override with
`META_HARNESS_DIR`). It is staged here in the canonical `harness-wrapper`
worktree because that is the unit the fleet supervisor publishes; the files
themselves belong in `meta-harness` and must be committed / PR'd there under the
paired ticket (see below). `harness-wrapper` is CANONICAL for the model-picker
corpus — these bytes are pulled verbatim from `test/corpus/models/` in this repo.

## Why this exists

The harness-wrapper corpus documents the invariant: *the two repos are in sync
IFF their committed `test/corpus/models/MANIFEST.sha256` are byte-equal.* That
did not hold against meta-harness's seed fixtures — its
`test/corpus/{claude-code,codex}/model-picker/` dirs shipped only `bytes.raw` +
`meta.json` (no `expected` field, no `expected.txt`) and it had no
`test/corpus/models/` tree or manifest at all. This bundle closes that gap so the
invariant becomes meetable, and it ships the reverse-direction guard
(`pkg/versions/parity_test.go` documents this class of guarantee as
one-directional; the meta-harness-drifts-first direction must be enforced by a
symmetric vendored `--check` in meta-harness CI — the META-HARNESS-91 analogue).

## Layout (paths are meta-harness-relative)

    test/corpus/models/                 byte-identical mirror of harness-wrapper's
      MANIFEST.sha256                     canonical corpus (README, MANIFEST, both cases)
      README.md
      claude-code/model-picker/{bytes.raw,expected.txt,meta.json}
      codex/model-picker/{bytes.raw,expected.txt,meta.json}
    test/discovery/models_parse.test.ts rewritten — reads the shared fixture, no hardcoded ids
    scripts/sync-models-corpus.sh       manifest regen + --check / --against (symmetric guard)
    .github/workflows/ci.yml            adds the `sync-models-corpus.sh --check` CI step

The `expected` field (canonical `[]ModelInfo` in picker order, Go-style
`{ID, Label, Description, Current, IsDefault}` serialization) and `expected.txt`
render are authored in harness-wrapper and mirrored here byte-for-byte, so both
repos' manifests match. `bytes.raw` is already byte-identical to meta-harness's
seed captures (verified).

## Apply

From the harness-wrapper repo root:

    scripts/sync-models-corpus.sh --to "$META_HARNESS_DIR"   # mirrors test/corpus/models/

then copy the three non-corpus files into meta-harness:

    cp crossrepo/meta-harness/test/discovery/models_parse.test.ts "$META_HARNESS_DIR/test/discovery/"
    cp crossrepo/meta-harness/scripts/sync-models-corpus.sh       "$META_HARNESS_DIR/scripts/"
    chmod +x "$META_HARNESS_DIR/scripts/sync-models-corpus.sh"
    cp crossrepo/meta-harness/.github/workflows/ci.yml           "$META_HARNESS_DIR/.github/workflows/"

Then in meta-harness: `pnpm vitest run test/discovery/models_parse.test.ts` and
`scripts/sync-models-corpus.sh --check` must both pass, and
`test/corpus/models/MANIFEST.sha256` must be byte-equal to harness-wrapper's.

## Paired ticket

The CI check (deliverable 3) is owned by the meta-harness-side ticket, the
**META-HARNESS-91 analogue** referenced in `pkg/versions/parity_test.go:11-18`.
Raise/track that ticket in meta-harness to land the `ci.yml` `--check` step and
the manifest guard; the fixture-shape adoption + test rewrite ship together with
it. The manifest guard is ALSO enforced inside `models_parse.test.ts` (the
`models corpus manifest` describe block) so `harness ci` catches drift even
before the dedicated CI step.
