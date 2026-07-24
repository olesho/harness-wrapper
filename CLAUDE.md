# harness-wrapper

This project is managed by [veracity](https://github.com/olesho/veracity). The
`veracity` binary is installed on your PATH (pinned in `.veracity-version`); this
project commits no tool source, only declarative wiring that delegates to it.

## Working here

- Lint / format / test a project: `veracity lint`, `veracity fmt`, `veracity test`.
- Regenerate structural docs: `veracity docs markdown` (auto-runs on Stop).
- Regenerate architecture diagrams: `veracity docs diagrams <project>` (manual).
- Check the project matches its lock: `veracity verify`.
- Add a project (monorepo): `veracity add`. Toggle features: `veracity edit <name>`.

Agent hooks lint edited files in-loop; the git hooks and CI enforce the same
`veracity` logic. Do not hand-edit files under `hooks/` or `.claude/` — they are
managed by veracity and will be reconciled. Your source and native lint config
are yours to edit freely.
