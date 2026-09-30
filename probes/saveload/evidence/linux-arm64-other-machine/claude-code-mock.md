claude-code, mock: 2.1.283 (Claude Code) (pinned 2.1.283), harness-wrapper (devel), linux/arm64

| Check | Result | What was seen |
|---|---|---|
| `source.elsewhere` the archive was saved on another machine, which this one cannot reach into | **pass** | saved on lima-agentd-ubuntu (linux/arm64, 2.1.283 (Claude Code)); loaded on lima-agentd-debian (linux/arm64) |
| `load.record-read` the adapter's record reader reaches the restored record's end, with nothing published | **pass** | 10 observations in 1 batch(es), end of record true; the saved prompt, reply, tool call and tool result read: true true true true; checkpoint at offset 40694 of the restored 40694 bytes; resets 0, rescans 0, faults 0; err=<nil> |
| `checkpoint.carried` the source's checkpoint, given to the restored record | info | the source's checkpoint (offset 40694, inode 36229) on the restored file: 1 reset(s) [replaced], then all 10 observations delivered again — a checkpoint names the file it was made on, so a Load seeds its own |
| `load.history` the first resumed model request carries the saved turns and the tool result | **pass** | request 1 carries the source's two turns — prompt, reply, tool call, tool result, reply — before the new prompt (20 items); the prompt names no nonce: true |
| `load.subagents` a subagent's record, saved beneath the Session's own, moves with it | info | 2 of 2 subagent file(s) restored with their saved content, at [projects/-WORK-restored-history-roots-agent-ws/0ec085bb-4fe6-4078-86ad-4f34c397702a/subagents/agent-a273fa2e635520125.jsonl projects/-WORK-restored-history-roots-agent-ws/0ec085bb-4fe6-4078-86ad-4f34c397702a/subagents/agent-a273fa2e635520125.meta.json]; the first resumed request carries the subagent's hand-back: true |
| `load.same-session` the Session reopens under its saved native id | **pass** | saved 0ec085bb-4fe6-4078-86ad-4f34c397702a, reopened 0ec085bb-4fe6-4078-86ad-4f34c397702a |
| `load.new-events-only` after the reopen from the seeded checkpoint, only the new turn's record events are delivered | **pass** | 3 record observations; ids the restored record already held: 0; dates not compared: the record was saved under another machine's clock; user inputs: ["PING again"]; resets 0, faults 0 |
| `load.second-reopen` after a stop and a second reopen, what the restored Session added is kept, and only new record events follow | **pass** | request 2 carries the saved turns and the first restored turn; missing: []; err=<nil>; reopened 0ec085bb-4fe6-4078-86ad-4f34c397702a from the checkpoint at offset 47197; 10 record observations; ids the restored record already held: 0; dates not compared: the record was saved under another machine's clock; user inputs: ["PING third" "TOOL pwd; cat saveload-note.txt"]; resets 0, faults 0 |
| `load.new-cwd` the resumed Session works in the new workspace | **pass** | a tool run after the restore printed "$WORK/restored/history-roots/agent/ws\nt6ed5a5b13e" |
| `load.workspace-file` a workspace file saved with the Session is read by a tool in the new workspace | info | saveload-note.txt restored; the tool's output holds nonce T: true (a smoke check, not workspace fidelity) |
| `load.no-rewrite` the restored record is appended to, its saved bytes unchanged: relocation moves paths, not contents | **pass** | the first 40694 bytes of the continued record hash to the saved sha256: true; it grew: true; relocation rules: 1 |
| `load.memory` the saved memory file is in the new memory directory | info | memory/notes.md restored with the saved content: true |
| `load.fresh-credential` the restored Session's requests carry the credential staged for the new environment | **pass** | 4 requests with the new environment's credential, 0 with the source's, 0 with none |
| `negative.harness-refuses` with no history restored, the harness alone refuses to resume the id | **pass** | claude --resume: No conversation found with session ID: 0ec085bb-4fe6-4078-86ad-4f34c397702a |
| `negative.empty-record` with no history restored, the record reader finds an empty record | info | 0 observations, checkpoint false, end of record true, err=<nil> — the sign a Load must refuse on, before any open |
| `negative.no-context` with no history restored, a reopen through the adapter fails or lacks the earlier context | **pass** | the adapter opened a Session 0ec085bb-4fe6-4078-86ad-4f34c397702a (the saved id: true) — a fresh conversation, not an error; request 6 has 4 items; a nonce among them: false (err=<nil>); err=<nil> |

| Saved state | Files | Conversation continued | Same Session id | Native state read back |
|---|---|---|---|---|
| `transcript-only`: the Session's transcript alone | 1 | true | true | none |

| Source file | Files | Bytes | Class | Saved by |
|---|---|---|---|---|
