# Bake-off corpus

Recorded PTY byte streams: the substrate for the [emulator bake-off](../../docs/md/internal/decisions/adr-001-vt100.md)
and the input to the Layer-2 adapter replay tests and the drift pipeline.

> 📖 Full story — canonical scenarios, adversarial recordings, recording workflow, privacy — is in the
> **[Corpus docs](../../docs/md/internal/testing/corpus.md)**.

## Layout

```
test/corpus/
  <harness>/<scenario>/
    bytes.raw          required  raw PTY byte stream captured from the harness
    meta.json          required  harness, recorded_at, terminal dims, binary version
    expected.txt       optional  ground-truth final assistant text (fidelity metrics)
    transcript.jsonl   optional  copy of the harness's own session log, for reference
```

`<harness>/adversarial/<scenario>/` holds negative recordings that must NOT fire the marker.

## Interactive recording

```sh
go run ./internal/screenbench/cmd/screenbench-record \
    --harness codex \
    --bin "$(which codex)" \
    --out test/corpus/codex/short-reply \
    --cols 120 --rows 40 \
    --binary-version "$(codex --version)" \
    --notes "single-turn short reply"
```

Scripted refreshes go through `make rebake-corpus` — see
[Versions & Drift](../../docs/md/internal/versions-drift.md).

## Privacy

Recordings may contain whatever you typed and whatever the model said. Treat scenarios as **public**
before checking them in — strip secrets, paths, internal info.
