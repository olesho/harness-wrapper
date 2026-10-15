# codex 0.160.0, model the harness's default

git version 2.50.1 (Apple Git-155); worktree Sessions side by side: true. 0 of 19 steps failed.

| Step | OK | Took | Detail |
|---|---|---|---|
| root: reads the workspace's instructions | yes | 3.3s | <nil> "MANGO-A374F6" |
| wt/a: opens | yes | 100ms | <nil> |
| wt/b: opens | yes | 100ms | <nil> |
| wt/a: reads the workspace's instructions | yes | 3.9s | <nil> "MANGO-A374F6" (the instructions file is at the workspace's root, two directories above) |
| wt/a: works there | yes | 5.1s | <nil> pwd "/private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestCodexWorkDirsLive521942341/002/env1/workspace/wt/a", want /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestCodexWorkDirsLive521942341/002/env1/workspace/wt/a |
| wt/b: reads the workspace's instructions | yes | 4.3s | <nil> "MANGO-A374F6" (the instructions file is at the workspace's root, two directories above) |
| wt/b: works there | yes | 8s | <nil> pwd "/private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestCodexWorkDirsLive521942341/002/env1/workspace/wt/b", want /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestCodexWorkDirsLive521942341/002/env1/workspace/wt/b |
| wt/a: its commit on its branch, alone | yes |  | log "bd04762 a: add file", files "README.md a.txt" |
| wt/b: its commit on its branch, alone | yes |  | log "c411218 b: add file", files "README.md b.txt" |
| app: the repository's own checkout untouched | yes |  | files "README.md" |
| wt/b: its worktree removed once it ended, its branch kept | yes |  | <nil> "", branch "b" |
| wt/a: goes on beside the removal | yes | 4s | <nil> "## a" |
| wt/a: reopens | yes | 100ms | <nil> |
| wt/a: remembers after the reopen | yes | 3.6s | <nil> "a.txt" |
| load: a copied worktree's links name the source | yes |  | git status before a repair: exit status 128 "fatal: not a git repository: /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestCodexWorkDirsLive521942341/002/env1/workspace/app/.git/worktrees/app" |
| load: git worktree repair links it again | yes |  | repair: <nil> "repair: gitdir incorrect: /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestCodexWorkDirsLive521942341/002/env2-elsewhere/workspace/app/.git/worktrees/app/gitdir"; log <nil> "bd04762 a: add file" |
| load: wt/a's record read where it was moved | yes |  | 12 items, <nil> |
| load: wt/a's Session opens loaded, in the new wt/a | yes | 1.3s | <nil> |
| load: remembers, works in the new wt/a, and git works there | yes | 7s | <nil> "a.txt /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestCodexWorkDirsLive521942341/002/env2-elsewhere/workspace/wt/a bd04762 a: add file" |
