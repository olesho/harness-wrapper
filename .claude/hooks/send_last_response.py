#!/usr/bin/env python3
"""Claude Code Stop hook: POST the last assistant response to an HTTP server.

Installed per-project (see SETUP.md). Claude Code runs this when a turn ends
and pipes a JSON payload on stdin that includes `transcript_path` (the
conversation .jsonl) plus session metadata. We read the last assistant
message out of the transcript and POST the full payload + response to the
server.

Config (environment variables):
  CC_WEBHOOK_URL    target URL (default http://localhost:8000/)
  CC_WEBHOOK_DEBUG  if set to "1", append a trace to /tmp/cc_hook_debug.log
"""
import json
import os
import sys
import time
import urllib.request

WEBHOOK_URL = os.environ.get("CC_WEBHOOK_URL", "http://localhost:8000/")
DEBUG = os.environ.get("CC_WEBHOOK_DEBUG") == "1"
DEBUG_LOG = "/tmp/cc_hook_debug.log"


def debug(msg: str) -> None:
    if not DEBUG:
        return
    try:
        with open(DEBUG_LOG, "a", encoding="utf-8") as f:
            f.write(f"{time.strftime('%H:%M:%S')} {msg}\n")
    except Exception:
        pass


def last_assistant_text(transcript_path: str) -> str:
    """Return the concatenated text blocks of the final assistant message."""
    last = ""
    try:
        with open(transcript_path, "r", encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    entry = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if entry.get("type") != "assistant":
                    continue
                content = entry.get("message", {}).get("content", "")
                if isinstance(content, str):
                    text = content
                else:  # list of content blocks
                    text = "".join(
                        b.get("text", "")
                        for b in content
                        if isinstance(b, dict) and b.get("type") == "text"
                    )
                if text.strip():
                    last = text  # keep overwriting -> ends on the final turn
    except FileNotFoundError:
        pass
    return last


def main() -> int:
    raw = sys.stdin.read()
    debug(f"INVOKED url={WEBHOOK_URL} stdin_bytes={len(raw)}")
    try:
        payload = json.loads(raw)
    except Exception as e:
        debug(f"BAD STDIN: {e!r}")
        return 0

    response_text = last_assistant_text(payload.get("transcript_path", ""))
    debug(
        f"event={payload.get('hook_event_name')} "
        f"session={payload.get('session_id')} response_len={len(response_text)}"
    )

    # Forward the full hook payload, plus the extracted response.
    # `last_assistant_message` is the canonical field consumers read;
    # `response` is kept as a backward-compatible alias.
    out = dict(payload)
    out["last_assistant_message"] = response_text
    out["response"] = response_text
    body = json.dumps(out).encode("utf-8")

    req = urllib.request.Request(
        WEBHOOK_URL, data=body, headers={"Content-Type": "application/json"}
    )
    try:
        urllib.request.urlopen(req, timeout=5).read()
        debug("POST ok")
    except Exception as e:
        # Never block Claude Code on a webhook failure.
        debug(f"POST failed: {e!r}")
        print(f"send_last_response hook: {e}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
