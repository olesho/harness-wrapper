# harness-wrapper

This project is managed by [harness](https://github.com/olesho/harness). The
`harness` binary is installed on your PATH (pinned in `.harness-version`); this
project commits no tool source, only declarative wiring that delegates to it.

## Working here

- Lint / format / test a project: `harness lint`, `harness fmt`, `harness test`.
- Regenerate structural docs: `harness docs markdown` (auto-runs on Stop).
- Regenerate architecture diagrams: `harness docs diagrams <project>` (manual).
- Check the project matches its lock: `harness verify`.
- Add a project (monorepo): `harness add`. Toggle features: `harness edit <name>`.

Agent hooks lint edited files in-loop; the git hooks and CI enforce the same
`harness` logic. Do not hand-edit files under `hooks/` or `.claude/` — they are
managed by harness and will be reconciled. Your source and native lint config
are yours to edit freely.
