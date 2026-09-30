claude-code, mock: 2.1.283 (Claude Code) (pinned 2.1.283), harness-wrapper (devel), linux/arm64

| Check | Result | What was seen |
|---|---|---|
| `source.turns` two source turns: a reply with nonce A, a tool result with nonce T | **pass** | turn 1 completed, reply "PONG ab3d1aa89ab"; turn 2 completed, 1 tool call(s), output "t7bbcbbd73d" |
| `source.subagent` a third source turn hands a task to a subagent | info | turn completed; the subagent started 1 time(s) and stopped 1; the tool result holds its reply: true |
| `source.stopped` the source harness and its children stopped before anything is copied | **pass** | Close: stopped=true drained=true err=<nil>; processes still naming the source: 0 |
| `archive.standalone` the candidate state is copied to a standalone archive, each file with its size and hash | **pass** | 5 files in the history-roots archive; 2 variants saved |
| `source.inaccessible` the source's roots are gone before the restore starts | **pass** | the record's path: stat $WORK/source/profile/projects/-WORK-source-workspace/edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100.jsonl: no such file or directory; the kept copy: open $WORK/.gone-93efe93c4d: permission denied |
| `load.record-read` the adapter's record reader reaches the restored record's end, with nothing published | **pass** | 10 observations in 1 batch(es), end of record true; the saved prompt, reply, tool call and tool result read: true true true true; checkpoint at offset 39424 of the restored 39424 bytes; resets 0, rescans 0, faults 0; err=<nil> |
| `checkpoint.carried` the source's checkpoint, given to the restored record | info | the source's checkpoint (offset 39424, inode 34654) on the restored file: 1 reset(s) [replaced], then all 10 observations delivered again — a checkpoint names the file it was made on, so a Load seeds its own |
| `load.history` the first resumed model request carries the saved turns and the tool result | **pass** | request 7 carries the source's two turns — prompt, reply, tool call, tool result, reply — before the new prompt (20 items); the prompt names no nonce: true |
| `load.subagents` a subagent's record, saved beneath the Session's own, moves with it | info | 2 of 2 subagent file(s) restored with their saved content, at [projects/-WORK-restored-history-roots-agent-ws/edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100/subagents/agent-a079636ae59cb753a.jsonl projects/-WORK-restored-history-roots-agent-ws/edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100/subagents/agent-a079636ae59cb753a.meta.json]; the first resumed request carries the subagent's hand-back: true |
| `load.same-session` the Session reopens under its saved native id | **pass** | saved edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100, reopened edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100 |
| `load.new-events-only` after the reopen from the seeded checkpoint, only the new turn's record events are delivered | **pass** | 3 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["PING again"]; resets 0, faults 0 |
| `load.second-reopen` after a stop and a second reopen, what the restored Session added is kept, and only new record events follow | **pass** | request 8 carries the saved turns and the first restored turn; missing: []; err=<nil>; reopened edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100 from the checkpoint at offset 45914; 10 record observations; ids the restored record already held: 0; dated no later than its last entry: 0; user inputs: ["PING third" "TOOL pwd; cat saveload-note.txt"]; resets 0, faults 0 |
| `load.new-cwd` the resumed Session works in the new workspace | **pass** | a tool run after the restore printed "$WORK/restored/history-roots/agent/ws\nt7bbcbbd73d" |
| `load.workspace-file` a workspace file saved with the Session is read by a tool in the new workspace | info | saveload-note.txt restored; the tool's output holds nonce T: true (a smoke check, not workspace fidelity) |
| `load.no-rewrite` the restored record is appended to, its saved bytes unchanged: relocation moves paths, not contents | **pass** | the first 39424 bytes of the continued record hash to the saved sha256: true; it grew: true; relocation rules: 1 |
| `load.memory` the saved memory file is in the new memory directory | info | memory/notes.md restored with the saved content: true |
| `load.fresh-credential` the restored Session's requests carry the credential staged for the new environment | **pass** | 4 requests with the new environment's credential, 0 with the source's, 0 with none |
| `negative.harness-refuses` with no history restored, the harness alone refuses to resume the id | **pass** | claude --resume: No conversation found with session ID: edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100 |
| `negative.empty-record` with no history restored, the record reader finds an empty record | info | 0 observations, checkpoint false, end of record true, err=<nil> — the sign a Load must refuse on, before any open |
| `negative.no-context` with no history restored, a reopen through the adapter fails or lacks the earlier context | **pass** | the adapter opened a Session edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100 (the saved id: true) — a fresh conversation, not an error; request 12 has 4 items; a nonce among them: false (err=<nil>); err=<nil> |
| `source.untouched` nothing was read from or written to the source while the restored Sessions ran | **pass** | the source's path is still absent: true; its 15 files, kept out of reach, are unchanged: true |

| Saved state | Files | Conversation continued | Same Session id | Native state read back |
|---|---|---|---|---|
| `transcript-only`: the Session's transcript alone | 1 | true | true | none |

| Source file | Files | Bytes | Class | Saved by |
|---|---|---|---|---|
| `config/.claude.json` | 1 | 796 | rendered |  |
| `config/backups/**` | 1 | 273 | other |  |
| `config/memory/notes.md` | 1 | 34 | history | history-roots |
| `config/persona.md` | 1 | 92 | rendered |  |
| `config/projects/-WORK-source-workspace/edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100.jsonl` | 1 | 39424 | history | history-roots, transcript-only |
| `config/projects/-WORK-source-workspace/edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100/subagents/agent-a079636ae59cb753a.jsonl` | 1 | 19616 | history | history-roots |
| `config/projects/-WORK-source-workspace/edf6f87a-a0c4-4c1a-9f0a-3a6a45ea6100/subagents/agent-a079636ae59cb753a.meta.json` | 1 | 157 | history | history-roots |
| `config/settings.json` | 1 | 4117 | rendered |  |
| `scratch/markers/**` | 4 | 517 | other |  |
| `workspace/CLAUDE.md` | 1 | 49 | rendered |  |
| `workspace/saveload-note.txt` | 1 | 12 | other | history-roots |
