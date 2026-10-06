# claude-code 2.1.283: 4 Sessions side by side (live)

linux-arm64, turns for 5m0s; the run took 5m6s. Peak memory of every Session's harness tree together: 1600 MiB (154 samples); of the probe's own process, which hosts every Session's adapter and the mock: 18 MiB.

| Criterion | Result | Detail |
|---|---|---|
| no start hangs | pass | a control Session alone, 4 at once, one at the end; the slowest open took 524ms.  |
| .claude.json parses after every write and keeps what was rendered | pass | 1 states read: 0 torn, 0 corrupt, 0 missing a rendered entry, 0 reads found no file |
| every hook event lands in its own Session's spool | pass | 1736 hook payloads: 0 in another Session's spool; 0 tool events a Session reported for another's tool use; 0 tool uses with no hook event |
| every transcript parses and holds only its own Session | pass | 6 transcripts read.  |
| a connector's headersHelper still runs at the end | pass | the Session opened last sent the header from its file: true; 30 of the MCP server's 30 requests carried it |
| every turn completed | **fail** | 412 input turns, 1 failed; after 81 background commands claude took a turn of its own within the wait, but for 0 |

| Session | Open | Turns completed / sent | Own turns | Tool uses / started / finished | Subagents | Peak memory | Batches (peak a second) |
|---|---|---|---|---|---|---|---|
| control | 524 ms | 1 / 1 | 0 | 0 / 0 / 0 | 0 | 0 MiB | 5 (4) |
| s0 | 318 ms | 129 / 129 | 21 | 65 / 65 / 65 | 22 | 368 MiB | 530 (6) |
| s1 | 293 ms | 120 / 120 | 20 | 60 / 60 / 60 | 20 | 365 MiB | 496 (6) |
| s2 | 312 ms | 120 / 120 | 20 | 60 / 60 / 60 | 20 | 350 MiB | 495 (5) |
| s3 | 334 ms | 121 / 122 | 20 | 61 / 61 / 61 | 20 | 359 MiB | 502 (7) |
| final | 202 ms | 1 / 1 | 0 | 0 / 0 / 0 | 0 | 266 MiB | 5 (3) |

.claude.json: 1 states read, 0 torn, 0 corrupt, 0 missing a rendered entry. What claude changed of it alone, in the control Session, is not held against the run:
- alone: bypassPermissionsModeAccepted: rendered true, then <nil>
- alone: projects › /tmp/TestClaudeSessionsLive2431209988/001/env/workspace › hasCompletedProjectOnboarding: rendered true, then <nil>

Hook payloads by event: PostToolUse 328, PreToolUse 246, SessionEnd 6, SessionStart 6, Stop 493, SubagentStart 82, SubagentStop 82, UserPromptSubmit 493.
- s1: waited 4.91s for idle
- s3 AGENT failed: reply "", want "SUB-s3-62"
