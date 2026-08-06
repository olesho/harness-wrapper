# Permissions & sandboxing

Every supported harness has its own permission vocabulary, its own flags, and its own idea of what
"ask me first" means. harness-wrapper normalizes that into **one ladder of five rungs** and translates
to native flags before the harness starts.

This page is the single place the whole model is written down: the vocabulary, where you set it, how
it is validated, how to change it on a live session, and — most importantly — **what it does and does
not actually enforce**.

![Canonical rungs and how each harness launches them](../diagrams/permission-model.svg)

## The vocabulary

```
plan  →  manual  →  ask  →  auto  →  bypass          (least → most permissive)
```

| Rung | Intent | `claude` / `claude-code` | `codex` |
|---|---|---|---|
| `plan` | think, don't act | `--permission-mode plan` | **rejected** — no launch flag exists |
| `manual` | ask before everything | `--permission-mode manual` | `-s read-only -a untrusted` |
| `ask` | edits allowed, ask for the rest | `--permission-mode acceptEdits` | `-s workspace-write -a on-request` |
| `auto` | work in the workspace unattended | `--permission-mode auto` | `-s workspace-write -a never` |
| `bypass` | no restrictions | `--permission-mode bypassPermissions` | `-s danger-full-access -a never` |

Each harness's **native spellings** are also accepted for that harness: `acceptEdits`, `dontAsk`,
`bypassPermissions` for claude; `read-only`, `workspace-write`, `danger-full-access` for codex. A
native spelling sent to the harness that does not own it is **rejected**, not ignored.

`opencode` and `pi` have no permission axis; any mode against them is an error rather than a silent
no-op.

### Why codex has no launch-time `plan`

Codex ships no flag for the non-executing rung. Silently dropping the request would start codex with
*no* restriction for a caller who asked for the strictest one, so it is a hard error everywhere. Codex
plan mode is reachable only in-band, by sending `/plan` once the session is open — which is exactly
what [`SetPermissionMode`](#switching-on-a-live-session) does for you.

## Where you set it

| Surface | How |
|---|---|
| [CLI](cli.md#flags) | `--permission-mode <rung>` — accepted in **every** mode, passthrough included |
| [Chat API](chat.md#open) | `chat.Options.PermissionMode` |
| [`harness.RunTurn`](../internal/harness.md#runturn--exactly-one-interactive-turn) | `TurnConfig.PermissionMode` |
| [One-shot](../internal/oneshot.md) | `oneshot.Config.PermissionMode` |
| [HTTP gateway](gateway.md#permission-mode-semantics) | `permission_mode` on `POST /v1/conversations` and `POST /v1/turns` |
| [Workspace turns](../internal/env.md#running-a-turn-in-a-workspace) | `StructuredTurnConfig.PermissionMode` |
| Clients | `permissionMode` (TypeScript) / `permission_mode` (Python) |

All of them funnel into `wrapper.Config.PermissionMode` and the same translator.

## Validation rejects; it never silently no-ops

The wrapper refuses, rather than dropping, every one of these:

- an unknown mode string;
- a native spelling belonging to the *other* harness;
- `plan` on codex;
- any mode on a harness with no permission axis;
- a non-bypass mode when a bypass-enabling flag (`--dangerously-skip-permissions`,
  `--dangerously-bypass-approvals-and-sandbox`) is already in `Args` — a contradiction, not a merge.

Over HTTP each of these is a `400 invalid_config`; in Go they wrap both `chat.ErrInvalidOptions` and
`wrapper.ErrInvalidConfig`, so `errors.Is` matches either.

**An explicit flag in `Args` wins.** If argv already carries a permission-axis flag, the typed knob is
not injected on top — in any of the spellings the harnesses accept (bare token, `--flag=value`, and
clap's attached short form). For codex the suppression is whole-directive: a flag on *either* axis
suppresses injection on both, so you never get half your argv from one source and half from another.

## The two halves of "bypass"

`--sandbox-defaults` (CLI, `run` and `structured-run` only, claude only) contributes **two** things:

- the **args half** — `--dangerously-skip-permissions`, which the `bypass` rung also delivers;
- the **env half** — `IS_SANDBOX=1`, which nothing else sets. It is what suppresses claude's
  *Bypass Permissions mode* acceptance screen and allows running as root.

So `--permission-mode bypass` is **not** a drop-in for `--sandbox-defaults`: with the rung alone the
acceptance screen comes back and root is disallowed. The one legal pairing is
`--sandbox-defaults --permission-mode bypass`, where `--sandbox-defaults` contributes the env half
only; every other pairing exits 2. Over HTTP there is no `--sandbox-defaults` equivalent at all — pass
`IS_SANDBOX=1` in the request's `env`, or answer the acceptance screen with an
[`input_policy`](gateway.md#permission-mode-semantics).

The full rationale, the dedup rules, and why the env half lives in exactly one file are in the
[wrapper spec](../internal/wrapper.md#sandbox-defaults-injection).

## Switching on a live session

A launch rung is not a life sentence. `pkg/chat` can read the harness's current posture off the screen
and drive it to a different one:

```go
func (c *Conversation) PermissionMode() (string, bool)
func (c *Conversation) SetPermissionMode(ctx context.Context, target string) (observed string, err error)
```

`PermissionMode` is a pure read — no control token, no readiness wait, valid mid-turn. `false` means
the *screen* carries no readable signal (a modal is covering the footer, or the harness is still on an
onboarding wall), never "readable, and not the mode you asked about".

`SetPermissionMode` **returns the final observed posture on every path, success or failure** — that is
why the signature returns a string alongside the error. Its gates, in order:

1. the conversation is open;
2. the harness has a switchable axis at all — claude exposes the rungs, codex exposes only its
   *collaboration* axis (`plan` / `default`);
3. the target is reachable on that axis (this is what rejects claude's launch-only `dontAsk` and
   codex's sandbox spellings);
4. the caller holds the control token;
5. no turn is in flight;
6. `bypass` is only reachable when the session was **launched** bypass-enabled — you cannot cycle your
   way up to unrestricted;
7. the composer is ready.

If the harness is already at the target, it returns immediately without typing anything.

### Cycle-and-check, never cycle-and-count

Switching drives the harness's own key sequence and **re-reads the posture after every press**. It
never infers the mode from a press count: the ring length is used only as an upper bound. A repaint
budget per press keeps a busy harness from being spammed, and a modal that appears mid-cycle aborts
rather than typing into it:

```go
var blocked *chat.PermissionModeBlockedError
if errors.As(err, &blocked) {
	_ = conv.Answer(ctx, blocked.Request.ID, chat.InputAnswer{OptionID: "1"})
	mode, err = conv.SetPermissionMode(ctx, "bypass") // the token is still held
}
```

Codex's `plan` is entered by submitting `/plan` instead of cycling, and codex refuses that while a task
is running. That refusal is recognised and retried a bounded number of times, then reported as
`ErrCodexPlanRefusedBusy` — a *retryable* condition, distinct from "the switch failed".

### If it doesn't take

When the target is not observed within the bound, the driver **restores the starting posture** and
reports what happened:

| Outcome | Error |
|---|---|
| target not reached, start restored | `ErrPermissionModeSwitchFailed` |
| target not reached, restore also failed, and the session is left *more permissive* than it started | `ErrPermissionModeIndeterminate` |
| target not reached, restore failed, not more permissive | `ErrPermissionModeSwitchFailed` |
| aborted by a modal, a cancelled context, a closed session, or a write failure | that error, **no restore attempt** |

`ErrPermissionModeIndeterminate` is the one that deserves an alarm: it is the explicit signal that the
invariant *"never silently more permissive than it started"* could not be guaranteed.

### Scope

The observed mode is **process-local and not persisted**. `chat.Session` carries no rung, so the
invariant holds within a single conversation's lifetime; reopening a stored session starts from the
launch rung again.

## Reading a rung back

Three different surfaces report a permission mode, and they do **not** mean the same thing:

| Surface | Reports | Notes |
|---|---|---|
| `wrapper.EffectiveLaunchRung(harness, args, mode)` | the rung implied by the **final argv** | replays the suppression rule, so argv wins over the knob; `""` = could not be named |
| [`structured-run` result](../internal/turnproto.md#permission-mode-on-this-result) | the canonical **launch** rung | never a native spelling; absent on `startup_error` |
| [Gateway conversation listing](gateway.md#permission-mode-semantics) | what was **requested** at open | may legitimately be `acceptEdits`, `dontAsk`, `read-only` |
| `turns.PermissionModeDetector` (via `conv.PermissionMode()`) | what is **on screen now** | claude returns a rung; codex returns a collaboration value |

Do not share a parser between them.

## What is actually enforced

A rung is a launch argument. Whether it *binds* depends on who is watching the harness:

| Axis | passthrough | interactive `run` (tty) | unattended `run` / `structured-run` |
|---|---|---|---|
| claude per-tool permission dialog (`plan`/`manual`/`ask`) | human answers | **not detected** → stalls to the deadline | **not detected** → stalls to the deadline |
| claude bypass-acceptance screen | human answers | surfaced → chooser on the tty | auto-accepted by the trust policy |
| codex `-s` sandbox axis | enforced by codex | enforced by codex | **enforced by codex** |
| codex `-a` approval axis | human answers | surfaced → chooser on the tty | **auto-approved** |

The practical reading: on unattended paths only codex's sandbox axis is a real restriction. Do not
treat `"permission_mode": "manual"` on a structured run as evidence that the turn was supervised — it
is evidence of how the process was launched, nothing more. If you need real containment for an
unattended run, put the run in a [contained workspace](../internal/env.md) rather than relying on the
rung.
