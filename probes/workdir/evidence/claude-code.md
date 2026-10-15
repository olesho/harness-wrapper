# claude-code 2.1.296, model haiku

git version 2.50.1 (Apple Git-155); worktree Sessions side by side: true. 0 of 19 steps failed.

| Step | OK | Took | Detail |
|---|---|---|---|
| root: reads the workspace's instructions | yes | 1.4s | <nil> "MANGO-2A419B" |
| wt/a: opens | yes | 200ms | <nil> |
| wt/b: opens | yes | 200ms | <nil> |
| wt/a: reads the workspace's instructions | yes | 1.1s | <nil> "MANGO-2A419B" (the instructions file is at the workspace's root, two directories above) |
| wt/a: works there | yes | 3.5s | <nil> pwd "/private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestClaudeWorkDirsLive1177221906/002/env1/workspace/wt/a", want /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestClaudeWorkDirsLive1177221906/002/env1/workspace/wt/a |
| wt/b: reads the workspace's instructions | yes | 1.6s | <nil> "MANGO-2A419B" (the instructions file is at the workspace's root, two directories above) |
| wt/b: works there | yes | 6.8s | <nil> pwd "/private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestClaudeWorkDirsLive1177221906/002/env1/workspace/wt/b", want /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestClaudeWorkDirsLive1177221906/002/env1/workspace/wt/b |
| wt/a: its commit on its branch, alone | yes |  | log "3c450d5 a: add file", files "README.md a.txt" |
| wt/b: its commit on its branch, alone | yes |  | log "eaa858f b: add file", files "README.md b.txt" |
| app: the repository's own checkout untouched | yes |  | files "README.md" |
| wt/b: its worktree removed once it ended, its branch kept | yes |  | <nil> "", branch "b" |
| wt/a: goes on beside the removal | yes | 2.2s | <nil> "## a" |
| wt/a: reopens | yes | 300ms | <nil> |
| wt/a: remembers after the reopen | yes | 1.2s | <nil> "a.txt" |
| load: a copied worktree's links name the source | yes |  | git status before a repair: exit status 128 "fatal: not a git repository: /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestClaudeWorkDirsLive1177221906/002/env1/workspace/app/.git/worktrees/app" |
| load: git worktree repair links it again | yes |  | repair: <nil> "repair: gitdir incorrect: /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestClaudeWorkDirsLive1177221906/002/env2-elsewhere/workspace/app/.git/worktrees/app/gitdir"; log <nil> "3c450d5 a: add file" |
| load: wt/a's record read where it was moved | yes |  | 12 items, <nil> |
| load: wt/a's Session opens loaded, in the new wt/a | yes | 700ms | <nil> |
| load: remembers, works in the new wt/a, and git works there | yes | 4s | <nil> "a.txt /private/var/folders/_p/kgsjzrc91g57sggc0v8gfnch0000gn/T/TestClaudeWorkDirsLive1177221906/002/env2-elsewhere/workspace/wt/a 3c450d5 a: add file" |
