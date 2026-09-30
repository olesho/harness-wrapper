codex, live: codex-cli 0.144.5 (pinned 0.144.5), harness-wrapper (devel), darwin/arm64

| Check | Result | What was seen |
|---|---|---|
| `source.turns` two source turns: a reply with nonce A, a tool result with nonce T | **pass** | turn 1 completed, reply "PONGaf416f40ef7"; turn 2 completed, 1 tool call(s), output "Script completed\nWall time 0.1 seconds\nOutput:\ntb3cfad2c23" |
| `source.stopped` the source harness and its children stopped before anything is copied | **pass** | Close: stopped=true drained=true err=<nil>; processes still naming the source: 0 |
| `source.aux` the Session's name and goal are set, and read back, before the save | **pass** | goal="Keep the save and load probe's notes maa451586f1", goal_status="paused", name="saveload probe maa451586f1"; err=<nil>; processes left: 0 |
| `archive.standalone` the candidate state is copied to a standalone archive, each file with its size and hash | **pass** | 7 files in the candidate archive; 7 variants saved |
| `source.inaccessible` the source's roots are gone before the restore starts | **pass** | the record's path: stat $WORK/source/profile/sessions/2026/09/30/rollout-2026-09-30T10-48-03-01a0f17f-dc8c-7b92-92f5-398ff42ba104.jsonl: no such file or directory; the kept copy: open $WORK/.gone-2fc4aa75bf: permission denied |
| `load.record-read` the adapter's record reader reaches the restored record's end, with nothing published | **pass** | 6 observations in 1 batch(es), end of record true; the saved prompt, reply, tool call and tool result read: true true true true; checkpoint at offset 36848 of the restored 36848 bytes; resets 0, rescans 0, faults 0; err=<nil> |
| `checkpoint.carried` the source's checkpoint, given to the restored record | info | the source's checkpoint (offset 36476, inode 250737987) on the restored file: 1 reset(s) [replaced], then all 6 observations delivered again — a checkpoint names the file it was made on, so a Load seeds its own |
| `load.history` the live model recalls nonce A in the restored Session, without a tool | **pass** | the reply to "What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools." was "PONGaf416f40ef7", after 0 tool call(s); the prompt names no nonce: true |
| `load.same-session` the Session reopens under its saved native id | **pass** | saved 01a0f17f-dc8c-7b92-92f5-398ff42ba104, reopened 01a0f17f-dc8c-7b92-92f5-398ff42ba104 |
| `load.new-events-only` after the reopen from the seeded checkpoint, only the new turn's record events are delivered | **pass** | 3 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools."]; resets 0, faults 0 |
| `load.aux` the Session's name and goal are read back after the load | **pass** | saved goal="Keep the save and load probe's notes maa451586f1", goal_status="paused", name="saveload probe maa451586f1"; read back goal="Keep the save and load probe's notes maa451586f1", goal_status="paused", name="saveload probe maa451586f1"; err=<nil> |
| `load.second-reopen` after a stop and a second reopen, what the restored Session added is kept, and only new record events follow | **pass** | the reply to "Earlier in this conversation you ran a shell command for me. What did it print? Reply with that output only, and use no tools." was "tb3cfad2c23", after 0 tool call(s); reopened 01a0f17f-dc8c-7b92-92f5-398ff42ba104 from the checkpoint at offset 42867; 8 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["Earlier in this conversation you ran a shell command for me. What did it print? Reply with that output only, and use no tools." "Use your shell tool to run exactly this command: pwd — then reply with the command's output and nothing else."]; resets 0, faults 0 |
| `load.new-cwd` the resumed Session works in the new workspace | **pass** | a tool run after the restore printed "Script completed\nWall time 0.1 seconds\nOutput:\n$WORK/restored/candidate/agent/ws" |
| `load.aux-kept` the Session's name and goal are still there after two more turns and a second reopen | **pass** | saved goal="Keep the save and load probe's notes maa451586f1", goal_status="paused", name="saveload probe maa451586f1"; read back goal="Keep the save and load probe's notes maa451586f1", goal_status="paused", name="saveload probe maa451586f1"; err=<nil> |
| `load.no-rewrite` the restored record is appended to, its saved bytes unchanged: relocation moves paths, not contents | **pass** | the first 36848 bytes of the continued record hash to the saved sha256: true; it grew: true; relocation rules: 0 |
| `load.memory` the saved memory file is in the new memory directory | info | memory/notes.md restored with the saved content: true |
| `negative.harness-refuses` with no history restored, the harness alone refuses to resume the id | **pass** | the harness has no record of the session: thread/resume: codex: no rollout found for thread id 01a0f17f-dc8c-7b92-92f5-398ff42ba104 (-32600) |
| `negative.empty-record` with no history restored, the record reader finds an empty record | info | 0 observations, checkpoint false, end of record true, err=<nil> — the sign a Load must refuse on, before any open |
| `negative.no-context` with no history restored, a reopen through the adapter fails or lacks the earlier context | **pass** | the adapter opened a Session 01a0f180-73c4-7b53-8a9b-6e176c811bb8 (the saved id: false) — a fresh conversation, not an error; the reply to "What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools." was "None"; err=<nil> |
| `source.untouched` nothing was read from or written to the source while the restored Sessions ran | **pass** | the source's path is still absent: true; its 76 files, kept out of reach, are unchanged: true |

| Source file | Files | Bytes | Class | Saved by |
|---|---|---|---|---|
| `config/.personality_migration` | 1 | 3 | other |  |
| `config/AGENTS.md` | 1 | 326 | rendered |  |
| `config/auth.json` | 1 | 3912 | secret |  |
| `config/config.toml` | 1 | 463 | rendered |  |
| `config/goals_1.sqlite` | 1 | 4096 | other | candidate, rollout+goals, rollout+goals-wal, rollout+goals-db |
| `config/goals_1.sqlite-shm` | 1 | 32768 | other | candidate, rollout+goals |
| `config/goals_1.sqlite-wal` | 1 | 61832 | other | candidate, rollout+goals, rollout+goals-wal |
| `config/installation_id` | 1 | 36 | other |  |
| `config/logs_2.sqlite` | 1 | 176128 | other |  |
| `config/logs_2.sqlite-shm` | 1 | 32768 | other |  |
| `config/logs_2.sqlite-wal` | 1 | 399672 | other |  |
| `config/memories_1.sqlite` | 1 | 40960 | other |  |
| `config/memories_1.sqlite-shm` | 1 | 32768 | other |  |
| `config/memories_1.sqlite-wal` | 1 | 65952 | other |  |
| `config/memory/notes.md` | 1 | 34 | history | candidate, history-roots |
| `config/models_cache.json` | 1 | 245362 | other |  |
| `config/session_index.jsonl` | 1 | 132 | other | candidate, rollout+index |
| `config/sessions/2026/09/30/rollout-2026-09-30T10-48-03-01a0f17f-dc8c-7b92-92f5-398ff42ba104.jsonl` | 1 | 36848 | history | candidate, history-roots, rollout-only, rollout+index, rollout+goals, rollout+goals-wal, rollout+goals-db |
| `config/skills/**` | 50 | 374701 | other |  |
| `config/state_5.sqlite` | 1 | 4096 | other |  |
| `config/state_5.sqlite-shm` | 1 | 32768 | other |  |
| `config/state_5.sqlite-wal` | 1 | 2377272 | other |  |
| `scratch/markers/**` | 3 | 351 | other |  |
| `workspace/AGENTS.md` | 1 | 49 | rendered |  |
| `workspace/saveload-note.txt` | 1 | 12 | other | candidate |
