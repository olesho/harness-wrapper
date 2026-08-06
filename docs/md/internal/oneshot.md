# One-shot turns

`pkg/oneshot` is the **in-process** library for "run one turn, tell me what happened". It drives a
real interactive harness through [`harness.RunTurn`](harness.md#runturn--exactly-one-interactive-turn)
under a headless auto-accept policy and returns a
[`turnproto.TurnStatus`](turnproto.md#status-vocabulary) — the same frozen vocabulary the
[`structured-run` CLI](../guide/cli.md#structured-one-shot-structured-run) puts on the wire.

It is the shared core behind the one-shot paths, so the CLI, the gateway and an embedding Go program
classify a turn the same way.

## API

```go
type Config struct {
	Harness    string // required
	BinaryPath string // required
	Args       []string

	Effort, Model, PermissionMode string

	WorkingDir string
	Env        []string // the caller pre-cleans harness-specific vars
	Prompt     string   // required
}

type Outcome struct {
	Status           turnproto.TurnStatus
	Reply            string // populated only on "completed"
	Reason           string
	HarnessSessionID string
}

func RunOneShot(ctx context.Context, cfg Config) (turnproto.TurnStatus, error)
func RunOneShotDetailed(ctx context.Context, cfg Config) (Outcome, error)

// Building blocks, exported so other one-shot paths classify identically:
func Classify(res harness.TurnResult, err error) (turnproto.TurnStatus, string)
func Reply(res harness.TurnResult) string
func AffirmativeOption(req chat.InputRequest) *chat.InputOption
func AutoAcceptAnswer(req chat.InputRequest) (chat.InputAnswer, bool)
```

### The error contract

**All four classified outcomes return a nil error.** `completed`, `errored`, `deadline` and
`startup_error` are *results*, not failures — a caller switches on the status. A non-nil error is
reserved for something that could not be classified at all, which in practice means an invalid config
(empty harness, binary path or prompt).

This is the same shape as [`pkg/env.RunStructuredTurn`](env.md#running-a-turn-in-a-workspace) and for
the same reason: "the harness hit its deadline" is information, not an exception.

## How a turn is classified

`Classify` branches on the error `RunTurn` returned:

| Condition | Status | Reason |
|---|---|---|
| no error, turn state `complete` | `completed` | — |
| no error, any other turn state | `errored` | `turn ended in unexpected state` |
| context deadline exceeded | `deadline` | — |
| the turn itself errored | `errored` | the turn's reason, or `turn errored` |
| the conversation was closed | `errored` | the error text |
| anything else, session id known | `errored` | the error text |
| anything else, no session id | `startup_error` | the error text |

The last two rows are the meaningful split: a session id means the harness process existed and did
something, so the failure is about the *turn*; no session id means the run never got that far.

## Which reply text you get

`Reply` prefers the harness's **own transcript** over screen-scraped text, keyed on the history source
`RunTurn` reported — not on whether history is non-empty, since the store fallback also returns turns.
The order is: transcript → the turn's extracted text → the last assistant turn in history.

Set `HARNESS_WRAPPER_RUN_DEBUG=1` to have the chosen source logged to stderr; it is the fastest way to
tell "the model said little" apart from "we lost the transcript and scraped the screen".

## The policy `oneshot` owns

Because it is headless by construction, `oneshot` fixes a policy the caller does not have to assemble:

- **exit after the turn** — the harness is gracefully quit and stopped, which also flushes its session
  log so the transcript is readable;
- **auto-skip the codex update notice**, so an unattended run doesn't wedge on a menu;
- **auto-answer the folder-trust prompt** affirmatively;
- **auto-accept** anything else that surfaces as an input request, via `AutoAcceptAnswer` — an
  affirmative option if one is recognisable, otherwise the first option, otherwise decline.

It never touches the terminal. Interactive one-shot behaviour — asking a human on `/dev/tty` — stays
in `cmd/harness-wrapper`, which is why `--auto-accept` is a CLI flag and not a `Config` field.

### What that policy costs

These limits are deliberate and pinned by tests; read them before trusting a restrictive rung on this
path:

- The affirmative trust answer also accepts claude's **bypass-permissions acceptance screen** — the
  detector reports both under one kind, so no policy can target them independently.
- Claude's **per-tool permission dialog is not detected at all**, so `plan` / `manual` / `ask` do not
  produce an answerable prompt here; the turn stalls until the deadline.
- `AutoAcceptAnswer` is wired unconditionally, so codex **approval prompts are auto-approved** even
  when a restrictive rung was requested. Codex's sandbox axis still binds, because codex enforces it
  itself.

The full per-path table is in [Runtime enforcement per path](wrapper.md#runtime-enforcement-per-path).

## Deadlines

`oneshot` owns no timeout and reads no environment. The per-turn deadline is the caller's `ctx`. The
CLI is what supplies a default (15 minutes, overridable) before calling in — keeping the clock at the
boundary where a user can see and change it.

## Compared with a chat conversation

| | `pkg/oneshot` | [`pkg/chat`](../guide/chat.md) |
|---|---|---|
| Turns | exactly one | many |
| Process | quit and stopped when the turn ends | lives until `Close` |
| Store | an ephemeral in-memory store | caller-supplied `Store` |
| Blocking prompts | auto-answered | policy, callback, or surfaced to the client |
| Result | a status + reply | an event stream and history |

Reach for `chat` when a human or a service will take more than one turn; reach for `oneshot` when the
answer to "did this work?" is the whole product.
