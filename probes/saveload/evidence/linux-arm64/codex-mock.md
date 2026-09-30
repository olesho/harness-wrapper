codex, mock: codex-cli 0.144.5 (pinned 0.144.5), harness-wrapper (devel), linux/arm64

| Check | Result | What was seen |
|---|---|---|
| `source.turns` two source turns: a reply with nonce A, a tool result with nonce T | **pass** | turn 1 completed, reply "PONG a2925dfd2b3"; turn 2 completed, 1 tool call(s), output "Chunk ID: d36b54\nWall time: 0.0000 seconds\nProcess exited with code 0\nOriginal token count: 3\nOutput:\nt4858f5e4fe" |
| `source.stopped` the source harness and its children stopped before anything is copied | **pass** | Close: stopped=true drained=true err=<nil>; processes still naming the source: 0 |
| `source.aux` the Session's name and goal are set, and read back, before the save | **pass** | goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; err=<nil>; processes left: 0 |
| `archive.standalone` the candidate state is copied to a standalone archive, each file with its size and hash | **pass** | 7 files in the candidate archive; 7 variants saved |
| `source.inaccessible` the source's roots are gone before the restore starts | **pass** | the record's path: stat $WORK/source/profile/sessions/2026/09/30/rollout-2026-09-30T10-49-41-01a0f181-5921-7a20-9b7c-de1827279c85.jsonl: no such file or directory; the kept copy: open $WORK/.gone-4710e2dcf1: permission denied |
| `load.record-read` the adapter's record reader reaches the restored record's end, with nothing published | **pass** | 6 observations in 1 batch(es), end of record true; the saved prompt, reply, tool call and tool result read: true true true true; checkpoint at offset 35111 of the restored 35111 bytes; resets 0, rescans 0, faults 0; err=<nil> |
| `checkpoint.carried` the source's checkpoint, given to the restored record | info | the source's checkpoint (offset 34739, inode 36599) on the restored file: 1 reset(s) [replaced], then all 6 observations delivered again — a checkpoint names the file it was made on, so a Load seeds its own |
| `load.history` the first resumed model request carries the saved turns and the tool result | **pass** | request 4 carries the source's two turns — prompt, reply, tool call, tool result, reply — before the new prompt (13 items); the prompt names no nonce: true |
| `load.same-session` the Session reopens under its saved native id | **pass** | saved 01a0f181-5921-7a20-9b7c-de1827279c85, reopened 01a0f181-5921-7a20-9b7c-de1827279c85 |
| `load.new-events-only` after the reopen from the seeded checkpoint, only the new turn's record events are delivered | **pass** | 3 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["PING again"]; resets 0, faults 0 |
| `load.aux` the Session's name and goal are read back after the load | **pass** | saved goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; read back goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; err=<nil> |
| `load.second-reopen` after a stop and a second reopen, what the restored Session added is kept, and only new record events follow | **pass** | request 5 carries the saved turns and the first restored turn; missing: []; err=<nil>; reopened 01a0f181-5921-7a20-9b7c-de1827279c85 from the checkpoint at offset 40179; 8 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["PING third" "TOOL pwd; cat saveload-note.txt"]; resets 0, faults 0 |
| `load.new-cwd` the resumed Session works in the new workspace | **pass** | a tool run after the restore printed "Chunk ID: 0eb55a\nWall time: 0.0000 seconds\nProcess exited with code 0\nOriginal token count: 20\nOutput:\n$WORK/restored/candidate/agent/ws\nt4858f5e4fe" |
| `load.workspace-file` a workspace file saved with the Session is read by a tool in the new workspace | info | saveload-note.txt restored; the tool's output holds nonce T: true (a smoke check, not workspace fidelity) |
| `load.aux-kept` the Session's name and goal are still there after two more turns and a second reopen | **pass** | saved goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; read back goal="Keep the save and load probe's notes m98e04f7bd4", goal_status="paused", name="saveload probe m98e04f7bd4"; err=<nil> |
| `load.no-rewrite` the restored record is appended to, its saved bytes unchanged: relocation moves paths, not contents | **pass** | the first 35111 bytes of the continued record hash to the saved sha256: true; it grew: true; relocation rules: 0 |
| `load.memory` the saved memory file is in the new memory directory | info | memory/notes.md restored with the saved content: true |
| `load.fresh-credential` the restored Session's requests carry the credential staged for the new environment | **pass** | 4 requests with the new environment's credential, 0 with the source's, 0 with none |
| `negative.harness-refuses` with no history restored, the harness alone refuses to resume the id | **pass** | the harness has no record of the session: thread/resume: codex: no rollout found for thread id 01a0f181-5921-7a20-9b7c-de1827279c85 (-32600) |
| `negative.empty-record` with no history restored, the record reader finds an empty record | info | 0 observations, checkpoint false, end of record true, err=<nil> — the sign a Load must refuse on, before any open |
| `negative.no-context` with no history restored, a reopen through the adapter fails or lacks the earlier context | **pass** | the adapter opened a Session 01a0f181-7ecf-7b02-91f2-3436d4446cfd (the saved id: false) — a fresh conversation, not an error; request 14 has 5 items; a nonce among them: false (err=<nil>); err=<nil> |
| `source.untouched` nothing was read from or written to the source while the restored Sessions ran | **pass** | the source's path is still absent: true; its 75 files, kept out of reach, are unchanged: true |

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
| `config/.personality_migration` | 1 | 3 | other |  |
| `config/AGENTS.md` | 1 | 278 | rendered |  |
| `config/config.toml` | 1 | 658 | rendered |  |
| `config/goals_1.sqlite` | 1 | 4096 | other | candidate, rollout+goals, rollout+goals-wal, rollout+goals-db |
| `config/goals_1.sqlite-shm` | 1 | 32768 | other | candidate, rollout+goals |
| `config/goals_1.sqlite-wal` | 1 | 61832 | other | candidate, rollout+goals, rollout+goals-wal |
| `config/installation_id` | 1 | 36 | other |  |
| `config/logs_2.sqlite` | 1 | 311296 | other |  |
| `config/logs_2.sqlite-shm` | 1 | 32768 | other |  |
| `config/logs_2.sqlite-wal` | 1 | 313152 | other |  |
| `config/memories_1.sqlite` | 1 | 4096 | other |  |
| `config/memories_1.sqlite-shm` | 1 | 32768 | other |  |
| `config/memories_1.sqlite-wal` | 1 | 65952 | other |  |
| `config/memory/notes.md` | 1 | 34 | history | candidate, history-roots |
| `config/session_index.jsonl` | 1 | 135 | other | candidate, rollout+index |
| `config/sessions/2026/09/30/rollout-2026-09-30T10-49-41-01a0f181-5921-7a20-9b7c-de1827279c85.jsonl` | 1 | 35111 | history | candidate, history-roots, rollout-only, rollout+index, rollout+goals, rollout+goals-wal, rollout+goals-db |
| `config/skills/**` | 50 | 374701 | other |  |
| `config/state_5.sqlite` | 1 | 4096 | other |  |
| `config/state_5.sqlite-shm` | 1 | 32768 | other |  |
| `config/state_5.sqlite-wal` | 1 | 2381392 | other |  |
| `scratch/markers/**` | 3 | 357 | other |  |
| `workspace/AGENTS.md` | 1 | 49 | rendered |  |
| `workspace/saveload-note.txt` | 1 | 12 | other | candidate |
