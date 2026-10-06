# codex 0.160.0: 8 Sessions side by side (mock)

linux-arm64, turns for 30m0s; the run took 30m4s. Peak memory of every app-server's tree together: 3051 MiB; each at its peak: 378 MiB, 368 MiB, 350 MiB, 346 MiB, 345 MiB, 336 MiB, 333 MiB, 332 MiB, 314 MiB; of the probe's own process, which hosts every Session's adapter and the mock: 256 MiB.

| Criterion | Result | Detail |
|---|---|---|
| every app-server starts behind the start lock, and the failure of #50290 shows without it | pass | behind the lock: 0 of 9 failed to start (the slowest open took 374ms); on a fresh home, all at once: 7 of 8 failed.  |
| no turn fails on a busy database | pass | 13679 input turns, 0 failed, 0 of them on a database; after 3415 goals codex worked in turns of its own within the wait, but for 0 |
| session_index.jsonl, when codex writes it, parses with an entry per thread | pass | codex wrote none: it writes the file when a thread is named, and no Session's thread was.  |
| every rollout is whole | pass | 9 rollouts read.  |
| codex's databases pass SQLite's integrity check | pass | 6 databases.  |

| Session | Open | Turns completed / sent | Tool uses | Batches (peak a second) |
|---|---|---|---|---|
| control | 59 ms | 1883 / 1883 | 759 | 8769 (27) |
| s0 | 327 ms | 1873 / 1873 | 755 | 8729 (26) |
| s1 | 347 ms | 1903 / 1903 | 765 | 8838 (24) |
| s2 | 352 ms | 1911 / 1911 | 770 | 8879 (26) |
| s3 | 355 ms | 1905 / 1905 | 764 | 8845 (24) |
| s4 | 373 ms | 1908 / 1908 | 768 | 8871 (29) |
| s5 | 341 ms | 1878 / 1878 | 753 | 8725 (26) |
| s6 | 358 ms | 1930 / 1930 | 776 | 8955 (27) |
| s7 | 324 ms | 1903 / 1903 | 765 | 8845 (25) |

On a fresh home, all at once:
- fresh1: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-145137/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-145137/fresh/config
- fresh2: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-145137/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-145137/fresh/config
- fresh3: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-145137/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-145137/fresh/config
- fresh4: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-145137/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-145137/fresh/config
- fresh5: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-145137/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-145137/fresh/config
- fresh6: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-145137/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-145137/fresh/config
- fresh7: Open: open_failed (config_invalid): initialize: codex exited: Error: failed to initialize sqlite state runtime under /tmp/sp/work/20261006-145137/fresh/config: failed to initialize state runtime at /tmp/sp/work/20261006-145137/fresh/config

session_index.jsonl: codex wrote none: it writes the file when a thread is named, and no Session's thread was. Databases: goals_1.sqlite ok (0.0 MiB, wal 3.9 MiB), logs_2.sqlite ok (21.6 MiB, wal 7.5 MiB), memories_1.sqlite ok (0.0 MiB, wal 0.0 MiB), queue_1.sqlite ok (0.0 MiB, wal 0.0 MiB), state_5.sqlite ok (0.2 MiB, wal 4.1 MiB), thread_history_1.sqlite ok (35.4 MiB, wal 0.0 MiB).
- s0: waited 1.86s for idle
- s4: waited 1.06s for idle
