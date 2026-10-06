# claude-code 2.1.283: 4 Sessions side by side (mock)

linux-arm64, turns for 30m0s; the run took 30m31s. Peak memory of every Session's harness tree together: 1877 MiB (914 samples).

| Criterion | Result | Detail |
|---|---|---|
| no start hangs | pass | a control Session alone, 4 at once, one at the end; the slowest open took 4.707s.  |
| .claude.json parses after every write and keeps what was rendered | pass | 1 states read: 0 torn, 0 corrupt, 0 missing a rendered entry, 0 reads found no file |
| every hook event lands in its own Session's spool | pass | 12707 hook payloads: 0 in another Session's spool; 0 tool events a Session reported for another's tool use; 0 tool uses with no hook event |
| every transcript parses and holds only its own Session | pass | 6 transcripts read.  |
| a connector's headersHelper still runs at the end | pass | the Session opened last sent the header from its file: true; 30 of the MCP server's 30 requests carried it |
| every turn completed | **fail** | 3628 turns, 1 failed |

| Session | Open | Turns completed / sent | Own turns | Tool uses / started / finished | Subagents | Peak memory |
|---|---|---|---|---|---|---|
| control | 338 ms | 1 / 1 | 0 | 0 / 0 / 0 | 0 | 0 MiB |
| s0 | 416 ms | 908 / 908 | 151 | 454 / 454 / 454 | 151 | 468 MiB |
| s1 | 294 ms | 917 / 917 | 153 | 459 / 459 / 459 | 153 | 490 MiB |
| s2 | 372 ms | 908 / 908 | 151 | 454 / 454 / 454 | 151 | 462 MiB |
| s3 | 327 ms | 892 / 893 | 148 | 447 / 447 / 447 | 149 | 460 MiB |
| final | 4706 ms | 1 / 1 | 0 | 0 / 0 / 0 | 0 | 231 MiB |

.claude.json: 1 states read, 0 torn, 0 corrupt, 0 missing a rendered entry. What claude changed of it alone, in the control Session, is not held against the run:
- alone: bypassPermissionsModeAccepted: rendered true, then <nil>
- alone: projects › /tmp/sp/work/20261006-133946/env/workspace › hasCompletedProjectOnboarding: rendered true, then <nil>

Hook payloads by event: PostToolUse 2418, PreToolUse 1814, SessionEnd 6, SessionStart 6, Stop 3627, SubagentStart 604, SubagentStop 604, UserPromptSubmit 3628.
- s0: waited 1.02s for idle
- s0: waited 1.82s for idle
- s0: waited 1.76s for idle
- s0: waited 7.44s for idle
- s0: waited 1.14s for idle
- s0: waited 1.02s for idle
- s0: waited 1.29s for idle
- s0: waited 1.12s for idle
- s0: waited 1.35s for idle
- s0: waited 1.69s for idle
- s0: waited 5.88s for idle
- s0: waited 1.48s for idle
- s0: waited 1.51s for idle
- s0: waited 1.64s for idle
- s0: waited 1.1s for idle
- s0: waited 1.14s for idle
- s0: waited 3.11s for idle
- s0: waited 4.35s for idle
- s0: waited 1.18s for idle
- s0: waited 1.86s for idle
- s0: waited 3.65s for idle
- s0: waited 2.55s for idle
- s0: waited 4.18s for idle
- s0: waited 1.31s for idle
- s0: waited 1.51s for idle
- s0: waited 4.62s for idle
- s0: waited 1.23s for idle
- s0: waited 1.84s for idle
- s0: waited 1.14s for idle
- s0: waited 1.82s for idle
- s0: waited 1.54s for idle
- s1: waited 1.14s for idle
- s1: waited 1.15s for idle
- s1: waited 1.93s for idle
- s1: waited 7.34s for idle
- s1: waited 3.6s for idle
- s1: waited 1.45s for idle
- s1: waited 1.09s for idle
- s1: waited 1.33s for idle
- s1: waited 1.34s for idle
- s1: waited 1.55s for idle
- s1: waited 1.18s for idle
- s1: waited 1.65s for idle
- s1: waited 4.69s for idle
- s1: waited 1.05s for idle
- s1: waited 1.12s for idle
- s1: waited 2.84s for idle
- s1: waited 2.46s for idle
- s1: waited 1.51s for idle
- s1: waited 1.93s for idle
- s1: waited 2.66s for idle
- s1: waited 1.38s for idle
- s1: waited 1.76s for idle
- s1: waited 2.32s for idle
- s1: waited 3.43s for idle
- s1: waited 2.06s for idle
- s1: waited 1.65s for idle
- s1: waited 3.64s for idle
- s1: waited 1.37s for idle
- s1: waited 1.17s for idle
- s1: waited 2.94s for idle
- s1: waited 1.15s for idle
- s1: waited 1.14s for idle
- s1: waited 1.64s for idle
- s1: waited 1.07s for idle
- s2: waited 1.46s for idle
- s2: waited 1.07s for idle
- s2: waited 2.72s for idle
- s2: waited 6.6s for idle
- s2: waited 2.55s for idle
- s2: waited 1.33s for idle
- s2: waited 1.55s for idle
- s2: waited 1.43s for idle
- s2: waited 1.38s for idle
- s2: waited 1.91s for idle
- s2: waited 1.03s for idle
- s2: waited 2.36s for idle
- s2: waited 3.1s for idle
- s2: waited 4.35s for idle
- s2: waited 1.54s for idle
- s2: waited 1.44s for idle
- s2: waited 3.42s for idle
- s2: waited 1.6s for idle
- s2: waited 3.78s for idle
- s2: waited 1.04s for idle
- s2: waited 1.78s for idle
- s2: waited 1.88s for idle
- s2: waited 4.8s for idle
- s2: waited 2.01s for idle
- s2: waited 5.01s for idle
- s2: waited 2.39s for idle
- s2: waited 5.4s for idle
- s2: waited 1.08s for idle
- s2: waited 1.17s for idle
- s2: waited 1.39s for idle
- s2: waited 3.35s for idle
- s2: waited 1.44s for idle
- s3: waited 1.09s for idle
- s3: waited 2.54s for idle
- s3: waited 1.03s for idle
- s3: waited 5.93s for idle
- s3: waited 2.61s for idle
- s3: waited 1.8s for idle
- s3: waited 1.07s for idle
- s3: waited 1.33s for idle
- s3: waited 1.96s for idle
- s3: waited 1.82s for idle
- s3: waited 4.78s for idle
- s3: waited 1.28s for idle
- s3: waited 1.04s for idle
- s3: waited 3.07s for idle
- s3: waited 2.36s for idle
- s3: waited 2.28s for idle
- s3: waited 1.5s for idle
- s3: waited 2.4s for idle
- s3: waited 2.55s for idle
- s3: waited 3.11s for idle
- s3: waited 2.25s for idle
- s3: waited 2.48s for idle
- s3: waited 1.23s for idle
- s3: waited 2.85s for idle
- s3: waited 1.52s for idle
- s3: waited 3.3s for idle
- s3: waited 1.51s for idle
- s3: waited 2.05s for idle
- s3: waited 1.72s for idle
- s3: waited 1.28s for idle
- s3: waited 1.54s for idle
- s3 BG own turn failed: the turn did not end in time
