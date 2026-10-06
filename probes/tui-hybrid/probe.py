#!/usr/bin/env python3
"""Probe: claude's interactive TUI (no -p) driven on a PTY for input and interrupts
only, with turn state read from hooks and the transcript JSONL.

Runs against mockapi.py (start it first: python3 mockapi.py 18712 <workdir>/mock).
Each scenario's timeline — actions, hook payloads, transcript records, screen
snapshots — lands in <workdir>/<session>/timeline.jsonl, plus a digest on stdout.

Usage: probe.py <workdir> [session ...]      sessions: main exhaust
Needs pyte (python -m pip install pyte) for screen snapshots.
"""
import fcntl
import json
import os
import shutil
import struct
import subprocess
import sys
import termios
import threading
import time
import uuid

import pyte

HERE = os.path.dirname(os.path.abspath(__file__))
PORT = int(os.environ.get("MOCK_PORT", "18712"))
TOKEN = "probe-placeholder-token"
COLS, ROWS = 120, 40
EVENTS = ("SessionStart", "SessionEnd", "UserPromptSubmit", "UserPromptExpansion",
          "PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch", "Stop",
          "StopFailure", "Notification", "MessageDisplay", "PermissionRequest",
          "PermissionDenied", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact")
MATCHED = {"PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest",
           "PermissionDenied"}


def hook_settings():
    cmd = f"{sys.executable} {os.path.join(HERE, 'hook.py')}"
    hooks = {}
    for e in EVENTS:
        entry = {"hooks": [{"type": "command", "command": cmd}]}
        if e in MATCHED:
            entry["matcher"] = "*"
        hooks[e] = [entry]
    return {"hooks": hooks, "skipDangerousModePermissionPrompt": True}


class Session:
    def __init__(self, work, name, env_extra=None, args=()):
        self.base = os.path.join(work, name)
        shutil.rmtree(self.base, ignore_errors=True)
        self.cfg = os.path.join(self.base, "cfg")
        self.cwd = os.path.realpath(os.path.join(self.base, "cwd"))
        os.makedirs(self.cfg)
        os.makedirs(self.cwd)
        self.cwd = os.path.realpath(self.cwd)
        json.dump({"hasCompletedOnboarding": True, "bypassPermissionsModeAccepted": True,
                   "projects": {self.cwd: {"hasTrustDialogAccepted": True}}},
                  open(os.path.join(self.cfg, ".claude.json"), "w"))
        json.dump(hook_settings(), open(os.path.join(self.cfg, "settings.json"), "w"))
        self.hooklog = os.path.join(self.base, "hooks.jsonl")
        open(self.hooklog, "w").close()
        self.sid = str(uuid.uuid4())
        env = {k: v for k, v in os.environ.items()
               if not k.startswith("CLAUDE") and not k.startswith("ANTHROPIC")}
        env.update({"CLAUDE_CONFIG_DIR": self.cfg, "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{PORT}",
                    "ANTHROPIC_AUTH_TOKEN": TOKEN, "DISABLE_AUTOUPDATER": "1",
                    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "TERM": "xterm-256color",
                    "HOOKLOG": self.hooklog})
        env.update(env_extra or {})
        self.t0 = time.time()
        self.timeline = []
        self.lock = threading.Lock()
        self.screen = pyte.Screen(COLS, ROWS)
        self.stream = pyte.ByteStream(self.screen)
        self.raw = open(os.path.join(self.base, "pty.raw"), "wb")
        master, slave = os.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
        argv = [os.environ.get("CLAUDE_BIN", "claude"), "--session-id", self.sid, "--dangerously-skip-permissions",
                "--model", "claude-haiku-4-5"] + list(args)
        self.proc = subprocess.Popen(argv, cwd=self.cwd, env=env, stdin=slave, stdout=slave,
                                     stderr=slave, start_new_session=True)
        os.close(slave)
        self.master = master
        self.note("launch", argv=argv[1:])
        threading.Thread(target=self._pty, daemon=True).start()
        self.tpath = os.path.join(self.cfg, "projects", self.cwd.replace("/", "-").replace(".", "-"),
                                  self.sid + ".jsonl")
        self.hook_off = 0
        self.tr_off = 0
        self.hooks = []
        self.records = []
        self.stop = False
        threading.Thread(target=self._tail, daemon=True).start()

    def rel(self, t=None):
        return round((t or time.time()) - self.t0, 3)

    def add(self, rec):
        with self.lock:
            self.timeline.append(rec)

    def note(self, what, **kw):
        self.add({"t": self.rel(), "src": "action", "what": what, **kw})

    def _pty(self):
        while True:
            try:
                b = os.read(self.master, 65536)
            except OSError:
                break
            if not b:
                break
            self.raw.write(b)
            with self.lock:
                try:
                    self.stream.feed(b)
                except Exception:  # pyte gaps (e.g. private DSR); keep draining the PTY
                    self.stream = pyte.ByteStream(self.screen)

    def _tail(self):
        while not self.stop:
            self._drain()
            time.sleep(0.02)

    def _drain(self):
        with open(self.hooklog) as f:
            f.seek(self.hook_off)
            data = f.read()
        if data.endswith("\n"):
            self.hook_off += len(data.encode())
            for line in data.splitlines():
                h = json.loads(line)
                p = h["payload"]
                self.hooks.append(h)
                self.add({"t": self.rel(h["t"]), "src": "hook", "event": p.get("hook_event_name"),
                          "prompt_id": p.get("prompt_id"), "payload": slim_hook(p)})
        if os.path.exists(self.tpath):
            with open(self.tpath) as f:
                f.seek(self.tr_off)
                data = f.read()
            cut = data.rfind("\n") + 1
            if cut:
                self.tr_off += len(data[:cut].encode())
                now = time.time()
                for line in data[:cut].splitlines():
                    r = json.loads(line)
                    self.records.append(r)
                    self.add({"t": self.rel(now), "src": "transcript", "rec": slim_rec(r)})

    def screen_text(self):
        with self.lock:
            lines = [l.rstrip() for l in self.screen.display]
        while lines and not lines[-1]:
            lines.pop()
        return "\n".join(lines)

    def snap(self, label):
        self.add({"t": self.rel(), "src": "screen", "label": label, "text": self.screen_text()})

    def write(self, b, what):
        self.note(what, bytes=repr(b))
        os.write(self.master, b)

    def type(self, text):
        self.write(text.encode(), "type")
        time.sleep(0.15)
        self.write(b"\r", "enter")

    def esc(self):
        self.write(b"\x1b", "esc")

    def wait_hook(self, event, since, timeout=30, pred=None):
        end = time.time() + timeout
        while time.time() < end:
            for h in self.hooks:
                p = h["payload"]
                if h["t"] >= since and p.get("hook_event_name") == event and (pred is None or pred(p)):
                    return h
            time.sleep(0.02)
        self.note("timeout", waiting_for=event)
        return None

    def wait_any(self, events, since, timeout=30):
        end = time.time() + timeout
        while time.time() < end:
            for h in self.hooks:
                if h["t"] >= since and h["payload"].get("hook_event_name") in events:
                    return h
            time.sleep(0.02)
        self.note("timeout", waiting_for=list(events))
        return None

    def wait_screen(self, needle, timeout=20):
        end = time.time() + timeout
        while time.time() < end:
            if needle in self.screen_text():
                return True
            time.sleep(0.05)
        return False

    def close(self):
        self.note("close")
        try:
            self.write(b"\x03", "ctrl-c")
            time.sleep(0.3)
            self.write(b"\x03", "ctrl-c")
            self.proc.wait(timeout=10)
        except Exception:
            self.proc.kill()
        time.sleep(0.5)
        self._drain()
        self.stop = True
        self.note("exited", code=self.proc.returncode)
        with open(os.path.join(self.base, "timeline.jsonl"), "w") as f:
            for r in sorted(self.timeline, key=lambda r: r["t"]):
                f.write(json.dumps(r) + "\n")


def slim_hook(p):
    keep = {}
    for k, v in p.items():
        if k in ("session_id", "transcript_path", "cwd", "hook_event_name", "permission_mode"):
            continue
        if isinstance(v, str) and len(v) > 160:
            v = v[:160] + "…"
        keep[k] = v
    return keep


def slim_rec(r):
    out = {"type": r.get("type")}
    for k in ("subtype", "uuid", "parentUuid", "promptId", "isMeta", "level", "toolUseID",
              "isApiErrorMessage", "error", "content"):
        if k in r and k != "content":
            out[k] = r[k]
    m = r.get("message")
    if isinstance(m, dict):
        out["role"] = m.get("role")
        out["stop_reason"] = m.get("stop_reason")
        c = m.get("content")
        if isinstance(c, str):
            out["text"] = c[:160]
        elif isinstance(c, list):
            out["blocks"] = [{"type": b.get("type"),
                              **({"text": str(b.get("text", ""))[:120]} if b.get("type") == "text" else {}),
                              **({"name": b.get("name")} if b.get("type") == "tool_use" else {}),
                              **({"content": str(b.get("content"))[:120]} if b.get("type") == "tool_result" else {})}
                             for b in c]
    elif "content" in r:
        out["content"] = str(r["content"])[:160]
    extra = sorted(set(r) - {"type", "subtype", "uuid", "parentUuid", "message", "sessionId", "cwd",
                             "version", "gitBranch", "userType", "isSidechain", "timestamp",
                             "requestId", "entrypoint", "slug"})
    out["keys"] = extra
    return out


def turn(s, label, prompt, then=None, wait=("Stop", "StopFailure"), timeout=40):
    s.note("scenario", label=label)
    t = time.time()
    s.type(prompt)
    if then:
        then(t)
    h = s.wait_any(wait, t, timeout)
    time.sleep(1.0)
    s.snap(label)
    return h


def run_main(work):
    s = Session(work, "main")
    t = time.time()
    s.wait_hook("SessionStart", 0, 20)
    ready = s.wait_screen("❯", 20)
    s.note("composer-visible", ok=ready)
    time.sleep(1.0)
    s.snap("ready")

    turn(s, "01-ping", "PING 1")
    turn(s, "02-tool", "TOOL echo hi")

    def esc_after_display(t0):
        s.wait_hook("MessageDisplay", t0, 10)
        time.sleep(1.0)
        s.esc()
    turn(s, "03-interrupt-mid-text", "SLOW 40", esc_after_display, timeout=8)
    turn(s, "03b-after-interrupt", "PING 2")

    def esc_mid_tool(t0):
        s.wait_hook("PreToolUse", t0, 10)
        time.sleep(1.0)
        s.esc()
    turn(s, "04-interrupt-mid-tool", "TOOL sleep 30", esc_mid_tool, timeout=8)
    turn(s, "04b-after-interrupt", "PING 3")

    def esc_stall(t0):
        s.wait_hook("UserPromptSubmit", t0, 10)
        time.sleep(1.0)
        s.esc()
    turn(s, "05-interrupt-before-token", "STALL 30", esc_stall, timeout=8)

    def esc_early(t0):
        time.sleep(0.05)
        s.esc()
    turn(s, "06-esc-right-after-enter", "PING 4", esc_early, timeout=8)
    turn(s, "06b-after", "PING 5")

    turn(s, "07-api-retry-recovers", "ERR 529 2", timeout=60)

    def queue_second(t0):
        s.wait_hook("MessageDisplay", t0, 10)
        time.sleep(0.5)
        s.type("PING 6")
    s.note("scenario", label="08-send-while-busy")
    t = time.time()
    s.type("SLOW 8")
    queue_second(t)
    s.wait_hook("Stop", t, 30, pred=lambda p: "PONG 6" in (p.get("last_assistant_message") or ""))
    time.sleep(1.0)
    s.snap("08-send-while-busy")
    s.close()
    return s


def run_exhaust(work):
    s = Session(work, "exhaust", env_extra={"CLAUDE_CODE_MAX_RETRIES": "1"})
    s.wait_hook("SessionStart", 0, 20)
    s.wait_screen("❯", 20)
    time.sleep(1.0)
    turn(s, "09-api-error-exhausted", "ERR 529 9", timeout=60)
    turn(s, "09b-after", "PING 7")
    s.close()
    return s


def fresh(work, name, env_extra=None):
    s = Session(work, name, env_extra=env_extra)
    s.wait_hook("SessionStart", 0, 20)
    s.wait_screen("❯", 20)
    time.sleep(1.0)
    return s


def run_stall(work):
    s = fresh(work, "stall")

    def esc_stall(t0):
        s.wait_hook("UserPromptSubmit", t0, 10)
        time.sleep(1.0)
        s.esc()
    turn(s, "05-interrupt-before-token", "STALL 30", esc_stall, timeout=6)
    s.write(b"\x15", "ctrl-u")  # clear the restored composer
    time.sleep(0.3)
    s.snap("05-after-ctrl-u")
    turn(s, "05b-after", "PING 5")
    s.close()
    return s


def run_early(work):
    s = fresh(work, "early")

    def esc_early(t0):
        time.sleep(0.05)
        s.esc()
    turn(s, "06-esc-right-after-enter", "SLOW 6", esc_early, timeout=6)
    s.write(b"\x15", "ctrl-u")
    time.sleep(0.3)
    turn(s, "06b-after", "PING 4")
    s.close()
    return s


def run_retry(work):
    s = fresh(work, "retry")
    turn(s, "07-api-retry-recovers", "ERR 529 2", timeout=60)
    s.close()
    return s


def run_queue(work):
    s = fresh(work, "queue")
    s.note("scenario", label="08-send-while-busy")
    t = time.time()
    s.type("SLOW 8")
    s.wait_hook("UserPromptSubmit", t, 10)
    time.sleep(1.5)  # mid-reply: SLOW 8 streams for ~4 s
    s.type("PING 6")
    s.wait_hook("Stop", t, 30, pred=lambda p: "PONG 6" in (p.get("last_assistant_message") or ""))
    time.sleep(1.0)
    s.snap("08-send-while-busy")
    s.close()
    return s


def digest(s):
    print(f"\n######## {os.path.basename(s.base)}  sid={s.sid}")
    for r in sorted(s.timeline, key=lambda r: r["t"]):
        if r["src"] == "action":
            if r["what"] == "scenario":
                print(f"\n=== {r['label']}")
            else:
                print(f"{r['t']:8.3f} ACT  {r['what']} {r.get('bytes', '')}")
        elif r["src"] == "hook":
            p = dict(r["payload"])
            pid = (r.get("prompt_id") or "-")[:8]
            p.pop("prompt_id", None)
            print(f"{r['t']:8.3f} HOOK {r['event']:<18} pid={pid} {json.dumps(p)[:230]}")
        elif r["src"] == "transcript":
            print(f"{r['t']:8.3f} TR   {json.dumps(r['rec'])[:260]}")
        elif r["src"] == "screen":
            tail = "\n".join(r["text"].splitlines()[-12:])
            print(f"{r['t']:8.3f} SCREEN [{r['label']}]\n" + "\n".join("         | " + l for l in tail.splitlines()))


if __name__ == "__main__":
    work = os.path.abspath(sys.argv[1])
    which = sys.argv[2:] or ["main", "exhaust"]
    for w in which:
        digest({"main": run_main, "exhaust": run_exhaust, "stall": run_stall, "early": run_early,
                "retry": run_retry, "queue": run_queue}[w](work))
