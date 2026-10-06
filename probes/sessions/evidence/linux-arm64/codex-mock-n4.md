# codex 0.160.0: 4 Sessions side by side (mock)

linux-arm64, turns for 30m0s; the run took 30m11s. Peak memory of every app-server's tree together: 1625 MiB; each at its peak: 352 MiB, 332 MiB, 325 MiB, 323 MiB, 309 MiB, 192 MiB, 190 MiB, 187 MiB, 71 MiB; of the probe's own process, which hosts every Session's adapter and the mock: 1460 MiB.

| Criterion | Result | Detail |
|---|---|---|
| every app-server starts behind the start lock, and the failure of #50290 shows without it | pass | behind the lock: 0 of 5 failed to start (the slowest open took 1.714s); on a fresh home, all at once: 1 of 4 failed.  |
| no turn fails on a busy database | pass | 4998 input turns, 0 failed, 0 of them on a database; after 1247 goals codex worked in turns of its own within the wait, but for 0 |
| session_index.jsonl, when codex writes it, parses with an entry per thread | pass | codex wrote none: it writes the file when a thread is named, and no Session's thread was.  |
| every rollout is whole | pass | 5 rollouts read.  |
| codex's databases pass SQLite's integrity check | pass | 6 databases.  |

| Session | Open | Turns completed / sent | Tool uses | Batches (peak a second) |
|---|---|---|---|---|
| control | 226 ms | 1248 / 1248 | 504 | 6760 (24) |
| s0 | 1713 ms | 1245 / 1245 | 500 | 6781 (24) |
| s1 | 1645 ms | 1238 / 1238 | 497 | 6755 (24) |
| s2 | 1572 ms | 1261 / 1261 | 505 | 6861 (25) |
| s3 | 1645 ms | 1253 / 1253 | 504 | 6790 (25) |

On a fresh home, all at once:
- fresh0: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-141204/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-141204/fresh/config

session_index.jsonl: codex wrote none: it writes the file when a thread is named, and no Session's thread was. Databases: goals_1.sqlite ok (0.0 MiB, wal 3.9 MiB), logs_2.sqlite ok (12.3 MiB, wal 6.8 MiB), memories_1.sqlite ok (0.0 MiB, wal 0.0 MiB), queue_1.sqlite ok (0.0 MiB, wal 0.1 MiB), state_5.sqlite ok (0.2 MiB, wal 4.1 MiB), thread_history_1.sqlite ok (13.1 MiB, wal 0.0 MiB).
- control: waited 1.05s for idle
- s1: waited 1.5s for idle
- s1: waited 1.02s for idle
- s3: waited 1.4s for idle
- s3: waited 3.11s for idle
