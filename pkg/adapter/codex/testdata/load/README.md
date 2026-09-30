# Threads codex saved

One directory per codex version: a thread that version saved, as the files an archive keeps of it
(`conformance.SavedSession`: `saved.json`, and each file under `files/<root>/<path>`). The profile
names a version in `Descriptor.Load` only while the pinned codex loads the thread kept here for it,
its name and its goal with it (`TestCodexLoadsSavedThreads`), and `TestLoadSourcesHaveSavedThreads`
fails a source with none, or one with no name and no goal to lose.

A directory is written once, by the version it is named for, against the mock model API, and never
again: a thread the loading version wrote proves nothing about the one that saved it. To record the
pinned version's:

```sh
TZ=UTC HW_RECORD_SAVED_SESSION=/tmp/hw-saved-session/codex HW_REAL_CODEX=<the pinned codex> \
  go test -count=1 -run TestCodexRecordsSavedThread ./pkg/adapter/codex/
```

`HW_RECORD_SAVED_SESSION` is where the thread's roots are while it is recorded. The rollout names
that path, so it says nothing of the machine; the distribution sits beside it, at the same path
with `-harness` added. Both are removed once the thread is saved. `TZ=UTC` keeps the machine's
timezone out of the rollout.

| Version | Recorded | Holds |
|---|---|---|
| `0.144.5` | 2026-09-30, macOS arm64 | Two turns, `PING 1` and `TOOL echo saved`; the name `a saved thread`, in `session_index.jsonl`; the paused goal `Keep what was saved`, in `goals_1.sqlite` and its `-wal`; the profile's account of both, `scratch/native/<thread>.json`; and the agent's memory. |
