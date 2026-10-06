#!/usr/bin/env python3
"""A local stand-in for the Anthropic Messages API, for driver probes.

Routes on the text of the last *user* message that carries text (claude may
append trailing system reminders; tool results continue the scenario):

  PING <n>         reply "PONG <n>"
  SLOW <n>         reply "slow" in <n> chunks, 0.5 s apart (default 40)
  STALL <s>        send message_start, then nothing for <s> seconds (default 60)
  TOOL <command>   a Bash tool_use running <command>; after its result, "TOOL DONE"
  MCP              a tool_use of mcp__probe__echo; after its result, "MCP DONE"
  ERR <code> <k>   answer HTTP <code> the first <k> times this text is seen
                   (529 -> overloaded_error, 429 -> rate_limit_error), then "RECOVERED"
  BIG <kib>        reply with <kib> KiB of text, streamed in 64 KiB deltas (capacity runs)
  LOOP <k> <bytes> <k> Bash tool calls in a row, each printing <bytes> bytes, then
                   "LOOP DONE <k>" (capacity runs: many events per turn)
  anything else    reply "ok"

Every request is appended to <log>/requests.jsonl (path, stream flag, model,
last user text, system prompt length and a marker check) and its full body to
<log>/bodies/<n>.json. Client disconnects mid-stream are logged too: that is how
an interrupt reaches the API. --no-bodies skips the bodies: claude sends the
whole conversation on every request, so over a long capacity run they grow
quadratically (16 GB after ~1000 requests).

Usage: mockapi.py <port> <logdir> [--no-bodies]
"""
import json
import os
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
LOG = sys.argv[2]
SAVE_BODIES = "--no-bodies" not in sys.argv[3:]
os.makedirs(os.path.join(LOG, "bodies"), exist_ok=True)
lock = threading.Lock()
seen = {}
counter = [0]


def log(rec):
    rec["t"] = round(time.time(), 3)
    with lock:
        with open(os.path.join(LOG, "requests.jsonl"), "a") as f:
            f.write(json.dumps(rec) + "\n")


def text_of(content):
    if isinstance(content, str):
        return content
    out = []
    for b in content or []:
        if b.get("type") == "text":
            out.append(b.get("text", ""))
    return "\n".join(out)


def route(body):
    """Return (scenario_text, tool_result_or_None)."""
    msgs = body.get("messages", [])
    last_user = None
    for m in reversed(msgs):
        if m.get("role") == "user":
            last_user = m
            break
    if last_user is None:
        return "", None
    c = last_user.get("content")
    # The scenario keyword is in the user's own text; system reminders are
    # separate blocks that start with "<system-reminder>". A new prompt can
    # share a message with the result of a tool its turn interrupted, so a
    # keyword wins over a tool result.
    t = text_of(c)
    lines = [l for l in t.splitlines() if l.strip() and not l.startswith("<")]
    for l in reversed(lines):
        w = l.strip().split()
        if w and w[0] in ("PING", "SLOW", "STALL", "TOOL", "MCP", "ERR", "BIG", "LOOP"):
            return l.strip(), None
    if isinstance(c, list):
        results = [b for b in c if b.get("type") == "tool_result"]
        if results:
            r = results[-1].get("content")
            rt = text_of(r) if isinstance(r, list) else str(r)
            return "", rt
    return (lines[-1].strip() if lines else ""), None


def loop_cmd(size, i):
    """A Bash command printing size bytes, tagged with the call's index."""
    return f"printf 'loop-{i} '; head -c {size} /dev/zero | tr '\\0' x; echo"


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        log({"path": self.path, "method": "GET"})
        self.send_response(404)
        self.send_header("content-length", "0")
        self.end_headers()

    def do_HEAD(self):
        self.send_response(200)
        self.send_header("content-length", "0")
        self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("content-length", "0"))
        raw = self.rfile.read(n)
        try:
            body = json.loads(raw)
        except Exception:
            body = {}
        with lock:
            counter[0] += 1
            idx = counter[0]
        if SAVE_BODIES:
            with open(os.path.join(LOG, "bodies", f"{idx:04d}.json"), "wb") as f:
                f.write(raw)
        if not self.path.startswith("/v1/messages") or "count_tokens" in self.path:
            log({"n": idx, "path": self.path, "note": "not messages"})
            payload = json.dumps({"input_tokens": 10}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        scen, tool_result = route(body)
        system = body.get("system")
        stext = system if isinstance(system, str) else "\n".join(
            b.get("text", "") for b in (system or []) if isinstance(b, dict))
        log({"n": idx, "path": self.path, "stream": bool(body.get("stream")),
             "model": body.get("model"), "scenario": scen,
             "tool_result": (tool_result or "")[:200] if tool_result is not None else None,
             "tools": [t.get("name") for t in body.get("tools", []) if isinstance(t, dict)][:80],
             "system_len": len(stext), "persona_marker": "PERSONA-MARKER-P11" in stext,
             "n_messages": len(body.get("messages", []))})
        w = scen.split()
        # Error scenarios.
        if w and w[0] == "ERR":
            code = int(w[1]) if len(w) > 1 else 529
            k = int(w[2]) if len(w) > 2 else 2
            with lock:
                seen[scen] = seen.get(scen, 0) + 1
                c = seen[scen]
            if c <= k:
                etype = {429: "rate_limit_error", 529: "overloaded_error"}.get(code, "api_error")
                payload = json.dumps({"type": "error", "error": {"type": etype, "message": f"mock {etype} {c}/{k}"}}).encode()
                self.send_response(code)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(payload)))
                if code == 429:
                    self.send_header("retry-after", "1")
                self.send_header("request-id", f"req_mock_{idx}")
                self.end_headers()
                self.wfile.write(payload)
                log({"n": idx, "note": f"sent {code} ({c}/{k})"})
                return
            return self.reply(body, text="RECOVERED")
        if tool_result is not None:
            # A tool finished: continue a LOOP, or close the scenario.
            prev = self.prev_scenario(body)
            if prev.startswith("LOOP"):
                pw = prev.split()
                k = int(pw[1]) if len(pw) > 1 else 10
                size = int(pw[2]) if len(pw) > 2 else 1000
                done = self.tool_calls_since(body, prev)
                if done < k:
                    return self.reply(body, tool=("Bash", {"command": loop_cmd(size, done + 1), "description": "capacity loop"}))
                return self.reply(body, text=f"LOOP DONE {k}")
            word = "MCP DONE" if prev.startswith("MCP") else "TOOL DONE"
            return self.reply(body, text=f"{word}: {tool_result.strip()[:60]}")
        if w and w[0] == "PING":
            return self.reply(body, text="PONG " + (w[1] if len(w) > 1 else ""))
        if w and w[0] == "SLOW":
            k = int(w[1]) if len(w) > 1 else 40
            return self.reply(body, chunks=["slow%d " % i for i in range(k)], delay=0.5)
        if w and w[0] == "STALL":
            s = float(w[1]) if len(w) > 1 else 60
            return self.reply(body, text="after stall", stall=s)
        if w and w[0] == "TOOL":
            cmd = scen[len("TOOL "):]
            return self.reply(body, tool=("Bash", {"command": cmd, "description": "probe"}))
        if w and w[0] == "MCP":
            return self.reply(body, tool=("mcp__probe__echo", {"text": "hello-mcp"}))
        if w and w[0] == "BIG":
            kib = int(w[1]) if len(w) > 1 else 512
            block = ("capacity " * 7282)[:65536]  # 64 KiB per delta
            chunks = [block] * (kib // 64) + ([block[:(kib % 64) * 1024]] if kib % 64 else [])
            return self.reply(body, chunks=chunks)
        if w and w[0] == "LOOP":
            size = int(w[2]) if len(w) > 2 else 1000
            return self.reply(body, tool=("Bash", {"command": loop_cmd(size, 1), "description": "capacity loop"}))
        return self.reply(body, text="ok")

    def prev_scenario(self, body):
        for m in reversed(body.get("messages", [])):
            if m.get("role") != "user":
                continue
            t = text_of(m.get("content"))
            for l in t.splitlines():
                if l.strip().split()[:1] and l.strip().split()[0] in ("MCP", "TOOL", "LOOP"):
                    return l.strip()
        return ""

    def tool_calls_since(self, body, scenario):
        """Count assistant tool_use blocks after the user message carrying scenario."""
        msgs = body.get("messages", [])
        start = 0
        for i in range(len(msgs) - 1, -1, -1):
            m = msgs[i]
            if m.get("role") == "user" and scenario in text_of(m.get("content")):
                start = i
                break
        n = 0
        for m in msgs[start:]:
            if m.get("role") == "assistant" and isinstance(m.get("content"), list):
                n += sum(1 for b in m["content"] if b.get("type") == "tool_use")
        return n

    def sse(self, event, data):
        chunk = f"event: {event}\ndata: {json.dumps(data)}\n\n".encode()
        self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
        self.wfile.flush()

    def reply(self, body, text=None, chunks=None, delay=0.0, stall=0.0, tool=None):
        model = body.get("model", "mock")
        mid = "msg_" + uuid.uuid4().hex[:20]
        if not body.get("stream"):
            content = []
            if tool:
                content.append({"type": "tool_use", "id": "toolu_" + uuid.uuid4().hex[:20], "name": tool[0], "input": tool[1]})
            else:
                content.append({"type": "text", "text": text if text is not None else "".join(chunks or [])})
            payload = json.dumps({"id": mid, "type": "message", "role": "assistant", "model": model,
                                  "content": content, "stop_reason": "tool_use" if tool else "end_turn",
                                  "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 5}}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("transfer-encoding", "chunked")
        self.send_header("cache-control", "no-cache")
        self.end_headers()
        try:
            self.sse("message_start", {"type": "message_start", "message": {
                "id": mid, "type": "message", "role": "assistant", "model": model, "content": [],
                "stop_reason": None, "stop_sequence": None,
                "usage": {"input_tokens": 10, "output_tokens": 1}}})
            if stall:
                end = time.time() + stall
                while time.time() < end:
                    time.sleep(1)
                    self.sse("ping", {"type": "ping"})
            if tool:
                tid = "toolu_" + uuid.uuid4().hex[:20]
                self.sse("content_block_start", {"type": "content_block_start", "index": 0,
                         "content_block": {"type": "tool_use", "id": tid, "name": tool[0], "input": {}}})
                self.sse("content_block_delta", {"type": "content_block_delta", "index": 0,
                         "delta": {"type": "input_json_delta", "partial_json": json.dumps(tool[1])}})
                self.sse("content_block_stop", {"type": "content_block_stop", "index": 0})
                stop = "tool_use"
            else:
                self.sse("content_block_start", {"type": "content_block_start", "index": 0,
                         "content_block": {"type": "text", "text": ""}})
                for c in (chunks if chunks is not None else [text]):
                    self.sse("content_block_delta", {"type": "content_block_delta", "index": 0,
                             "delta": {"type": "text_delta", "text": c}})
                    if delay:
                        time.sleep(delay)
                self.sse("content_block_stop", {"type": "content_block_stop", "index": 0})
                stop = "end_turn"
            self.sse("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                     "usage": {"output_tokens": 5}})
            self.sse("message_stop", {"type": "message_stop"})
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            log({"note": "client disconnected mid-stream", "msg": mid})


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
    srv.daemon_threads = True
    srv.serve_forever()
