# Structured turn protocol

`pkg/turnproto` defines the **neutral wire contract for one supervised turn**: a single JSON object on
stdout plus an exit code. It is what [`harness-wrapper structured-run`](../guide/cli.md#structured-one-shot-structured-run)
emits and what a host orchestrator parses — including a host written in another language.

The package is a faithful backport of meta-harness's TypeScript `src/turnproto/protocol.ts` +
`parse.ts`. **Every exported name, JSON tag, status string and exit code is a frozen cross-repo
contract**: the two implementations are meant to produce byte-compatible results for the same run, and
the [cross-language conformance corpus](testing/conformance.md) pins that agreement.

![Structured turn protocol](../diagrams/structured-run.svg)

## Exit codes

```go
const (
	ExitOK       = 0   // status "completed"
	ExitError    = 1   // status "errored" | "startup_error"
	ExitUsage    = 2   // CLI usage error — carried for fidelity; nothing in Go emits it
	ExitDeadline = 124 // status "deadline"
)

func ExitCode(s TurnStatus) int   // the one canonical status → exit-code table
```

`ExitCode` is the only place the mapping lives; never re-derive it. `ExitUsage` exists so a Go host
parsing the output of the *TypeScript* runner reads the same vocabulary — the Go CLI's own usage
errors exit 2 through its flag parser, not through this constant.

On a deadline the CLI also prints one frozen anchor line to **stderr**:

```go
const DeadlineLine = "harness-wrapper run: context deadline exceeded"
```

Hosts may match that string; it is part of the contract, so it does not change with wording elsewhere.

## Status vocabulary

```go
type TurnStatus string

const (
	StatusCompleted    TurnStatus = "completed"     // the harness finished a turn; Reply is populated
	StatusErrored      TurnStatus = "errored"       // the turn reached the harness and failed
	StatusDeadline     TurnStatus = "deadline"      // the run timeout fired (exit 124)
	StatusStartupError TurnStatus = "startup_error" // no harness was ever launched
)
```

The `startup_error` / `errored` split is load-bearing: `startup_error` means the failure happened
*before* the harness process existed (bad binary path, invalid config), so nothing about the harness's
behavior can be inferred from it — which is also why it never carries a `permission_mode`.

## The result object

```go
type StructuredTurnResult struct {
	Status            TurnStatus          `json:"status"`
	Reply             string              `json:"reply"`
	HarnessSessionID  string              `json:"harnessSessionID"`
	TranscriptEntries []transcript.Event  `json:"transcript_entries"`
	WorkingDir        string              `json:"working_dir"`
	Reason            string              `json:"reason,omitempty"`
	TranscriptError   string              `json:"transcript_error,omitempty"`
	Usage             *transcript.Usage   `json:"usage,omitempty"`
	PermissionMode    string              `json:"permission_mode,omitempty"`
}
```

| Field | Always present | Notes |
|---|:--:|---|
| `status` | ✅ | One of the four values above. |
| `reply` | ✅ | `""` on every non-`completed` status — the key is still emitted. |
| `harnessSessionID` | ✅ | **camelCase**, unlike its neighbours. That inconsistency is inherited from the TypeScript original and frozen; do not "fix" it. |
| `transcript_entries` | ✅ | `[]` when the transcript could not be read or the harness has no reader. Elements are [`transcript.Event`](transcript.md#the-canonical-event) in their public JSON shape. |
| `working_dir` | ✅ | The directory the turn ran in. |
| `reason` | — | Present on `errored` / `startup_error`. Human-readable; not stable for parsing. |
| `transcript_error` | — | A best-effort transcript read failed; the turn itself may still be `completed`. |
| `usage` | — | [`transcript.Usage`](transcript.md#token-usage). Omitted entirely when unknown — but when present, **all five inner keys serialize, including zeros**. |
| `permission_mode` | — | The canonical **launch** rung. See below. |

Exactly one such object is written, as the **last line of stdout**. Anything the harness or the Go
runtime printed before it is tolerated by design.

### `permission_mode` on this result

This key reports the canonical rung the turn was **launched at** (`plan` | `manual` | `ask` | `auto` |
`bypass`), resolved by `wrapper.EffectiveLaunchRung` from the final argv — never a harness-native
spelling like `acceptEdits` or `read-only`.

Read it one-directionally:

- **Presence of `bypass` is trustworthy.** Every unrestricted launch path reports it, including paths
  that carry no canonical flag: `--sandbox-defaults`, a raw `--dangerously-skip-permissions` after
  `--`, and codex's `-s danger-full-access` in any spelling.
- **Absence never means "safe."** It means no rung could be *named* — harness default, unsupported
  harness, a codex argv that sets only the `-a` approval axis, or an unreadable flag value. Claude's
  `dontAsk` is no longer one of these causes: it reports the `manual` rung.
- **A rung is a launch argument, not an enforcement claim.** On this path claude's per-tool permission
  dialog is not detected at all, and codex approvals are auto-answered by
  [`oneshot.AutoAcceptAnswer`](oneshot.md#what-that-policy-costs). See
  [runtime enforcement per path](wrapper.md#runtime-enforcement-per-path).
- **`startup_error` never carries it**, because no harness was launched.
- It reports the **argv half only**. The `IS_SANDBOX=1` environment half that `--sandbox-defaults`
  also sets is not representable as a rung and is not reflected here.

> **Same key, different meaning on the gateway.** `permission_mode` in a
> [`harness-chatd`](../guide/gateway.md#permission-mode-semantics) response reports what the caller
> *requested* at open time and may legitimately carry `acceptEdits`, `dontAsk` or `read-only`. The two
> readings deliberately differ — do not share a parser or a schema between them.

## Parsing: `ParseLastJSONLine`

```go
func ParseLastJSONLine(data []byte) (*StructuredTurnResult, bool)
```

The host-side reader. It splits on `"\n"` and scans **backward** for the first line that parses:

- empty lines are skipped;
- a line whose first non-space byte is not `{` is rejected outright, so a bare scalar or a JSON array
  in the tail can never be mistaken for a result;
- the first line that unmarshals wins — so a truncated final line, a trailing log message, or
  multiple JSON lines (last object wins) all behave predictably.

This is why the CLI is free to let the harness print a banner, a version notice, or progress noise on
stdout: only the last well-formed JSON object is contractual.

## Producers and consumers

| Role | Where | What it does |
|---|---|---|
| Producer | `cmd/harness-wrapper structured-run` | Builds the result, sets `permission_mode` (guarded so `startup_error` never carries one), writes exactly one marshalled line, exits with `ExitCode(status)`, and prints `DeadlineLine` to stderr on a deadline. Its startup-error path falls back to a hand-rolled minimal object so a marshalling failure still produces valid output. |
| Consumer | [`pkg/env.RunStructuredTurn`](env.md#running-a-turn-in-a-workspace) | The Go host client: uploads the prompt file into a workspace, execs `harness-wrapper structured-run`, feeds the captured stdout to `ParseLastJSONLine`. A non-zero guest exit is **not** an error there — only a spawn/transport failure is. |
| Consumer | meta-harness (TypeScript) | The other half of the frozen contract. |

Because the protocol is transport-agnostic, the same bytes work over a local pipe, a container `exec`,
or a remote sandbox — which is exactly what [`internal/env`](env.md) exploits.
