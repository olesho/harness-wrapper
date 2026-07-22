# HARNESS-WRAPPER-78 — `--sandbox-defaults` argv tripwire for meta-harness

Sibling repo: `meta-harness` (`~/Work/aether/meta-harness`, override with
`META_HARNESS_DIR`). Paired consume-side ticket: **META-HARNESS-107**
(`fleet-db://META-HARNESS/META-HARNESS-107`) — *"sandbox-defaults becomes sugar
for permissionMode bypass"*, `open` / `decomposed` under META-HARNESS-99.

Like [`HARNESS-WRAPPER-77-permission-mode.md`](HARNESS-WRAPPER-77-permission-mode.md)
and unlike [`APPLY.md`](APPLY.md) (HARNESS-WRAPPER-66), this is a **note, not a
byte-exact bundle**: nothing in this file is copied into the sibling. It is
staged in the canonical `harness-wrapper` worktree for the same reason the other
two are — that is the unit the fleet supervisor publishes.

**This is a watch, not a pending break.** META-HARNESS-107's own retitle note
already fixes its acceptance bar the safe way: *"**The sugar stays claude-only
and argv-preserving.** Nothing about the argv or env that `--sandbox-defaults`
emits today changes for any existing caller."* So as written, MH-107 promises not
to fire this tripwire. It is written down precisely so that promise is checked
rather than assumed.

## The invariant

In the **`--sandbox-defaults`-alone** case (no `--permission-mode`), the claude
arg half stays `--dangerously-skip-permissions` — matching `metaHarnessArgs`
(`src/cli/structured-runner.ts:89-93`), which emits that one token for
`claude-code` and nothing for any other harness.

The **compose** arm (`--sandbox-defaults --permission-mode bypass`, where Go
skips the arg append and lets `pkg/wrapper` own the single permission directive)
is a **Go-only detail with no TS counterpart** — meta-harness has no
`--permission-mode`. Nothing in that arm is a parity claim.

## The tripwire tests

If META-HARNESS-107's acceptance bar ever moves off "argv-preserving", the Go
follow-up is a change to `applySandboxDefaults`'s alone-case arg half, and these
force it to be conscious rather than silent:

- `cmd/harness-wrapper/sandbox_defaults_test.go` — `TestApplySandboxDefaults`
  rows 1–7 (the pre-composition rows; a diff in *their expectations* is this
  tripwire firing).
- `cmd/harness-wrapper/structured_run_test.go` —
  `TestStructuredRun_SandboxDefaultsInjection`, which observes the argv and env
  of a really spawned process.

The golden-file half of the same mirror is already recorded at
`docs/md/internal/wrapper.md` (`cmd/harness-wrapper/testdata/flags.golden` ↔ the
TS half's `test/cli/testdata/wrapper-flags.golden`); this note is its behavioural
counterpart.

## Pre-existing divergences — do not compound them

Both predate this ticket and are **not** being closed by it. They are recorded so
neither side "fixes" one unilaterally and so a future parity pass knows they are
known:

- **Host-set `IS_SANDBOX`.** Go *preserves* it — `hasEnvKey` suppresses the
  append when the key is already defined, whatever its value
  (`cmd/harness-wrapper/sandbox_defaults.go`). meta-harness's `buildGuestEnv`
  (`src/cli/structured-runner.ts:100-106`) *overwrites* it to `"1"`.
- **Token position.** Go **appends** the claude token after the caller's args;
  meta-harness **prepends** it (`src/cli/structured-runner.ts:359-362`). claude
  parses these flags position-independently, so this is cosmetic — but it means
  argv parity is "up to position", not byte-equality.

## Acceptance

Nothing to apply in the sibling. If META-HARNESS-107 lands with its stated bar
intact, this note stays a no-op; if that bar moves, the Go-side change and the
two tripwire tests above are the work it implies.
