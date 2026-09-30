claude-code, live: 2.1.283 (Claude Code) (pinned 2.1.283), harness-wrapper (devel), darwin/arm64

| Check | Result | What was seen |
|---|---|---|
| `source.turns` two source turns: a reply with nonce A, a tool result with nonce T | **pass** | turn 1 completed, reply "PONGaa3946efe75"; turn 2 completed, 1 tool call(s), output "ta99949235b" |
| `source.stopped` the source harness and its children stopped before anything is copied | **pass** | Close: stopped=true drained=true err=<nil>; processes still naming the source: 0 |
| `archive.standalone` the candidate state is copied to a standalone archive, each file with its size and hash | **pass** | 3 files in the history-roots archive; 2 variants saved |
| `source.inaccessible` the source's roots are gone before the restore starts | **pass** | the record's path: stat $WORK/source/profile/projects/-WORK-source-workspace/fa5a7e33-338f-4873-89c2-7192a2e0ed66.jsonl: no such file or directory; the kept copy: open $WORK/.gone-49c0296e47: permission denied |
| `load.record-read` the adapter's record reader reaches the restored record's end, with nothing published | **pass** | 6 observations in 1 batch(es), end of record true; the saved prompt, reply, tool call and tool result read: true true true true; checkpoint at offset 42215 of the restored 42215 bytes; resets 0, rescans 0, faults 0; err=<nil> |
| `checkpoint.carried` the source's checkpoint, given to the restored record | info | the source's checkpoint (offset 42215, inode 250713751) on the restored file: 1 reset(s) [replaced], then all 6 observations delivered again — a checkpoint names the file it was made on, so a Load seeds its own |
| `load.history` the live model recalls nonce A in the restored Session, without a tool | **pass** | the reply to "What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools." was "PONGaa3946efe75", after 0 tool call(s); the prompt names no nonce: true |
| `load.same-session` the Session reopens under its saved native id | **pass** | saved fa5a7e33-338f-4873-89c2-7192a2e0ed66, reopened fa5a7e33-338f-4873-89c2-7192a2e0ed66 |
| `load.new-events-only` after the reopen from the seeded checkpoint, only the new turn's record events are delivered | **pass** | 3 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools."]; resets 0, faults 0 |
| `load.second-reopen` after a stop and a second reopen, what the restored Session added is kept, and only new record events follow | **pass** | the reply to "Earlier in this conversation you ran a shell command for me. What did it print? Reply with that output only, and use no tools." was "ta99949235b", after 0 tool call(s); reopened fa5a7e33-338f-4873-89c2-7192a2e0ed66 from the checkpoint at offset 52422; 10 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["Earlier in this conversation you ran a shell command for me. What did it print? Reply with that output only, and use no tools." "Use the Bash tool to run exactly this command: pwd — then reply with the command's output and nothing else."]; resets 0, faults 0 |
| `load.new-cwd` the resumed Session works in the new workspace | **pass** | a tool run after the restore printed "$WORK/restored/history-roots/agent/ws" |
| `load.no-rewrite` the restored record is appended to, its saved bytes unchanged: relocation moves paths, not contents | **pass** | the first 42215 bytes of the continued record hash to the saved sha256: true; it grew: true; relocation rules: 1 |
| `load.memory` the saved memory file is in the new memory directory | info | memory/notes.md restored with the saved content: true |
| `negative.harness-refuses` with no history restored, the harness alone refuses to resume the id | **pass** | claude --resume: No conversation found with session ID: fa5a7e33-338f-4873-89c2-7192a2e0ed66 |
| `negative.empty-record` with no history restored, the record reader finds an empty record | info | 0 observations, checkpoint false, end of record true, err=<nil> — the sign a Load must refuse on, before any open |
| `negative.no-context` with no history restored, a reopen through the adapter fails or lacks the earlier context | **pass** | the adapter opened a Session fa5a7e33-338f-4873-89c2-7192a2e0ed66 (the saved id: true) — a fresh conversation, not an error; the reply to "What exact word did you reply with in your very first answer in this conversation? Reply with that word only, and use no tools." was "This is the first message you've sent me in this conversation, so there is no previous answer to reference."; err=<nil> |
| `source.untouched` nothing was read from or written to the source while the restored Sessions ran | **pass** | the source's path is still absent: true; its 16 files, kept out of reach, are unchanged: true |

| Source file | Files | Bytes | Class | Saved by |
|---|---|---|---|---|
| `config/.claude.json` | 1 | 861 | rendered |  |
| `config/.last-cleanup` | 1 | 24 | other |  |
| `config/backups/**` | 1 | 322 | other |  |
| `config/memory/notes.md` | 1 | 34 | history | history-roots |
| `config/persona.md` | 1 | 92 | rendered |  |
| `config/policy-limits.json` | 1 | 214 | other |  |
| `config/policy-limits.json.stamp.json` | 1 | 225 | other |  |
| `config/projects/-WORK-source-workspace/fa5a7e33-338f-4873-89c2-7192a2e0ed66.jsonl` | 1 | 42215 | history | history-roots, transcript-only |
| `config/remote-settings.json` | 1 | 2 | other |  |
| `config/settings.json` | 1 | 4656 | rendered |  |
| `scratch/markers/**` | 3 | 351 | other |  |
| `workspace/CLAUDE.md` | 1 | 49 | rendered |  |
| `workspace/saveload-note.txt` | 1 | 12 | other | history-roots |
