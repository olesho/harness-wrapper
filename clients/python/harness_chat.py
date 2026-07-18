"""Minimal client for harness-chatd (HTTP + SSE).

Stdlib only. Usage:

    from harness_chat import Client

    client = Client("http://127.0.0.1:8080")
    conv = client.open(harness="codex", binary_path="/usr/local/bin/codex")
    try:
        with conv.control():
            turn_id = conv.send("summarize this project")
            for ev in conv.events():
                if ev.turn.id == turn_id and ev.turn.state == "complete":
                    break
        print(conv.history())
    finally:
        conv.close()
"""

from __future__ import annotations

import contextlib
import json
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any, Iterator


class HarnessChatError(RuntimeError):
    def __init__(self, status: int, code: str, message: str):
        super().__init__(f"{status} {code}: {message}")
        self.status = status
        self.code = code
        self.message = message


@dataclass
class Turn:
    id: str
    session_id: str
    role: str
    state: str
    text: str = ""
    reason: str = ""
    started_at: str = ""
    completed_at: str = ""
    # Populated when the wrapper recognized an upstream API error. Code
    # 0 means transport error (no HTTP code surfaced by the harness).
    # retry_after is a Go-duration string ("30s", "2m"); empty when the
    # harness did not include a parseable hint.
    http_code: int = 0
    retry_after: str = ""

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> "Turn":
        return cls(
            id=d.get("id", ""),
            session_id=d.get("session_id", ""),
            role=d.get("role", ""),
            state=d.get("state", ""),
            text=d.get("text", ""),
            reason=d.get("reason", ""),
            started_at=d.get("started_at", ""),
            completed_at=d.get("completed_at", ""),
            http_code=int(d.get("http_code", 0)),
            retry_after=d.get("retry_after", ""),
        )


@dataclass
class TurnEvent:
    turn: Turn
    error: str = ""

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> "TurnEvent":
        return cls(turn=Turn.from_json(d.get("turn", {})), error=d.get("error", ""))


def _parse_sse_block(lines: list[str]) -> str | None:
    """Join the ``data:`` payloads of one SSE block; None when it has none."""
    data = "\n".join(s[5:].lstrip() for s in lines if s.startswith("data:"))
    return data or None


class Client:
    def __init__(self, base_url: str, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def open(
        self,
        *,
        harness: str,
        binary_path: str,
        args: list[str] | None = None,
        working_dir: str = "",
        env: list[str] | None = None,
        cols: int = 0,
        rows: int = 0,
    ) -> "Conversation":
        body = {
            "harness": harness,
            "binary_path": binary_path,
            "args": args or [],
            "working_dir": working_dir,
            "env": env or [],
            "cols": cols,
            "rows": rows,
        }
        resp = self._request("POST", "/v1/conversations", body)
        return Conversation(self, resp["id"])

    def list(self) -> list[dict[str, Any]]:
        return self._request("GET", "/v1/conversations") or []

    # --- internals ---
    def _request(self, method: str, path: str, body: Any = None) -> Any:
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(
            self.base_url + path, data=data, method=method, headers=headers
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
                if not raw:
                    return None
                return json.loads(raw)
        except urllib.error.HTTPError as e:
            payload = e.read()
            try:
                err = json.loads(payload)
                raise HarnessChatError(
                    e.code, err.get("code", ""), err.get("error", str(e))
                ) from None
            except (ValueError, AttributeError):
                raise HarnessChatError(e.code, "", payload.decode("utf-8", "replace")) from None


class Conversation:
    def __init__(self, client: Client, id: str):
        self.client = client
        self.id = id
        self._token: str | None = None

    def acquire(self) -> str:
        resp = self.client._request("POST", f"/v1/conversations/{self.id}/control")
        self._token = resp["token"]
        return self._token

    def release(self) -> None:
        if self._token is None:
            return
        tok, self._token = self._token, None
        self.client._request("DELETE", f"/v1/conversations/{self.id}/control/{tok}")

    @contextlib.contextmanager
    def control(self) -> Iterator[None]:
        self.acquire()
        try:
            yield
        finally:
            self.release()

    def send(self, text: str) -> str:
        if self._token is None:
            raise HarnessChatError(409, "no_control", "acquire control before send()")
        resp = self.client._request(
            "POST",
            f"/v1/conversations/{self.id}/messages",
            {"token": self._token, "text": text},
        )
        return resp["turn_id"]

    def history(self) -> list[Turn]:
        resp = self.client._request("GET", f"/v1/conversations/{self.id}/history")
        return [Turn.from_json(t) for t in resp.get("turns", [])]

    def close(self) -> None:
        try:
            self.client._request("DELETE", f"/v1/conversations/{self.id}")
        except HarnessChatError as e:
            if e.status != 404:
                raise

    def events(self) -> Iterator[TurnEvent]:
        """SSE stream of turn events. Caller must break out to stop."""
        url = f"{self.client.base_url}/v1/conversations/{self.id}/events"
        req = urllib.request.Request(url, headers={"Accept": "text/event-stream"})
        # Long-lived: no urlopen timeout.
        with urllib.request.urlopen(req) as resp:
            buf: list[str] = []
            for raw in resp:
                line = raw.decode("utf-8", "replace").rstrip("\n").rstrip("\r")
                if line == "":
                    data = _parse_sse_block(buf)
                    buf.clear()
                    if data is not None:
                        yield TurnEvent.from_json(json.loads(data))
                    continue
                if line.startswith(":"):  # comment / heartbeat
                    continue
                buf.append(line)
