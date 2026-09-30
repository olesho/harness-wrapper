# Sessions claude saved

One directory per claude version: a Session that version saved, as the files an archive keeps of it
(`conformance.SavedSession`: `saved.json`, and each file under `files/<root>/<path>`). The profile
names a version in `Descriptor.Load` only while the pinned claude loads the Session kept here for it
(`TestClaudeLoadsSavedSessions`), and `TestLoadSourcesHaveSavedSessions` fails a source with none.

A directory is written once, by the version it is named for, against the mock model API, and never
again: a Session the loading version wrote proves nothing about the one that saved it. To record
the pinned version's:

```sh
TZ=UTC HW_RECORD_SAVED_SESSION=/tmp/hw-saved-session/claude-code HW_REAL_CLAUDE=<the pinned claude> \
  go test -count=1 -run TestClaudeRecordsSavedSession ./pkg/adapter/claudecode/
```

`HW_RECORD_SAVED_SESSION` is where the Session's roots are while it is recorded. The transcript
names that path, so it says nothing of the machine; the distribution sits beside it, at the same
path with `-harness` added. Both are removed once the Session is saved.

| Version | Recorded | Holds |
|---|---|---|
| `2.1.283` | 2026-09-30, macOS arm64 | Two turns, `PING 1` and `TOOL echo saved`, and the agent's memory. The transcript's directory is named for `/private/tmp/hw-saved-session/claude-code/workspace`. |
