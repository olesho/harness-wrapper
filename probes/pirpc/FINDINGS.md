# pi's RPC mode: findings

Probed on pi 1.0.4, the release's own executable, on Linux arm64 (Ubuntu 26.04 under Lima) and macOS
arm64, on 2026-10-06, against `internal/mockapi` speaking the Anthropic Messages and the OpenAI
Responses APIs. Each finding is a test in this package; `-v` prints the RPC records and the session
files behind it. Not run: Linux x64 (no throwaway x64 host), a real account.

## The adapter mappings, for pi

The rows of the interface specification's adapter mappings, for `pi --mode rpc`.

| Contract | pi 1.0.4 | Test |
|---|---|---|
| `provision` | The agent dir (`PI_CODING_AGENT_DIR`): `settings.json`, `models.json`, `mcp.json`, `auth.json` (a key under its provider), `APPEND_SYSTEM.md`, `skills/`. Sessions under `--session-dir`. pi writes `auth.json` and `models-store.json` there at start, so the dir is writable. | `TestARun` |
| `open` fresh / reopen | `--session-id <id> --session-dir <dir>`: pi creates the session's file with its first user message (`<dir>/<time>_<id>.jsonl`), and reopens that file under the same flags. A reopen whose file is gone silently starts a new, empty session under the same id (a warning on stderr): the profile checks the file before a reopen. stdin's end exits pi with 0. | `TestReopen` |
| `send` receipt | `prompt` answers `{success: true, data: {disposition: "started"}}`. While a run is busy, `success: false`, "Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message."; for a provider with no key, `success: false`, "No API key found for anthropic.", and nothing is written. Either refusal means the input never ran. | `TestARun`, `TestReceipt` |
| the input in the record | No id a client chooses reaches the record. The tag extension (`hwtag.ts`, loaded with `-e`) closes that gap: an input that starts with `<!--hw:ID-->` loses the tag, and a `custom` entry `hw.input {id}` lands before its user message, whose parent it is, or whose grandparent through the `system` message pi writes when the system prompt changed. pi runs input hooks before it checks for a busy run, so a refused input leaves a tag with no user message, mid-run: between the running input's user message and its answer. A tag pairs with its user message by `parentId`, never by order. The extension shows in `get_commands` as `hw-tag` (source `extension`); without it the tag stays in the input, and the model sees it. | `TestARun`, `TestReceipt`, `TestTags` |
| record-origin `turn_ended` | The last assistant message of the input's run. `stopReason` `stop`: completed. `error`, with `errorMessage`: failed; after exhausted retries each failed attempt is an assistant `error` and a `context_edit` that drops it from the context, and the last one is the outcome. `aborted` ("The operation was aborted.", partial text kept): interrupted mid-stream or before the first token. Interrupted mid-tool: an errored `toolResult` "Command aborted", then an assistant `error` "The operation was aborted.". | `TestOutcomes`, `TestRetry` |
| `interrupt` outcome | `abort` answers only after `agent_settled`; mid-tool it kills the tool's process tree. Its record is as above: `aborted`, or the mid-tool pair. | `TestOutcomes` |
| `turn_ended` (live) | `agent_settled` ends an input's run, after one or more `agent_end`; the run's outcome is its last assistant `message_end`. | `TestARun`, `TestOutcomes` |
| `retrying` | `auto_retry_start {attempt, maxAttempts, delayMs, errorMessage}` per retry, then `auto_retry_end {success, attempt, finalError}`. pi retries 429, 500 and 529 (`retry.maxRetries` in `settings.json`), usage walls included. | `TestRetry`, `TestErrorTexts` |
| error classes | Text only. Anthropic's API: `<status> {json}` with the API's error type (`overloaded_error`, `rate_limit_error`, `api_error`). OpenAI's: `OpenAI API error (<status>): {json}`, or `<code>: <message>` (`server_is_overloaded`). On Anthropic's API a usage wall reads like any 429 (pi drops the unified limiter's headers); on OpenAI's, `usage_limit_reached` carries `resets_at`. | `TestErrorTexts` |
| `tools_observed` | Live: `tool_execution_start` / `tool_execution_end` with `toolCallId`, `toolName`, `args`. In the record: the assistant's `toolCall` blocks and the `toolResult` messages, by call id. | `TestOutcomes` |
| `prompts` | None in stock pi: no tool asks before it runs. Only an extension raises a dialog (`extension_ui_request`). | — |
| `rate_limits` | None: pi relays no rate-limit headers. | `TestErrorTexts` |
| crash | Entries are appended at `message_end`, without fsync. Killed mid-stream, the input's user message ends the file; mid-tool, the assistant's tool call ends it, and the tool's command survives pi (it runs in a session of its own). Killed right after `prompt`, there is no file yet. Reopened, pi does not go on with the run, and the next input's request carries the orphaned user message. | `TestCrashes` |
| stop | stdin's end: exit 0; SIGTERM: exit 143. Either ends the tool's command; a run cut mid-tool ends the file as a crash does. | `TestStop` |
| record items and checkpoint | The session's JSONL file, read directly with no pi process: an append-only tree of entries with `id` and `parentId`, the header first. A byte offset serves as the checkpoint. | `TestARun` |

## Configuration

- **MCP.** pi connects to the servers in `mcp.json` when it starts: `initialize`, then `tools/list`,
  about 0.4 s after start and before any prompt. A header value `"!command"` is the command's
  output, which is how a `headers_file` reaches a server. `"exposure": "direct"` declares a server's
  tools to the model as `mcp__<server>__<tool>`; without it they stay behind `codemode`.
  (`TestMCPAtOpen`)
- **Models.** A model outside pi's catalog runs as a custom id, with a warning ("Model
  "claude-probe-9" not found for provider "anthropic". Using custom model id."). A `models.json`
  entry under its provider (`{"id": …}`) runs it without one. (`TestModelOutsideTheCatalog`)
- **Commands after a tag.** A tagged input is no extension command (`/hw-tag` reaches the model), but
  prompt templates and skills still expand (`/greet` becomes its text). (`TestTags`)

## Network

Behind a broker — an HTTPS proxy that ends TLS with its own CA and swaps placeholders in headers — pi
reaches the providers' own hosts through `HTTPS_PROXY` (CONNECT tunnels), trusting the CA from
`NODE_EXTRA_CA_CERTS` or `SSL_CERT_FILE`; both work. Each key travels in its API's header:
`x-api-key` for Anthropic's, `Authorization: Bearer` for OpenAI's, `x-goog-api-key` for Gemini's.
With `PI_OFFLINE=1`, the provider's host is the only one pi asks for. (`TestBehindABroker`)

## Distribution

The release's `pi` is self-contained (built with Bun; no Node). Alone it reports version `0.0.0`;
with the release's `package.json` beside it, its version. Those two files run an RPC turn with a
tool. The rest of the release — `docs/`, `examples/`, `theme/`, `assets/`, `export-html/`,
`native/`, `photon_rs_bg.wasm` — serves the TUI, pi's docs and images. (`TestDistribution`)

## The gate

Both conditions of Step 0 hold:

1. For a tagged input, pi's record proves `completed` (`stop`), `errored` (`error`) and `interrupted`
   (`aborted`, or the mid-tool pair, which the profile tells from a failure by its own note that it
   interrupted), and leaves everything else `unknown`: a tag without its user message, a user
   message without an answer, a tool call without its result.
2. pi works behind a broker that swaps a placeholder for its key, on the providers' own hosts, with
   no other host reached.

The real broker (iron-proxy, `enforce`) is agentd's end-to-end test, after the profile exists.
