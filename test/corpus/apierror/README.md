# API-error tag corpus

Real synthetic-API-error lines lifted out of Claude Code session transcripts —
the input to the tag mapping in `pkg/chat/apierror.go`, and the record of what
reading that tag CHANGES.

Vendored in the same shape as the auth corpus, so it can be mirrored
byte-identically into `meta-harness` when its TypeScript port adopts the
mapping. `harness-wrapper` is the canonical source; regenerate the manifest
with `scripts/sync-apierror-corpus.sh`. The two repos are in sync iff their
committed `MANIFEST.sha256` are equal.

## Layout

    <harness>/<case>/
      line.jsonl   one verbatim transcript line (real capture, never synthetic)
      meta.json    { harness, tag, verdict, code, reason,
                     screenOnly, screenAuthRequired, occurrences, versions }

`verdict` is what the mapping decides:

| verdict   | meaning                                                              |
|-----------|----------------------------------------------------------------------|
| `wall`    | a condition no retry fixes; errors the turn and sets `code`          |
| `errored` | a real failure, but not a wall; errors the turn with no code         |
| `none`    | no verdict — falls through to the screen relabels, unchanged         |

`screenOnly` is what TODAY's screen-only path decides from the same text, and
it is `complete` for every fixture here: Claude Code paints an API error as an
ordinary assistant bubble, so `ExtractMessage` returns it and the turn is
persisted as a SUCCESS whose reply is the error message. That column is the
whole point of the corpus — it is the delta, written down.

`screenAuthRequired` records whether `authRequired()` matches the rendered
text. It is `true` for three of the login-expiry fixtures and still does not
save them: `authRelabel` is gated on an EMPTY clean extraction, and the error
bubble IS the extraction, so the gate declines.

## Provenance

Every `line.jsonl` is a real capture from `~/.claude/projects/*/*.jsonl` on the
operator's machine, 2026-06 → 2026-09, across Claude Code 2.1.219 → 2.1.272.
19 distinct shapes, 99 occurrences; `occurrences` and `versions` record how
many lines collapsed into each fixture and which builds produced them.

Redacted to the fields a parser reads — `type`, `uuid`, `message.{role,model,
content}`, `timestamp`, `isApiErrorMessage`, `error`, `version`. Dropped:
`cwd`, `sessionId`, `parentUuid`, `gitBranch`, `userType`, `entrypoint`,
`isSidechain`, `message.usage`. Nothing was reworded; a fixture is the line
Claude wrote, minus the fields that identify where it wrote it.

## Turn-level delta

The corpus holds LINES; the mapping decides TURNS, and the difference matters.
Measured over the same transcripts, splitting at each user entry and letting
the last assistant entry decide:

| turns | outcome                                                           |
|-------|-------------------------------------------------------------------|
| 51    | complete → errored `auth_required` — ended on a login-expiry line  |
| 35    | complete → errored (no code) — ended on an `API Error: …` line     |
| 5     | UNCHANGED — tagged mid-turn, then a real reply arrived             |
| 1     | UNCHANGED — `unknown` tag, deliberately unmapped                   |

Nothing moves the other way: no turn that errors today completes because of
the tag. The 5 recovered turns are what the LAST WORD ONLY rule exists for —
without it they would have been false positives, and they are real.

## Known gap

`billing_error` has no fixture: no occurrence exists anywhere on this machine,
consistent with a Max-subscription fleet that cannot reach the API-key credit
path. Its mapping is therefore covered only by a synthesised line in
`pkg/chat/apierror_corpus_test.go`, and the mechanism carrying it is what the
99 real ones exercise. Add a real capture when one occurs — the recipe for
inducing the API-key shape on demand is in `../auth/README.md`.
