# ADR-021: The Pi profile drives pi over its RPC mode

**Status:** Accepted (2026-10-06)

**Intent:** principle 4, *prefer the harness's own record*, and principle 2, *a wrong verdict is worse
than no verdict* ([INTENT](../../../../INTENT.md#design-principles)) — an input's outcome is read from
pi's session file, and where that file cannot prove one the answer is `unknown`. It also applies 3
(one `api_key` kind and the contract's error classes, whatever provider the model names) and 7 (the key
reaches its own provider's hosts alone).

## Context

pi is a coding agent that serves about forty model providers. Its `--mode rpc` speaks JSON lines on
stdio — `prompt`, `abort`, `get_state`, `get_commands` — and streams the session's events
(`agent_start`, `message_end`, `auto_retry_start`, `agent_settled`). It writes each session as a file
of entries linked by `id` and `parentId` (format v3). hw's older pi layers — a screen adapter, a
headless profile, TUI classifier patterns — were verified on pi 0.76.0 only and are gone; pi 1.0.4 was
probed for this profile ([probes/pirpc](../../../../probes/pirpc/FINDINGS.md)), on macOS and Linux.

What the probe found that shapes the profile:

- pi records no id of the host's for an input: entry ids are its own, and the RPC request id is not
  written. An extension loaded with `-e` sees each input in its `input` hook and can append a
  `custom` entry, which pi writes before the user message.
- A run's end is its last assistant message's `stopReason`: `stop`, `aborted`, `error`, `length`. A
  failed call pi retries is followed by a `context_edit` that drops it; one it gives up on, by nothing.
  An abort while a tool runs is recorded as an `error`, `The operation was aborted.`.
- Error texts are the provider's: `<status> {json}` from Anthropic's API, `OpenAI API error
  (<status>): {json}` or `<code>: <message>` from OpenAI's. Only OpenAI's usage wall says when it
  resets; on Anthropic's API under a key, a 429 is a rate limit.
- pi takes an HTTPS proxy and a certificate authority from its environment, sends a key in
  `x-api-key` (Anthropic), `Authorization` (OpenAI and most others) or `x-goog-api-key` (Gemini), and
  with `PI_OFFLINE=1` contacts no host but the model's.
- The release's executable and its `package.json` are all RPC mode needs.

## Decision

1. **Transport: `pi --mode rpc`, one process per Session**, in a process group of its own, under the
   Session's id. Inputs are `prompt` alone; the profile never calls the setters that write pi's
   global settings. A run ends at `agent_settled`.
2. **Input identity: a tag extension.** The transport leads each input with `<!--hw:NATIVE-->`; the
   extension (`hw-tag.ts`, embedded in the profile and installed with the distribution) takes it off
   and writes it as a `hw.input` entry. The record pairs a tag with the user message it is the
   nearest ancestor of, through the entries pi writes between them — a compaction, a system message —
   and never past a message of the conversation; by `parentId`, never by order, because pi writes the
   tag of an input it refuses as busy, with no user message after it.
3. **Outcomes from the record, as it proves them.** `stop` completes a run, `aborted` interrupts it,
   `length` fails it with `max_output`. A failure ends the run once pi gives up on it: a class pi never
   retries, its last attempt, or any entry but the retry's `context_edit` after it; until then the
   run is open, and a crash there leaves it `unknown`. An abort recorded as an error interrupts the
   run when the profile noted, durably and before asking, that it interrupted the input
   (`scratch/pi-interrupts/<native>`); without the note it is a failure of class `internal`. An
   abort while pi waits to retry leaves the run at its dropped failure, with nothing after it: live,
   `auto_retry_end` (`Retry cancelled`) says so; in the record, the next input's user message ends
   the run, interrupted when noted.
4. **One credential kind, `api_key`, for every provider.** The model names its provider
   (`anthropic/claude-…`); the profile's table holds each provider's fixed hosts and the header its
   key travels in, and refuses a model of a provider it does not hold. The transport writes the key
   into `auth.json` under the provider at every launch. The kind's route is the table's union; each
   placeholder's swap is narrowed to the model's provider ([ADR-020](adr-020-placeholder-model.md)).
   A subscription's token is refused.
5. **Capabilities:** `resume`, `assign_session_id`, `tools_observed`, `retry_visible`,
   `brokered_credentials`. Not `prompts` — an extension's dialog is answered `cancelled` — nor
   `streaming_text`, `rate_limits`, `subagents`, `autonomous_turns`, `background_turns`,
   `session_load` or `login_keeper`.

## Alternatives

- **pi's TUI, through the screen layers.** Turned down: the rendered screen is a contract hw does not
  own (principle 1), and RPC mode states what the screen shows.
- **Matching an input by a digest of its text**, against the first user message after the leaf
  `get_entries` reported. Turned down: it fails for an input pi rewrites, and for two inputs with the
  same text.
- **A credential kind per provider.** Turned down: it puts forty field names before a person creating
  an agent, for one key per agent either way.
- **Counting failed answers alone, without the `context_edit`.** Turned down: the edit is pi's word
  that it retries; the count needs the retry limit, which the profile renders into `settings.json` and
  `open_config` so the two cannot differ.

## Consequences

- A tag is visible to the extension's other hooks only for the moment the input hook runs; the model
  never sees it.
- An abort the profile did not ask for — none is known — reads as a failure, not an interrupt.
- A model pi knows but the table does not hold (Bedrock, Vertex, Azure, Copilot, the subscription
  logins) cannot run on the profile: its key reaches no fixed host, or travels in no header.
- Saved sessions do not load yet (`session_load`); the profile declares its history roots and secret
  paths, so an archive exported now can load once a fixture proves the pinned pi's sessions.

## Follow-ups

- `session_load`: relocating a session's file to a new workspace, with a fixture the pinned pi saved.
- Subscription logins, which pi refreshes and writes back itself.
