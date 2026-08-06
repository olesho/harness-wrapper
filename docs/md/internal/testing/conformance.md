# Cross-language conformance corpus

`test/conformance/` freezes the **wire contracts two independent implementations must agree on** — the
Go one in this repo and the TypeScript one in meta-harness. It is the machinery behind Layer 0 of the
[testing tiers](README.md): not "does this code work", but "does this shape still mean what the other
side thinks it means".

It is a separate root from [`test/corpus/`](corpus.md) on purpose. The corpus records *bytes a harness
emitted*; the conformance corpus records *shapes we promised*.

## What is frozen

| Directory | Contract |
|---|---|
| `gateway/` | every [`harness-chatd`](../../guide/gateway.md) request/response DTO |
| `turnresult/` | [`StructuredTurnResult`](../turnproto.md) plus the `transcript.Event` and `Usage` shapes it embeds |
| `cli/` | the [structured-run](../../guide/cli.md#structured-one-shot-structured-run) exit codes, the frozen stderr deadline line, and the status → (exit code, stderr anchor) pairing |
| `MANIFEST.sha256` | a hash of every corpus file, sorted by path |

Each area carries a `fields.json` — the neutral field contract — plus example instances per DTO.

## The neutral field vocabulary

Field descriptions are language-agnostic by construction:

```json
{ "name": "HarnessSessionID", "json_tag": "harnessSessionID", "type": "string", "optional": false }
```

`type` is drawn from a small closed set (`string`, `int`, `bool`, `array`, `object`, `ref`, `any`,
`timestamp`), and `optional` is a **single boolean** that collapses Go's pointer / `omitempty` /
`omitzero` distinctions into the one fact the other language can check. Fields excluded from JSON
entirely are not described at all.

Comparison is **structural, not byte-identical**: Go emits struct-field order, TypeScript emits
insertion order, and forcing them to agree on ordering would freeze an implementation detail rather
than a contract. The manifest guards file integrity, not field order.

## Regenerating

```bash
make regen-conformance
```

This is the **only** supported entry point. It is an ordered two-step:

```bash
UPDATE_GOLDEN=1 go test ./cmd/harness-chatd/   # emits gateway/
UPDATE_GOLDEN=1 go test ./test/conformance/    # emits turnresult/ + cli/, then the manifest
```

The order matters because the gateway DTOs are unexported types in `package main`, so only a test
*hosted in that package* can describe them — while the manifest is written by the external package
over the whole tree. A plain `UPDATE_GOLDEN=1 go test ./...` does not guarantee that ordering and can
write a manifest over stale gateway bytes.

Regenerate only when you **intended** to change a contract. A diff here is the review artifact: it is
the moment to ask whether the other implementation needs the same change in the same release.

## Related golden files

The same `UPDATE_GOLDEN=1` convention freezes the local CLI surfaces:

| Command | Freezes |
|---|---|
| `UPDATE_GOLDEN=1 go test ./cmd/harness-wrapper/` | the full `--help` text and the flag set |
| `UPDATE_GOLDEN=1 go test ./cmd/harness-chatd/` | the route list, the flag set, and the wire contract |

Those are single-implementation freezes — they catch an accidental flag rename or a dropped route in
review, without involving the other repo.

## Direction of travel

harness-wrapper is **canonical**: it regenerates the corpus, meta-harness only consumes it. A
check-only script compares the two copies of the manifest and fails on drift without ever writing into
the sibling repository. The same one-way discipline applies to every shared corpus in this repo
(auth screens, model pickers, permission-mode footers): one owner writes, everyone else verifies.

> **Scope gap.** The shipped [Python and TypeScript clients](../../guide/clients.md) are **not** covered
> by this corpus. Their suites are ordinary unit tests against an in-process HTTP stub, so a client
> could drift from the gateway DTOs without this tier noticing.
