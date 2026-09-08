# HARNESS-WRAPPER-101 — `StructuredTurnResult.permission_mode` handoff note for meta-harness

Sibling repo: `meta-harness` (`~/Work/aether/meta-harness`, override with
`META_HARNESS_DIR`). Paired consume-side ticket: **META-HARNESS-101**.

Like its siblings [`HARNESS-WRAPPER-77-permission-mode.md`](HARNESS-WRAPPER-77-permission-mode.md)
and [`HARNESS-WRAPPER-78-sandbox-defaults-argv.md`](HARNESS-WRAPPER-78-sandbox-defaults-argv.md), and
unlike [`APPLY.md`](APPLY.md) (HARNESS-WRAPPER-66), this is a **note, not a byte-exact bundle**:
everything meta-harness must change lives in MH-owned TypeScript, and the conformance corpus reaches
MH through its own vendoring script rather than through files staged here.

> **Ticket numbering.** An earlier draft of the Go-side plan named the consume-side ticket
> **META-HARNESS-129**. The number of record is **META-HARNESS-101** — the one written down at
> `HARNESS-WRAPPER-77-permission-mode.md:4`. If META-HARNESS-129 exists, it is the same work under a
> second number and must be reconciled to one authority before either side lands.

## What lands on the Go side

`pkg/turnproto.StructuredTurnResult` gains one optional key:

```json
{"status":"completed", "...": "...", "permission_mode":"bypass"}
```

Declared **last** in the Go struct, so the `turnresult/fields.json` delta is an **append**, not a
mid-array insert. New corpus fixture:
`turnresult/StructuredTurnResult.completed_permission_mode.json`, with the existing
`StructuredTurnResult.completed` serving as the omitted variant — mirroring the gateway pair
`conversationSummary.permission_mode.json` / `.permission_mode_omitted.json`.

Populated in `cmd/harness-wrapper/structured_run.go` from `wrapper.EffectiveLaunchRung`, the
argv→rung inverse of the launch-time injection.

## Wire contract — reused verbatim, not re-opened

The name `permission_mode` (snake_case) and the omitempty semantics — omitted ≡ `""` ≡ "leave the
harness default", the way `effort` and `model` already behave — are already frozen by
[`HARNESS-WRAPPER-77-permission-mode.md`](HARNESS-WRAPPER-77-permission-mode.md) §"Wire contract",
"in both directions (request and response)". This ticket adopts that verbatim. There is nothing to
negotiate about the key name or the presence rule.

## ⚠ SEMANTICS DIVERGENCE — the load-bearing part of this note

HARNESS-WRAPPER-77's "response" direction meant `ConversationSummaryDTO`. **This is a different
field with the same key name, and a different value space.**

| | `conversationSummary.permission_mode` | `StructuredTurnResult.permission_mode` |
|---|---|---|
| Reading | what was **requested** — echoed verbatim from `openRequest` | the **effective canonical rung**, resolved from the final launch inputs |
| Vocabulary | canonical rungs **plus harness-native spellings** — `isSupportedPermissionMode` (`pkg/wrapper/wrapper.go`) accepts them, so `acceptEdits`, `dontAsk`, `read-only` can legitimately appear on the wire today (see the `PermissionMode` Literal in `clients/python/harness_chat.py`) | canonical rungs **only**: `plan` \| `manual` \| `ask` \| `auto` \| `bypass`. A native spelling is **never** emitted — `acceptEdits` is reported as `ask`, `danger-full-access` as `bypass` |
| Source | the open request | `wrapper.EffectiveLaunchRung(harness, finalArgs, requestedMode)` |
| On failure | reports the requested value | **never populated on `startup_error`** |

**Do not map both onto one TypeScript type or one zod schema.** They are different value spaces
sharing a key name, by design. A union type that admits `dontAsk` on the structured-result side would
advertise a value the Go producer cannot emit; a canonical-rungs-only enum on the summary side would
reject values the Go producer *does* emit.

## ⚠ ENFORCEMENT CAVEAT — MH orchestrators are the exact audience that would misread this

**A restrictive rung on a structured run is a launch argument, not a gate.**

- **Presence of `"bypass"` is trustworthy** for a turn that reached the harness. Every unrestricted
  launch path reports it, including those carrying no canonical `--permission-mode` at all:
  `--sandbox-defaults` (which injects `--dangerously-skip-permissions`), a raw
  `--dangerously-skip-permissions` after `--`, and codex's `-s danger-full-access` in every spelling.
  Closing that hole is the point of the field.
- **Absence never means "safe."** It means no canonical rung could be *named*: the harness default,
  an unsupported harness, a codex argv setting only the `-a` approval axis, or a present-but-unreadable
  flag. Claude's `dontAsk` is no longer one of these causes: it reports the `manual` rung.
- **`"manual"` does NOT mean "this turn was supervised."** On the `structured-run` path claude's
  per-tool permission dialog is not detected at all (no input request is emitted, so restrictive
  rungs stall an unattended turn to the deadline), and the approval callback is wired unconditionally
  to a catch-all auto-accept, which auto-approves codex's approval prompts. Only codex's `-s` sandbox
  axis is enforced by codex itself. Pinned in `pkg/oneshot/permission_pin_test.go` and tabulated in
  `docs/md/internal/wrapper.md` §"Runtime enforcement per path".
- **The argv half only.** The `IS_SANDBOX=1` environment half that `--sandbox-defaults` also sets
  (permits root, suppresses claude's Bypass Permissions acceptance screen) is not representable in a
  rung string and is not reflected. A `--sandbox-defaults` run and a bare `--permission-mode bypass`
  run both report `bypass` while differing in those behaviours.
- **In-band mutation is invisible.** A client can flip the mode inside the TUI; nothing observes it —
  the same residual gap `conversationSummary.permission_mode` documents.

## Backward compatibility

- **Old JSON → new consumer** (cheap): a result line lacking `permission_mode` still parses, with the
  field recovering as `""`. Pinned Go-side in `pkg/turnproto/turnproto_test.go`.
- **New producer → old TS consumer** (the genuinely risky direction): a Go producer that now emits an
  extra key against an MH parse path that may be strict about unknown keys. This is **not testable
  from the Go repo** and is **META-HARNESS-101's acceptance item**: confirm the zod / parse layer for
  `StructuredTurnResult` tolerates (or explicitly models) `permission_mode` before an upgraded
  harness-wrapper is deployed against an un-upgraded MH.

## Corpus / parity expectation

The cross-language wire corpus is **`test/conformance/`** at this repo's root —
`turnresult/fields.json`, `turnresult/StructuredTurnResult.<case>.json`, `cli/`, `gateway/`, and
`MANIFEST.sha256` — generated by `test/conformance/conformance_test.go` and
`cmd/harness-chatd/conformance_test.go` (regenerate with `make regen-conformance`; it is an ordered
two-step, never a plain `UPDATE_GOLDEN=1 go test ./...`). It is **not** `crossrepo/meta-harness/test/corpus`,
which holds only the HARNESS-WRAPPER-66 model-picker PTY fixtures.

`scripts/check-conformance-corpus.sh` is deliberately **check-only** and never writes into the
sibling; the pull/vendoring script is META-HARNESS-101's deliverable.

Adding the field changes `fields.json` → changes `MANIFEST.sha256` → the parity check **fails by
construction** until meta-harness re-vendors. **That failure is the expected state of
HARNESS-WRAPPER-101 at merge, not a regression** — the same posture HARNESS-WRAPPER-77 already
declares. The check is a dev-machine tool and is not invoked by `make test`.

## Acceptance

1. MH models `StructuredTurnResult.permission_mode` as an optional canonical-rung string, **separate**
   from the `conversationSummary.permission_mode` type.
2. MH's parse path tolerates the new key on results produced by an upgraded harness-wrapper
   (the new-producer/old-consumer item above).
3. Re-vendor `test/conformance/` in meta-harness, then `scripts/check-conformance-corpus.sh` goes
   green.
