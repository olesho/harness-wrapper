# pi corpus (seed captures)

Raw captures from **pi 0.76.0** (provider `cerebras`, model `gpt-oss-120b`),
recorded live while implementing the pi adapter/profile. They document the real
on-the-wire shapes the code keys on; they are NOT yet wired into an automated
replay test (that is the remaining corpus work — see below).

| File | What it is | Used to validate |
|---|---|---|
| `headless-json-simple.jsonl` | `pi -p --mode json` for a no-tool turn | session header → `pkg/harness/pi` SessionID; `message_end` vs `turn_end` de-dup → StreamParser |
| `headless-json-toolcall.jsonl` | `pi -p --mode json --tools read` (a `read` tool call) | `toolCall` block + `toolResult` message (with `toolCallId`) → StreamParser tool events |
| `tui-turn-raw.bin` | raw PTY bytes of one interactive turn | submit key (`\r`), `Working...`/`Thinking...` busy spinner, idle status line, OSC 133 markers |

## Key shapes (quick reference)

- **Session header** (first `--mode json` line): `{"type":"session","version":3,"id":"<uuid>",…}`.
- **Stream event order**: `session → agent_start → turn_start → (message_start → message_update* → message_end)+ → [tool_execution_*] → turn_end → agent_end`. Parse `message_end` only; `turn_end` duplicates the final `message_end`.
- **Content blocks**: `text` (kept), `thinking` (dropped), `toolCall {id,name,arguments}`; tool results arrive as a `role:"toolResult"` message carrying `toolCallId`/`toolName`.
- **Interactive submit**: a carriage return `\r` (NOT `\n`). pi does not enable the kitty keyboard protocol.
- **Busy indicator**: `Working...` (generating/tools) or `Thinking...` (reasoning).
- **Idle status line**: `… <pct>%/<ctx>k (auto) … <model> • <thinking-level>`.

## Remaining (not yet done)

- A formal screenbench recording + golden replay test under this dir (mirrors `test/corpus/codex`, `test/corpus/claude-code`) so TUI drift is caught automatically — the prerequisite for pinning pi in `pkg/versions/versions.json`.
- A screen-derived end-of-turn marker (`turns.Adapter.OnScreen`) and `MessageExtractor` for clean `Turn.Text` (today the interactive turn completes via the busy-aware idle fallback and `Turn.Text` is the raw screen).
- Interactive-path session-ID capture (e.g. inject `--session-id <uuid>` at launch) so `History()` is transcript-backed in the interactive flow.
