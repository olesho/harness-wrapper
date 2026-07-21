# hook-merge dedup/authority corpus

Cross-language conformance fixtures for hook-event **dedup + source authority**:
the identity ladder on `transcript.Event.ID()` (`pkg/transcript/event.go`) and
the parent/subagent source-authority filter `admitParent`
(`pkg/harness/filter.go`). Each fixture is an input event sequence plus the
deduped, order-normalized output it must produce.

This corpus follows the same shape as its siblings under `test/corpus/` (see the
[parent README](../README.md)): one directory per case, inputs and their
expected output committed side by side, goldens stored verbatim in the tree.

## Layout

    <case>/
      input.json    ordered arrival sequence + the latched acquisition mode
      golden.json   the deduped / order-normalized surviving set

There is no `bytes.raw`/`meta.json` pair here: these fixtures are *authored
event sequences*, not recorded PTY streams, because they exercise the
post-parse dedup/authority path rather than a terminal emulator.

### `input.json`

    {
      "mode": "hooks",              // latched effective mode: "hooks" | "stream"
      "run_id": "run-x",            // RunID for the dedup scope; also the hsid fallback
      "notes": "...",               // what rung / authority rule this case exercises
      "events": [                   // ARRIVAL order (pre-dedup, pre-sort)
        {
          "source": "live|file",           // acquisition provenance (Event.Source)
          "harness_session_id": "parent",  // dedup scope; parent vs subagent session
          "parent_session_id": "",         // non-empty => subagent (admitted in any mode)
          "native_id": "",                 // Event.NativeID — rung 1 when set
          "type": "text|tool_use|tool_result|session_meta",
          "role": "user|assistant|tool|system",
          "timestamp": "2026-01-01T00:00:00Z",  // NATIVE timestamp (see constraint below)
          "text": "", "tool_name": "", "tool_use_id": "",
          "tool_input": null, "output": "", "uuid": ""
        }
      ]
    }

`source`, `harness_session_id`, `parent_session_id`, and `native_id` are carried
as explicit fields because `Event.Source` and `Event.NativeID` are `json:"-"`
(durable-store metadata, not the public wire shape) yet both drive the
dedup/authority path.

### `golden.json`

    { "events": [ { "id", "harness_session_id", "source", "type", "role",
                    "timestamp", "text?", "tool_name?", "tool_use_id?",
                    "output?", "uuid?" } ] }

`id` is `transcript.Event.ID()` — the dedup identity. Goldens are regenerated
from the live Go path, never hand-edited:

    go test ./pkg/harness -run TestHookMergeCorpus -update

## What the test does (`pkg/harness/hookmerge_corpus_test.go`)

For each fixture, in arrival order:

1. **Authority filter** — every event passes `admitParent(mode, source, type,
   isSubagent)`, the *identical* predicate `emit()` applies in
   `pkg/harness/run.go`. Under `hooks` the file is authoritative for the parent
   conversation (live parent conversation kinds are dropped; live session/usage
   metadata is still admitted); subagent events are admitted in any mode.
2. **Dedup** — survivors are collapsed by `(run_id, harness_session_id,
   Event.ID())`, the consumer dedup key documented on
   `transcript.EventEnvelope`. First admitted copy of an identity wins.
3. **Normalize** — the surviving set is **sorted by `(harness_session_id, id)`
   and stripped of `Seq`**, so the golden is a SET, not a stream.

The normalization is deliberate. Go assigns `Seq` at emission time and delivers
in drain order (`emit()` / `ev.Seq = o.seq`), whereas TS orders by
seq-then-timestamp (`hookMerge.ts compareOrder`). The two need not agree on
emission/merge ORDER; the parity assertion is only that the same *logical
events, deduped identically*, survive on both sides. So the test compares
dedup-collapsed, order-normalized streams — not raw `Seq`.

## Fixture-authoring constraint: identical native timestamp on collapsing copies

`Event.ID()` folds `Timestamp.UnixNano()` into the content-hash rung
(`event.go`), and cross-source stability holds **only when both copies carry the
identical NATIVE timestamp** (the comment at `event.go` spells this out). So any
live+file (or otherwise content-hash-rung) collapse fixture MUST give both
copies the **same** native `timestamp`. Hand them different arrival timestamps
and the two hashes diverge, the events do NOT merge, and the golden silently
gains a spurious extra row.

This bites only the content-hash rung (rung 4). The higher rungs are
timestamp-independent: `native-id-collapse` deliberately gives its two copies
*different* timestamps to prove rung 1 (`NativeID`) short-circuits before the
hash; `uuid-keyed-messages` likewise collapses on the UUID rung across differing
timestamps.

## Cases

| case | rung / rule exercised | in → out |
| --- | --- | --- |
| `native-id-collapse` | rung 1 `NativeID`; collapses despite different timestamps | 2 → 1 |
| `live-file-collapse` | rung 4 content hash; live+file, **same** native timestamp | 2 → 1 |
| `tool-use-result-shared-id` | rung 3 `ToolUseID`, kind-qualified so use ≠ result | 2 → 2 |
| `uuid-keyed-messages` | rung 2 `msg:<uuid>`; redelivery collapses, ignores content | 3 → 2 |
| `content-hash-fallback` | rung 4 content hash for a parent event | 3 → 2 |
| `subagent-vs-parent-authority` | `admitParent` authority + hsid-scoped dedup | 5 → 3 |

`subagent-vs-parent-authority` is the integration case: it drops the live parent
copy (file authoritative), keeps live non-conversation metadata, collapses the
live+file subagent copies, and keeps a parent row and a subagent row that share
a content hash but differ in `harness_session_id` — proving the dedup key is
session-scoped.

## Cross-repo scope

`meta-harness` (the TS repo, `mergeHookEvents`) is **not** in this worktree, so
its output cannot be run or verified here. The deliverable in THIS repo is
`{corpus inputs + committed goldens + the Go test}`, and the goldens are authored
to match **Go's own** `Event.ID()`-dedup + `admitParent` output — that is what
the Go test asserts. Capturing/aligning goldens against TS `mergeHookEvents`
output, and landing a TS-side test that consumes this same corpus, are separate
cross-repo steps tracked on a `meta-harness` ticket and are **not** closing
conditions for this corpus.
