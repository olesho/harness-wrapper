# HARNESS-WRAPPER-77 — `permission_mode` handoff note for meta-harness

Sibling repo: `meta-harness` (`~/Work/aether/meta-harness`, override with
`META_HARNESS_DIR`). Paired consume-side ticket: **META-HARNESS-101**.

Unlike [`APPLY.md`](APPLY.md) (HARNESS-WRAPPER-66), this is a **note, not a
byte-exact bundle**: everything meta-harness must change here lives in MH-owned
TypeScript, and the conformance corpus reaches MH through its own vendoring
script rather than through files staged in this directory. It is staged in the
canonical `harness-wrapper` worktree for the same reason APPLY.md is — that is
the unit the fleet supervisor publishes.

## Wire contract

The field name is **`permission_mode`** — snake_case, in **both** directions
(request and response). meta-harness mirrors this repo's JSON shape by contract:
see the WIRE-COMPAT block at `src/gateway/dto.ts:1-21` — *"JSON field names are
Go's snake_case"*.

`omitempty` behaviour is part of that contract: an omitted field and `""` are
indistinguishable on the wire, which is the intended *"leave the harness
default"* semantics — identical to how `effort` and `model` already behave.

## What changes (paths are `$META_HARNESS_DIR`-relative)

### 1. `src/gateway/dto.ts` — `ConversationSummaryDTO` + its constructor

`ConversationSummaryDTO` gains `permission_mode?`. Its constructor
`conversationSummary(id, harness, sessionID)` (`src/gateway/dto.ts:303-308`)
gains a **fourth parameter**. It is called **positionally** at
`src/gateway/server.ts:623`, so this is a **signature change, not just a field
add** — that call site must be updated in the same commit.

### 2. `src/gateway/dto.ts` — `OpenRequest` / `RunTurnRequest`

Both gain `permission_mode?`, threaded through to meta-harness's launch config
(the same path `effort` / `model` already take).

### 3. `src/gateway/errors.ts` — `CHAT_ERROR_TABLE`

The ordered `CHAT_ERROR_TABLE` (`src/gateway/errors.ts:56-69`) gains an
`ErrInvalidConfig -> {400, "invalid_config"}` row, matching the new
`gateway/errorResponse.invalid_config.json` corpus fixture. In Go this is the
first non-`pkg/chat` sentinel in the map (it comes from `pkg/wrapper`).

> **Pre-existing divergence — do not compound it.** Go emits code `"closed"`
> (`cmd/harness-chatd/server.go:479`); meta-harness emits `"gone"`
> (`src/gateway/errors.ts:66`) for the same sentinel. That mismatch predates this
> ticket. Flagged here so the new `invalid_config` row is added *consistently*
> with Go and the existing `closed`/`gone` gap is tracked separately rather than
> widened.

## Corpus / parity expectation

`scripts/check-conformance-corpus.sh:5-14` in this repo is deliberately
**check-only** and never writes into the sibling; the pull/vendoring script is
META-HARNESS-101's deliverable.

Changing the three DTOs changes `fields.json` → changes `MANIFEST.sha256` → the
parity check **fails by construction** until meta-harness re-vendors. **That
failure is the expected state of HARNESS-WRAPPER-77 at merge, not a
regression.**

## Acceptance

Re-vendor `test/conformance/` in meta-harness, then
`scripts/check-conformance-corpus.sh` goes green.
