# harness-wrapper

This project is managed by [veracity](https://github.com/olesho/veracity). The
`veracity` binary is installed on your PATH (pinned in `.veracity-version`); this
project commits no tool source, only declarative wiring that delegates to it.

## Working here

- Lint / format / test a project: `veracity lint`, `veracity fmt`, `veracity test`.
- Regenerate structural docs: `veracity docs markdown` (auto-runs on Stop).
- Regenerate architecture diagrams: `veracity docs diagrams <project>` (manual).
- Check the project matches its lock: `veracity verify`.

Codex project hooks require a one-time trust: run `codex` here and use `/hooks`
to review and trust them. Do not hand-edit files under `hooks/` or `.codex/` —
they are managed by veracity. Your source and native lint config are yours.
