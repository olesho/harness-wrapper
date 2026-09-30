codex, mock: codex-cli 0.144.5 (pinned 0.144.5), harness-wrapper (devel), linux/arm64

| Check | Result | What was seen |
|---|---|---|
| `source.elsewhere` the archive was saved on another machine, which this one cannot reach into | **pass** | saved on lima-agentd-ubuntu (linux/arm64, codex-cli 0.144.5); loaded on lima-agentd-debian (linux/arm64) |
| `load.record-read` the adapter's record reader reaches the restored record's end, with nothing published | **pass** | 6 observations in 1 batch(es), end of record true; the saved prompt, reply, tool call and tool result read: true true true true; checkpoint at offset 35111 of the restored 35111 bytes; resets 0, rescans 0, faults 0; err=<nil> |
| `checkpoint.carried` the source's checkpoint, given to the restored record | info | the source's checkpoint (offset 34739, inode 36599) on the restored file: 1 reset(s) [replaced], then all 6 observations delivered again — a checkpoint names the file it was made on, so a Load seeds its own |
| `load.history` the first resumed model request carries the saved turns and the tool result | **pass** | request 1 carries the source's two turns — prompt, reply, tool call, tool result, reply — before the new prompt (13 items); the prompt names no nonce: true |
| `load.same-session` the Session reopens under its saved native id | **pass** | saved 01a0f181-5921-7a20-9b7c-de1827279c85, reopened 01a0f181-5921-7a20-9b7c-de1827279c85 |
| `load.new-events-only` after the reopen from the seeded checkpoint, only the new turn's record events are delivered | **pass** | 3 record observations; ids the restored record already held: 0; dates not compared: the record was saved under another machine's clock; user inputs: ["PING again"]; resets 0, faults 0 |
| `load.aux` the Session's name and goal are read back after the load | **pass** | saved goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; read back goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; err=<nil> |
| `load.second-reopen` after a stop and a second reopen, what the restored Session added is kept, and only new record events follow | **pass** | request 2 carries the saved turns and the first restored turn; missing: []; err=<nil>; reopened 01a0f181-5921-7a20-9b7c-de1827279c85 from the checkpoint at offset 40179; 8 record observations; ids the restored record already held: 0; dates not compared: the record was saved under another machine's clock; user inputs: ["PING third" "TOOL pwd; cat saveload-note.txt"]; resets 0, faults 0 |
| `load.new-cwd` the resumed Session works in the new workspace | **pass** | a tool run after the restore printed "Chunk ID: 4b3690\nWall time: 0.0001 seconds\nProcess exited with code 0\nOriginal token count: 20\nOutput:\n$WORK/restored/candidate/agent/ws\nt4858f5e4fe" |
| `load.workspace-file` a workspace file saved with the Session is read by a tool in the new workspace | info | saveload-note.txt restored; the tool's output holds nonce T: true (a smoke check, not workspace fidelity) |
| `load.aux-kept` the Session's name and goal are still there after two more turns and a second reopen | **pass** | saved goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; read back goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; err=<nil> |
| `load.no-rewrite` the restored record is appended to, its saved bytes unchanged: relocation moves paths, not contents | **pass** | the first 35111 bytes of the continued record hash to the saved sha256: true; it grew: true; relocation rules: 0 |
| `load.memory` the saved memory file is in the new memory directory | info | memory/notes.md restored with the saved content: true |
| `load.fresh-credential` the restored Session's requests carry the credential staged for the new environment | **pass** | 4 requests with the new environment's credential, 0 with the source's, 0 with none |
| `negative.harness-refuses` with no history restored, the harness alone refuses to resume the id | **pass** | the harness has no record of the session: thread/resume: codex: no rollout found for thread id 01a0f181-5921-7a20-9b7c-de1827279c85 (-32600) |
| `negative.empty-record` with no history restored, the record reader finds an empty record | info | 0 observations, checkpoint false, end of record true, err=<nil> — the sign a Load must refuse on, before any open |
| `negative.no-context` with no history restored, a reopen through the adapter fails or lacks the earlier context | **pass** | the adapter opened a Session 01a0f181-bce9-7602-a443-6e512da92c32 (the saved id: false) — a fresh conversation, not an error; request 11 has 5 items; a nonce among them: false (err=<nil>); err=<nil> |

| Saved state | Files | Conversation continued | Same Session id | Native state read back |
|---|---|---|---|---|
| `history-roots`: the adapter's history_roots alone: what a delete archives today | 2 | true | true | goal="", goal_status="", name="" |
| `rollout-only`: the thread's rollout alone | 1 | true | true | goal="", goal_status="", name="" |
| `rollout+index`: the rollout and session_index.jsonl | 2 | true | true | goal="", goal_status="", name="saveload probe m98e04f7bd4" |
| `rollout+goals`: the rollout and goals_1.sqlite with its sidecars | 4 | true | true | goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="" |
| `rollout+goals-wal`: the rollout, goals_1.sqlite and its -wal, without its -shm | 3 | true | true | goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="" |
| `rollout+goals-db`: the rollout and goals_1.sqlite without its sidecars | 2 | true | true | goal="", goal_status="", name="" |

| Source file | Files | Bytes | Class | Saved by |
|---|---|---|---|---|
