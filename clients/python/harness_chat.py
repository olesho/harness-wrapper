"""Minimal client for harness-chatd (HTTP + SSE).

Stdlib only. Usage:

    from harness_chat import Client

    client = Client("http://127.0.0.1:8080")
    conv = client.open(harness="codex", binary_path="/usr/local/bin/codex")
    try:
        with conv.control():
            turn_id = conv.send("summarize this project")
            for ev in conv.events():
                if ev.type != "turn" or ev.turn is None:
                    continue
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
from typing import Any, Iterator, Literal

# Mirrors isSupportedEffort in pkg/wrapper/wrapper.go. Documentation / IDE
# value only: the clients are deliberately thin transports, so nothing here
# validates at runtime and no type checker is configured in this repo.
Effort = Literal["low", "medium", "high", "xhigh", "max"]

# Mirrors isSupportedPermissionMode in pkg/wrapper/wrapper.go: the canonical
# rungs (least to most permissive) followed by the harness-native spellings.
# Documentation / IDE value only, same as Effort -- nothing validates at
# runtime. The alias is flat but the server is not: acceptEdits / dontAsk /
# bypassPermissions are claude / claude-code only, and read-only /
# workspace-write / danger-full-access are codex only; crossing them is a 400
# ``invalid_config`` rather than a silent no-op.
PermissionMode = Literal[
    "plan",
    "manual",
    "ask",
    "auto",
    "bypass",
    "acceptEdits",
    "dontAsk",
    "bypassPermissions",
    "read-only",
    "workspace-write",
    "danger-full-access",
]


@dataclass
class Containment:
    """An optional Landlock containment request (Linux, ABI 9+): an extra,
    kernel-enforced boundary around the harness, outside whatever its own
    permission settings enforce. Mirrors ``containment.Request`` in
    ``pkg/containment``; the server validates it, and a request it cannot
    honour is a 400 ``invalid_config`` -- never a silent downgrade.

    Serialization preserves exactly what is set: ``None`` fields are omitted,
    ``restrict_tcp=True`` with no (or an empty) ``connect_tcp`` means deny all
    TCP, and ``connect_tcp`` without ``restrict_tcp`` is sent as written and
    rejected by the server rather than "fixed" here.
    """

    kind: str = "landlock"
    read_only: list[str] | None = None
    read_write: list[str] | None = None
    restrict_tcp: bool | None = None
    connect_tcp: list[int] | None = None
    min_abi: int | None = None
    state_dir: str | None = None
    pass_env: list[str] | None = None

    def to_json(self) -> dict[str, Any]:
        body: dict[str, Any] = {"kind": self.kind}
        for key in (
            "read_only",
            "read_write",
            "restrict_tcp",
            "connect_tcp",
            "min_abi",
            "state_dir",
            "pass_env",
        ):
            value = getattr(self, key)
            if value is not None:
                body[key] = value
        return body


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
    """One SSE frame: a discriminated envelope keyed on ``type``.

    ``turn`` is None for frames that carry no turn (``input_request`` /
    ``input_resolved``, which carry ``input`` instead), so always check
    ``type`` and guard ``turn is not None`` before dereferencing it.
    """

    type: str = ""
    turn: Turn | None = None
    input: dict[str, Any] | None = None
    # How the harness process ended: set on the last frame, type "exited".
    exit: dict[str, Any] | None = None
    error: str = ""

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> "TurnEvent":
        raw = d.get("turn")
        return cls(
            type=d.get("type", ""),
            turn=Turn.from_json(raw) if raw is not None else None,
            input=d.get("input"),
            exit=d.get("exit"),
            error=d.get("error", ""),
        )


def _parse_sse_block(lines: list[str]) -> str | None:
    """Join the ``data:`` payloads of one SSE block; None when it has none."""
    data = "\n".join(s[5:].lstrip() for s in lines if s.startswith("data:"))
    return data or None


_UNSET: Any = object()


class Client:
    def __init__(self, base_url: str, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self._caps: Any = _UNSET

    def capabilities(self) -> dict[str, Any] | None:
        """GET /v1/capabilities, or None for a harness-chatd built before the
        route existed (404). Cached per client."""
        if self._caps is _UNSET:
            try:
                self._caps = self._request("GET", "/v1/capabilities")
            except HarnessChatError as e:
                if e.status != 404:
                    raise
                self._caps = None
        return self._caps

    def _require_containment(self, kind: str) -> None:
        """Raise unless the server lists ``kind`` among its containment kinds:
        an older harness-chatd would silently drop the field and run the
        harness uncontained."""
        caps = self.capabilities()
        kinds = list(((caps or {}).get("containment") or {}).get("kinds") or [])
        if caps is None:
            raise HarnessChatError(
                0,
                "containment_unsupported",
                "server predates containment (no /v1/capabilities); refusing to send a request it would ignore",
            )
        if kind not in kinds:
            raise HarnessChatError(
                0,
                "containment_unsupported",
                f'server does not support containment kind "{kind}" (supports: {", ".join(kinds) or "none"})',
            )

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
        effort: Effort | None = None,
        model: str | None = None,
        permission_mode: PermissionMode | None = None,
        containment: Containment | None = None,
    ) -> "Conversation":
        """Open a conversation. ``effort``, ``model`` and ``permission_mode``
        are optional knobs translated per-harness by the server; all three are
        omitted from the request body entirely when left as None.

        effort/model behavior (server-side, pkg/wrapper/wrapper.go):

        1. ``effort`` is validated and hard-fails; ``model`` is not validated at
           all. A non-enum effort is rejected, as is any effort on a harness
           that does not support it. ``model`` on an unsupported harness is
           silently ignored -- so ``model`` on "pi" is a SILENT NO-OP while
           ``effort`` on "pi" is an ERROR.
        2. On the harness-chatd gateway the effort-capable harness names are
           exactly "codex" and "claude-code", case-sensitively. ``effort``
           against "opencode", "pi" or "generic"/"" is a 400 ``invalid_options``
           -- the exact opposite of ``model``'s behavior on those same harnesses
           (silent no-op, see 1); do not assume symmetry. The gateway accepts
           only "codex", "claude-code", "opencode", "pi" and "generic"/"": plain
           "claude" is a 400 ``unknown_harness`` before effort is even
           considered, and so is "Codex" (the gateway does not normalize case).
        3. An explicit flag already present in ``args`` wins over the typed
           field: the server bails out of both the effort and the model rewrite
           when the flag / config key is already there. If you are migrating off
           the raw-``args`` escape hatch, drop the old flags or they silently
           win.
        4. codex remaps "max" -> "xhigh", so ``effort="max"`` against codex
           reaches the harness as ``model_reasoning_effort="xhigh"``.

        permission_mode behavior (server-side, pkg/wrapper/wrapper.go):

        1. Like ``effort`` and unlike ``model``, it is validated and hard-fails:
           an unknown mode, a native spelling belonging to the *other* harness,
           or any mode at all on a harness with no permission axis ("opencode",
           "pi", "generic"/"") is a 400 ``invalid_config`` before launch. A mode
           the caller believes restricts the harness is never dropped.
        2. "plan" is REJECTED on codex -- codex has no launch-time flag for the
           plan rung, and a no-op would launch it unrestricted. Codex plan mode
           is reachable only in-band, by sending "/plan" after opening.
        3. An explicit permission-axis flag already in ``args`` wins silently:
           --permission-mode / --dangerously-skip-permissions (claude,
           claude-code) and -s / --sandbox / -a / --ask-for-approval /
           --dangerously-bypass-approvals-and-sandbox (codex). The two
           --dangerously-* arms are reachable only for a bypass-class mode; any
           other mode paired with them is rejected (400), not suppressed.
        4. Restrictive rungs ("plan", "manual", "ask") are fully enforced only
           with a human at the TUI. Unattended, claude's permission dialogs are
           not detected (the turn stalls to the deadline) and codex's approval
           prompts are auto-approved -- only the -s sandbox axis still binds.
        5. "bypass" over the gateway carries no IS_SANDBOX=1 (chatd has no
           --sandbox-defaults), so pass IS_SANDBOX=1 in ``env`` or an
           ``input_policy`` with by_kind {"trust_prompt": ...}; otherwise
           claude-code stops on its acceptance screen as a trust_prompt input
           request and root is disallowed.

        containment (Landlock, Linux) is fixed for the conversation's life. It
        is sent only after GET /v1/capabilities lists its kind -- against an
        older server ``open`` raises ``containment_unsupported`` without
        posting -- and the server's echo of the applied policy is checked: a
        response without it closes the conversation and raises
        ``containment_not_applied``. The applied policy is
        ``Conversation.containment``.
        """
        if containment is not None:
            self._require_containment(containment.kind)
        body = {
            "harness": harness,
            "binary_path": binary_path,
            "args": args or [],
            "working_dir": working_dir,
            "env": env or [],
            "cols": cols,
            "rows": rows,
        }
        # Presence, not truthiness: an explicit "" is sent as "" (a server-side
        # no-op) to stay byte-identical with the TypeScript client, while an
        # unset value omits the key rather than emitting a JSON null.
        if effort is not None:
            body["effort"] = effort
        if model is not None:
            body["model"] = model
        if permission_mode is not None:
            body["permission_mode"] = permission_mode
        if containment is not None:
            body["containment"] = containment.to_json()
        resp = self._request("POST", "/v1/conversations", body)
        conv = Conversation(self, resp["id"], resp.get("containment"))
        if containment is not None and not resp.get("containment"):
            with contextlib.suppress(HarnessChatError):
                conv.close()
            raise HarnessChatError(
                0,
                "containment_not_applied",
                "the server opened the conversation without echoing an applied containment policy",
            )
        return conv

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
    def __init__(self, client: Client, id: str, containment: dict[str, Any] | None = None):
        self.client = client
        self.id = id
        # The applied containment policy echoed at open; None when uncontained.
        self.containment = containment
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

    def send(self, text: str, *, containment: Containment | None = None) -> str:
        """Send one message. ``containment`` may restate a contained
        conversation's policy (it must be the same policy); on an uncontained
        conversation the server refuses it -- containment is chosen at open."""
        if self._token is None:
            raise HarnessChatError(409, "no_control", "acquire control before send()")
        body: dict[str, Any] = {"token": self._token, "text": text}
        if containment is not None:
            self.client._require_containment(containment.kind)
            body["containment"] = containment.to_json()
        resp = self.client._request("POST", f"/v1/conversations/{self.id}/messages", body)
        return resp["turn_id"]

    def interrupt(self) -> dict[str, Any]:
        """Interrupt the turn in flight; needs no control token. Returns
        ``{"result": ...}`` -- "stopped", "cancelled", "too_late" or "no_turn" --
        with ``"error"`` when a cancelled turn's prompt would not clear from the
        composer. The interrupted turn arrives on the event stream with state
        "interrupted"."""
        return self.client._request("POST", f"/v1/conversations/{self.id}/interrupt")

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
