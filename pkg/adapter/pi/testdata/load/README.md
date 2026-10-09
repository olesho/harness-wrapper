# Sessions pi saved

One directory per pi version: a Session that version saved, as the files an archive keeps of it
(`conformance.SavedSession`: `saved.json`, and each file under `files/<root>/<path>`). The profile
names a version in `Descriptor.Load` only while the pinned pi loads the Session kept here for it
(`TestPiLoadsSavedSessions`), and `TestLoadSourcesHaveSavedSessions` fails a source with none.

A directory is written once, by the version it is named for, against the mock model API, and never
again: a Session the loading version wrote proves nothing about the one that saved it. To record the
pinned version's:

```sh
TZ=UTC HW_RECORD_SAVED_SESSION=/tmp/hw-saved-session/pi HW_REAL_PI=<the pinned pi> \
  go test -count=1 -run TestPiRecordsSavedSession ./pkg/adapter/pi/
```

`HW_RECORD_SAVED_SESSION` is where the Session's roots are while it is recorded. The session's
header names that workspace, which a load rewrites to its own
([ADR-024](../../../../../docs/md/internal/decisions/adr-024-history-rewrites.md)); the distribution
sits beside it, at the same path with `-harness` added. Both are removed once the Session is saved.

| Version | Recorded | Holds |
|---|---|---|
| `1.0.4` | 2026-10-09, macOS arm64, `openai/gpt-4.1-mini` | Two turns, `PING 1` and `TOOL echo saved`, in `config/sessions/<time>_<id>.jsonl`, whose header names `/private/tmp/hw-saved-session/pi/workspace`; and the agent's memory. |
