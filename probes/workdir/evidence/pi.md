# pi 1.0.4, model openrouter/anthropic/claude-haiku-4.5

git version 2.50.1 (Apple Git-155); worktree Sessions side by side: false. 0 of 19 steps failed.

| Step | OK | Took | Detail |
|---|---|---|---|
| root: reads the workspace's instructions | yes | 4.5s | <nil> "MANGO-83BFB4" |
| wt/a: opens | yes | 200ms | <nil> |
| wt/a: reads the workspace's instructions | yes | 3.7s | <nil> "MANGO-83BFB4" (the instructions file is at the workspace's root, two directories above) |
| wt/a: works there | yes | 4.2s | <nil> pwd "/private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestPiWorkDirsLive604173513/002/env1/workspace/wt/a", want /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestPiWorkDirsLive604173513/002/env1/workspace/wt/a |
| wt/b: opens | yes | 300ms | <nil> |
| wt/b: reads the workspace's instructions | yes | 3.9s | <nil> "MANGO-83BFB4" (the instructions file is at the workspace's root, two directories above) |
| wt/b: works there | yes | 3.5s | <nil> pwd "/private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestPiWorkDirsLive604173513/002/env1/workspace/wt/b", want /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestPiWorkDirsLive604173513/002/env1/workspace/wt/b |
| wt/a: its commit on its branch, alone | yes |  | log "b29e6b2 a: add file", files "README.md a.txt" |
| wt/b: its commit on its branch, alone | yes |  | log "d588dda b: add file", files "README.md b.txt" |
| app: the repository's own checkout untouched | yes |  | files "README.md" |
| wt/b: its worktree removed once it ended, its branch kept | yes |  | <nil> "", branch "b" |
| wt/a: reopens | yes | 200ms | <nil> |
| wt/a: remembers after the reopen | yes | 3s | <nil> "a.txt" |
| wt/a: goes on after wt/b's removal | yes | 5.3s | <nil> "## a" |
| load: a copied worktree's links name the source | yes |  | git status before a repair: exit status 128 "fatal: not a git repository: /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestPiWorkDirsLive604173513/002/env1/workspace/app/.git/worktrees/app" |
| load: git worktree repair links it again | yes |  | repair: <nil> "repair: gitdir incorrect: /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestPiWorkDirsLive604173513/002/env2-elsewhere/workspace/app/.git/worktrees/app/gitdir"; log <nil> "b29e6b2 a: add file" |
| load: wt/a's record read where it was moved | yes |  | 16 items, <nil> |
| load: wt/a's Session opens loaded, in the new wt/a | yes | 300ms | <nil> |
| load: remembers, works in the new wt/a, and git works there | yes | 4.2s | <nil> "a.txt /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestPiWorkDirsLive604173513/002/env2-elsewhere/workspace/wt/a b29e6b2 a: add file" |
