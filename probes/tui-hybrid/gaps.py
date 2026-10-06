#!/usr/bin/env python3
"""Probe the two gaps of the TUI hybrid (FINDINGS.md): an interrupt before the
first token leaves no trace, and API retries in progress are invisible.

Each scenario runs claude's interactive TUI on a PTY against mockapi.py, as
probe.py does, with two side channels added:

  - OpenTelemetry logs (CLAUDE_CODE_ENABLE_TELEMETRY=1), exported as OTLP/HTTP
    JSON to a receiver in this process;
  - claude's debug log (--debug-file), tailed as it is written.

Every hook payload, transcript record, OTel log record and debug-log line lands
in <workdir>/<scenario>/timeline.jsonl with its arrival time; a digest is printed.

Usage: gaps.py <workdir> [scenario ...]
Scenarios: retry exhaust stall early midtext midtool queue background (default: all).
Each run appends one verdict line per scenario to <workdir>/results.jsonl and prints it.
CLAUDE_BIN picks the claude binary (default: claude on PATH).
Start the mock first: python3 mockapi.py 18712 <workdir>/mock
Needs pyte.
"""
import json
import os
import re
import subprocess
import uuid
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import probe  # noqa: E402

DEBUG_KEEP = re.compile(r"\[engine\]|\[onCancel\]|API error \(attempt|Error in API request|aborted|retry|interrupt|cancel", re.I)


def attr_value(v):
    for k in ("stringValue", "intValue", "doubleValue", "boolValue"):
        if k in v:
            return int(v[k]) if k == "intValue" else v[k]
    if "arrayValue" in v:
        return [attr_value(x) for x in v["arrayValue"].get("values", [])]
    return v


class OTLP:
    """Collects OTLP/HTTP JSON log records with their arrival time."""

    def __init__(self):
        self.records = []
        self.lock = threading.Lock()
        outer = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n)
                t = time.time()
                try:
                    doc = json.loads(body or b"{}")
                except Exception:
                    doc = {}
                for rl in doc.get("resourceLogs", []):
                    res = {a["key"]: attr_value(a["value"]) for a in rl.get("resource", {}).get("attributes", [])}
                    for sl in rl.get("scopeLogs", []):
                        for lr in sl.get("logRecords", []):
                            attrs = {a["key"]: attr_value(a["value"]) for a in lr.get("attributes", [])}
                            body_v = attr_value(lr.get("body", {})) if lr.get("body") else None
                            with outer.lock:
                                outer.records.append({"t": t, "event_t": int(lr.get("timeUnixNano", "0")) / 1e9,
                                                      "name": attrs.get("event.name") or body_v, "attrs": attrs,
                                                      "resource": res})
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b"{}")

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def for_session(self, sid):
        with self.lock:
            return [r for r in self.records if r["attrs"].get("session.id") == sid]


OTLP_SERVER = None


class GapSession(probe.Session):
    def __init__(self, work, name, env_extra=None):
        base = os.path.join(work, name)
        self.dbgpath = os.path.join(base, "debug.log")
        env = {
            "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
            "OTEL_LOGS_EXPORTER": "otlp",
            "OTEL_METRICS_EXPORTER": "none",
            "OTEL_TRACES_EXPORTER": "none",
            "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/json",
            "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:%d/v1/logs" % OTLP_SERVER.port,
            "OTEL_LOGS_EXPORT_INTERVAL": "200",
            "OTEL_BLRP_SCHEDULE_DELAY": "200",
        }
        env.update(env_extra or {})
        self.dbg_off = 0
        args = ("--debug-file", self.dbgpath)
        if os.environ.get("DEBUG_FILTER"):
            args += ("--debug", os.environ["DEBUG_FILTER"])
        super().__init__(work, name, env_extra=env, args=args)
        threading.Thread(target=self._tail_debug, daemon=True).start()

    def _tail_debug(self):
        while not self.stop:
            try:
                with open(self.dbgpath, errors="replace") as f:
                    f.seek(self.dbg_off)
                    data = f.read()
            except FileNotFoundError:
                data = ""
            cut = data.rfind("\n") + 1
            if cut:
                self.dbg_off += len(data[:cut].encode())
                now = time.time()
                for line in data[:cut].splitlines():
                    if DEBUG_KEEP.search(line):
                        self.add({"t": self.rel(now), "src": "debug", "line": line[:300]})
            time.sleep(0.05)

    def close(self):
        super().close()
        time.sleep(1.0)  # let the last OTel batch arrive
        for r in OTLP_SERVER.for_session(self.sid):
            a = r["attrs"]
            keep = {k: a[k] for k in a if k not in ("session.id", "user.id", "organization.id", "user.account_uuid",
                                                    "terminal.type", "app.version", "user.email", "event.timestamp")}
            self.timeline.append({"t": self.rel(r["t"]), "src": "otel", "name": r["name"],
                                  "event_t": round(r["event_t"] - self.t0, 3), "attrs": keep})
        with open(os.path.join(self.base, "timeline.jsonl"), "w") as f:
            for r in sorted(self.timeline, key=lambda r: r["t"]):
                f.write(json.dumps(r) + "\n")


def fresh(work, name, env_extra=None):
    s = GapSession(work, name, env_extra=env_extra)
    s.wait_hook("SessionStart", 0, 20)
    s.wait_screen("❯", 20)
    time.sleep(1.0)
    return s


def run_retry(work):
    s = fresh(work, "retry")
    probe.turn(s, "retry-recovers", "ERR 529 2 " + uuid.uuid4().hex[:6], timeout=60)
    s.close()
    return s


def run_exhaust(work):
    s = fresh(work, "exhaust", {"CLAUDE_CODE_MAX_RETRIES": "2"})
    probe.turn(s, "retry-exhausted", "ERR 529 9 " + uuid.uuid4().hex[:6], timeout=90)
    s.close()
    return s


def run_stall(work):
    s = fresh(work, "stall")

    def esc(t0):
        s.wait_hook("UserPromptSubmit", t0, 10)
        time.sleep(1.0)
        s.esc()
    probe.turn(s, "esc-before-first-token", "STALL 30", esc, timeout=6)
    s.snap("after-esc")
    s.write(b"\x15", "ctrl-u")
    time.sleep(0.3)
    probe.turn(s, "next-prompt", "PING 5")
    s.close()
    return s


def run_early(work):
    s = fresh(work, "early")

    def esc(t0):
        time.sleep(0.05)
        s.esc()
    probe.turn(s, "esc-50ms-after-enter", "SLOW 6", esc, timeout=6)
    s.write(b"\x15", "ctrl-u")
    time.sleep(0.3)
    probe.turn(s, "next-prompt", "PING 4")
    s.close()
    return s


def run_midtext(work):
    s = fresh(work, "midtext")

    def esc(t0):
        s.wait_hook("UserPromptSubmit", t0, 10)
        time.sleep(2.5)
        s.esc()
    probe.turn(s, "esc-mid-text", "SLOW 40", esc, timeout=6)
    s.close()
    return s


def run_midtool(work):
    s = fresh(work, "midtool")

    def esc(t0):
        s.wait_hook("PreToolUse", t0, 10)
        time.sleep(1.0)
        s.esc()
    probe.turn(s, "esc-mid-tool", "TOOL sleep 30", esc, timeout=6)
    pids = subprocess.run(["pgrep", "-f", "sleep 30"], capture_output=True, text=True).stdout.split()
    s.note("sleep-30-left", pids=pids)
    s.write(b"\x15", "ctrl-u")
    time.sleep(0.3)
    s.close()
    return s


def run_queue(work):
    s = fresh(work, "queue")
    s.note("scenario", label="send-while-busy")
    t = time.time()
    s.type("SLOW 8")
    s.wait_hook("UserPromptSubmit", t, 10)
    time.sleep(1.5)
    s.type("PING 6")
    s.wait_hook("Stop", t, 40, pred=lambda p: "PONG 6" in (p.get("last_assistant_message") or ""))
    time.sleep(1.0)
    s.snap("send-while-busy")
    s.close()
    return s


def run_background(work):
    s = fresh(work, "background")
    s.note("scenario", label="background-command")
    t = time.time()
    s.type("BG sleep 3; echo bg-done")
    s.wait_hook("Stop", t, 30)
    s.wait_hook("Stop", t, 40, pred=lambda p: "BG DONE" in (p.get("last_assistant_message") or ""))
    time.sleep(1.0)
    s.snap("background-command")
    s.close()
    return s


def evaluate(s, name):
    """One verdict per scenario, from the timeline alone."""
    tl = sorted(s.timeline, key=lambda r: r["t"])
    dbg = [(r["t"], r["line"]) for r in tl if r["src"] == "debug"]
    hooks = [r for r in tl if r["src"] == "hook"]
    recs = [r["rec"] for r in tl if r["src"] == "transcript"]
    esc = next((r["t"] for r in tl if r["src"] == "action" and r["what"] == "esc"), None)
    first = lambda pat: next((t for t, l in dbg if re.search(pat, l)), None)
    ms = lambda t: None if t is None or esc is None else int(round((t - esc) * 1000))
    ends = [re.search(r"turn (\d+) end .*stop=(\S+)", l) for t, l in dbg if "[engine] turn" in l and " end " in l]
    v = {"scenario": name}
    if name in ("stall", "early", "midtext", "midtool"):
        end1 = first(r"\[engine\] turn 1 end")
        v.update({
            "onCancel_ms": ms(first(r"\[onCancel\]")),
            "turn_end_ms": ms(end1),
            "stop": ends[0].group(2) if ends and ends[0] else None,
            "interrupt_record": any("[Request interrupted" in json.dumps(r) for r in recs),
        })
        # stop is null when a reply was cut, tool_use when a running tool was stopped
        v["settled"] = v["onCancel_ms"] is not None and v["turn_end_ms"] is not None and v["turn_end_ms"] < 1500
        if name == "midtool":
            note = next((r for r in tl if r["src"] == "action" and r["what"] == "sleep-30-left"), {})
            v["sleep_left"] = len(note.get("pids", []))
    elif name in ("retry", "exhaust"):
        v.update({
            "attempts": [re.search(r"attempt (\d+/\d+)", l).group(1) for t, l in dbg if "API error (attempt" in l],
            "stop": ends[0].group(2) if ends and ends[0] else None,
            "Stop": any(h["event"] == "Stop" for h in hooks),
            "StopFailure": any(h["event"] == "StopFailure" for h in hooks),
        })
        want = ("end_turn", True, False) if name == "retry" else ("stop_sequence", False, True)
        v["settled"] = bool(v["attempts"]) and (v["stop"], v["Stop"], v["StopFailure"]) == want
    elif name == "queue":
        subs = [h for h in hooks if h["event"] == "UserPromptSubmit"]
        v.update({
            "submits": [(h["payload"].get("prompt"), (h.get("prompt_id") or "")[:8]) for h in subs],
            "engine_turns": sum(1 for t, l in dbg if re.search(r"\[engine\] turn \d+ start", l)),
            "stops": [h["payload"].get("last_assistant_message", "")[:20] for h in hooks if h["event"] == "Stop"],
            "queue_operation": any(r.get("type") == "queue-operation" for r in recs),
        })
        # Both turns end. Their Stop hooks (and debug lines) may arrive in either order: each
        # hook carries its prompt_id, so order is reported, not required.
        v["stops_in_order"] = bool(v["stops"]) and "PONG 6" in v["stops"][-1]
        v["settled"] = v["engine_turns"] == 2 and len(v["stops"]) == 2 and any("PONG 6" in x for x in v["stops"])
    elif name == "background":
        subs = [h for h in hooks if h["event"] == "UserPromptSubmit"]
        v.update({
            "submits": [(h["payload"].get("prompt", "")[:30], h["payload"].get("source")) for h in subs],
            "engine_turns": sum(1 for t, l in dbg if re.search(r"\[engine\] turn \d+ start", l)),
            "stops": [h["payload"].get("last_assistant_message", "")[:20] for h in hooks if h["event"] == "Stop"],
            "notification_record": any("task-notification" in json.dumps(r) for r in recs if r.get("type") == "user"),
        })
        v["settled"] = v["engine_turns"] >= 2 and any("BG DONE" in x for x in v["stops"])
    return v


def digest(s):
    print("\n######## %s  sid=%s" % (os.path.basename(s.base), s.sid))
    for r in sorted(s.timeline, key=lambda r: r["t"]):
        src = r["src"]
        if src == "action":
            if r["what"] == "scenario":
                print("\n=== " + r["label"])
            else:
                print("%8.3f ACT   %s %s" % (r["t"], r["what"], r.get("bytes", "")))
        elif src == "hook":
            if r["event"] == "MessageDisplay":
                continue
            p = dict(r["payload"])
            p.pop("prompt_id", None)
            print("%8.3f HOOK  %-18s pid=%s %s" % (r["t"], r["event"], (r.get("prompt_id") or "-")[:8], json.dumps(p)[:160]))
        elif src == "transcript":
            rec = r["rec"]
            if rec.get("type") in ("user", "assistant", "system"):
                print("%8.3f TR    %s" % (r["t"], json.dumps({k: rec.get(k) for k in ("type", "subtype", "uuid", "parentUuid", "promptId", "stop_reason", "isApiErrorMessage", "error", "text", "blocks") if rec.get(k) is not None})[:230]))
        elif src == "otel":
            a = r["attrs"]
            short = {k: a[k] for k in a if k in ("prompt.id", "attempt", "status_code", "error", "duration_ms", "total_attempts", "ttft_ms", "decision", "tool_name", "success")}
            if "prompt.id" in short:
                short["prompt.id"] = str(short["prompt.id"])[:8]
            print("%8.3f OTEL  %-30s (event_t %.3f) %s" % (r["t"], r["name"], r["event_t"], json.dumps(short)[:200]))
        elif src == "debug":
            print("%8.3f DBG   %s" % (r["t"], r["line"][:200]))
        elif src == "screen":
            tail = "\n".join(l for l in r["text"].splitlines()[-8:] if l.strip())
            print("%8.3f SCREEN [%s]\n%s" % (r["t"], r["label"], "\n".join("          | " + l for l in tail.splitlines())))


if __name__ == "__main__":
    work = os.path.abspath(sys.argv[1])
    os.makedirs(work, exist_ok=True)
    OTLP_SERVER = OTLP()
    binary = os.environ.get("CLAUDE_BIN", "claude")
    version = subprocess.run([binary, "--version"], capture_output=True, text=True).stdout.split()[0]
    runs = {"retry": run_retry, "exhaust": run_exhaust, "stall": run_stall, "early": run_early,
            "midtext": run_midtext, "midtool": run_midtool, "queue": run_queue, "background": run_background}
    for name in sys.argv[2:] or list(runs):
        s = runs[name](work)
        if os.environ.get("DIGEST"):
            digest(s)
        v = evaluate(s, name)
        v["claude"] = version
        with open(os.path.join(work, "results.jsonl"), "a") as f:
            f.write(json.dumps(v) + "\n")
        print(json.dumps(v), flush=True)
