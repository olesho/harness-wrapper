# auth conformance corpus

Cross-language conformance fixtures for the logged-out / auth-required signal
(`authRequired()` in `pkg/chat/ready.go` and `src/chat/ready.ts`).

This corpus is **vendored byte-identically into both repos** (`harness-wrapper`
and `meta-harness`). `harness-wrapper` is the canonical source; mirror it with
`scripts/sync-auth-corpus.sh`. `MANIFEST.sha256` pins the tree; each repo's
offline conformance test (`pkg/chat/auth_corpus_test.go`,
`test/chat/auth_corpus.test.ts`) asserts:

1. `authRequired(harness, screen.txt) === meta.authRequired` for every case, and
2. the recomputed corpus hash matches `MANIFEST.sha256`.

The two repos are in sync **iff** their committed `MANIFEST.sha256` are equal —
a one-line cross-repo drift check. Both implementations of `authRequired` must
therefore agree on every fixture, which is what "enforce the same resolution"
means here.

## Layout

    <harness>/<case>/
      screen.txt   verbatim rendered terminal screen (real capture, never synthetic)
      meta.json    { harness, authRequired, state, source, note }

## Provenance

Every `screen.txt` is a **real capture** recovered from live testing on VM
`167.233.43.85` (root; codex 0.144.6 + claude-code 2.1.215, both logged out),
recorded in Claude Code sessions `d413926b` (meta-harness / TS) and `3e9ee981`
(harness-wrapper / Go). No fixture is invented — inventing the claude fixture
(omitting the `✻ … for 0s` thinking marker) is exactly the bug that let the
original PRs ship green while broken.

## Known gap

No **logged-in successful-reply** claude screen was captured, so there is no
`claude-code` negative fixture guarding against over-broad claude anchors (only
`codex/normal-composer` guards the codex side). Add a real logged-in claude
reply capture as a `authRequired:false` case on the next live run.
