// Minimal client for harness-chatd (HTTP + SSE). Node 18+ / modern browsers.

export interface Turn {
  id: string;
  session_id: string;
  role: "user" | "assistant" | "system" | (string & {});
  state: "pending" | "streaming" | "complete" | "errored" | "interrupted" | (string & {});
  text?: string;
  reason?: string;
  started_at?: string;
  completed_at?: string;
  /**
   * Populated when the wrapper recognized an upstream API error
   * (e.g. Claude `API Error: 529`, Codex `exceeded retry limit, last
   * status: 503`). Zero / omitted for transport errors (no HTTP code
   * in the harness output) and for non-api turn errors.
   */
  http_code?: number;
  /**
   * Wait duration the harness suggested, as a Go-duration string
   * ("30s", "2m"). Omitted when the message contained no parseable
   * hint. Consumers should treat this as advisory.
   */
  retry_after?: string;
}

/** One selectable option of an `input_request` (trust dialog, menu, …). */
export interface InputRequest {
  id: string;
  kind: string;
  prompt: string;
  header?: string;
  multi_select?: boolean;
  options?: Array<{ id: string; alias?: string; label: string; description?: string }>;
}

/**
 * A frame from the `/events` SSE stream. `type` discriminates the payload:
 * `"turn"` frames carry `turn`, `"input_request"` / `"input_resolved"` frames
 * carry `input` and **no** `turn` at all — so always narrow on `type` (or check
 * `turn` for presence) before dereferencing it.
 *
 * `type` is always present on the wire; `turn` and `input` are omitted per
 * frame kind.
 */
export interface TurnEvent {
  type: "turn" | "input_request" | "input_resolved" | "exited" | (string & {});
  turn?: Turn;
  input?: InputRequest;
  /** How the harness process ended; set on the last frame, `type: "exited"`. */
  exit?: {
    status: string;
    exit_code: number;
    signal?: string;
    reason?: string;
    class?: string;
    ended_at?: string;
  };
  error?: string;
}

/**
 * Reasoning-effort levels the wrapper accepts. Mirrors the server-side enum;
 * this is compile-time typo protection only — the client performs no runtime
 * validation and sends whatever it is given.
 */
export type Effort = "low" | "medium" | "high" | "xhigh" | "max";

/**
 * Launch-time permission rungs the wrapper accepts, least to most permissive,
 * followed by the harness-native spellings it also takes. Mirrors
 * `isSupportedPermissionMode` in `pkg/wrapper/wrapper.go`; like {@link Effort}
 * this is compile-time typo protection only — the client performs no runtime
 * validation and sends whatever it is given.
 *
 * The union is deliberately flat, but the server is not: a native spelling is
 * accepted only by the harness that owns it. `acceptEdits` / `dontAsk` /
 * `bypassPermissions` are claude / claude-code only; `read-only` /
 * `workspace-write` / `danger-full-access` are codex only. Crossing them is a
 * 400 `invalid_config`, not a silent no-op.
 */
export type PermissionMode =
  // Canonical, harness-independent rungs.
  | "plan"
  | "manual"
  | "ask"
  | "auto"
  | "bypass"
  // claude / claude-code native --permission-mode values.
  | "acceptEdits"
  | "dontAsk"
  | "bypassPermissions"
  // codex native -s/--sandbox values.
  | "read-only"
  | "workspace-write"
  | "danger-full-access";

/**
 * An optional Landlock containment request (Linux, ABI 9+, or 6+ with the AppArmor socket layer): an extra,
 * kernel-enforced boundary around the harness, outside whatever its own
 * permission settings enforce. Mirrors `containment.Request` in
 * `pkg/containment`; the server validates it, and a request it cannot honour
 * is a 400 `invalid_config` — never a silent downgrade.
 *
 * Serialization preserves exactly what you set: unset fields are omitted,
 * `restrictTcp: true` with no (or an empty) `connectTcp` means deny all TCP,
 * and `connectTcp` without `restrictTcp` is sent as-is and rejected by the
 * server rather than "fixed" here.
 */
export interface Containment {
  /** The containment kind; only `"landlock"` exists. */
  kind: "landlock" | (string & {});
  /** Additional existing absolute paths the harness may read. */
  readOnly?: string[];
  /** Additional existing absolute paths the harness may read and write. */
  readWrite?: string[];
  /** Turn TCP filtering on: deny TCP bind and every connect except `connectTcp`. */
  restrictTcp?: boolean;
  /** Remote TCP ports the harness may connect to (with `restrictTcp`). */
  connectTcp?: number[];
  /** Lowest Landlock ABI to accept (6+). Unset detects: Landlock alone on ABI 9+, the AppArmor socket layer below it when installed; 9 refuses the layer. */
  minAbi?: number;
  /** Caller-managed persistent state directory instead of private state. */
  stateDir?: string;
  /** Extra environment variable NAMES to pass to the harness. */
  passEnv?: string[];
}

/** The applied containment policy the server echoes (see `containment.Applied`). */
export interface AppliedContainment {
  schema_version: number;
  kind: string;
  abi: number;
  required_abi: number;
  profile: string;
  profile_version: number;
  handled_fs: string[];
  grants: Array<{ path: string; access: string; rights: string[]; source: string; requested?: string }>;
  tcp: { mode: string; connect?: number[]; bind: string };
  /** "denied" (Landlock RESOLVE_UNIX) or "denied_outside_roots" (the AppArmor socket layer). */
  pathname_unix_sockets: string;
  /** The AppArmor socket layer, present only with "denied_outside_roots". */
  apparmor?: { profile: string; roots: string[] };
  scopes: string[];
  state: {
    mode: string;
    id?: string;
    home: string;
    tmp: string;
    harness_state?: string;
    harness_state_env?: string;
    state_dir?: string;
    state_dir_requested?: string;
  };
  supervision: { mode: string; cgroup?: string; reason?: string; cleanup?: string };
  env: string[];
  omitted?: string[];
  fingerprint: string;
}

/** GET /v1/capabilities. */
export interface Capabilities {
  containment: { kinds: string[] };
}

/** Renders a Containment as the wire object, omitting unset fields. */
export function containmentBody(c: Containment): Record<string, unknown> {
  const body: Record<string, unknown> = { kind: c.kind };
  if (c.readOnly !== undefined) body.read_only = c.readOnly;
  if (c.readWrite !== undefined) body.read_write = c.readWrite;
  if (c.restrictTcp !== undefined) body.restrict_tcp = c.restrictTcp;
  if (c.connectTcp !== undefined) body.connect_tcp = c.connectTcp;
  if (c.minAbi !== undefined) body.min_abi = c.minAbi;
  if (c.stateDir !== undefined) body.state_dir = c.stateDir;
  if (c.passEnv !== undefined) body.pass_env = c.passEnv;
  return body;
}

export interface OpenOptions {
  harness: string;
  binaryPath: string;
  args?: string[];
  workingDir?: string;
  env?: string[];
  cols?: number;
  rows?: number;
  /**
   * Reasoning effort for the harness. Unlike `model`, this is **validated and
   * hard-fails**:
   *
   * 1. A value outside the enum, or any effort at all on a harness that does
   *    not support it, is rejected before the harness launches. (`model` is
   *    never validated — see its doc.)
   * 2. On the harness-chatd gateway the effort-capable harness names are
   *    exactly `"codex"` and `"claude-code"`, case-sensitively. Every other
   *    accepted harness name — `"opencode"`, `"pi"`, `"generic"`/`""` —
   *    rejects `effort` outright with a 400 `invalid_options`. That is the
   *    exact opposite of `model`, which is a silent no-op on those same
   *    harnesses: do not assume the two knobs behave symmetrically. Note also
   *    that plain `"claude"` and `"Codex"` are 400 `unknown_harness` (the
   *    gateway matches the raw string) even though the wrapper itself would
   *    accept them.
   * 3. An explicit flag already present in `args` wins over this field: a
   *    `--effort` in `args` (claude / claude-code), or a
   *    `-c model_reasoning_effort=…` (codex), suppresses injection silently.
   * 4. codex remaps `"max"` → `"xhigh"`, so `effort: "max"` reaches the
   *    harness as `model_reasoning_effort="xhigh"`.
   */
  effort?: Effort;
  /**
   * Model override for the harness. Unlike `effort`, this is **not validated
   * at all**:
   *
   * 1. No value is ever rejected; an unsupported harness silently drops it
   *    rather than erroring, so a typo'd model name reaches the harness (or is
   *    dropped) without any client- or gateway-side complaint.
   * 2. Injection happens only for claude / claude-code (`--model <v>`) and
   *    codex (`-c model="<v>"`). On `"opencode"`, `"pi"` and
   *    `"generic"`/`""` it is a SILENT NO-OP — whereas `effort` on those same
   *    harnesses is a 400 `invalid_options`. The two knobs are not symmetric.
   * 3. An explicit `--model` (claude / claude-code) or `-c model=…` (codex)
   *    already in `args` wins over this field, silently.
   * 4. Only `effort` is remapped per harness (codex `"max"` → `"xhigh"`); the
   *    model string is passed through verbatim.
   */
  model?: string;
  /**
   * Launch-time permission posture for the harness. Like `effort` (and unlike
   * `model`) this is **validated and hard-fails** — a mode the caller believes
   * restricts the harness is never dropped on the floor:
   *
   * 1. An unknown mode, a native spelling belonging to the *other* harness, or
   *    any mode at all on a harness with no permission axis (`"opencode"`,
   *    `"pi"`, `"generic"`/`""`) is a 400 `invalid_config` before launch.
   * 2. `"plan"` is rejected on codex: codex has **no launch-time flag** for the
   *    plan rung, and a no-op would launch it unrestricted. Plan mode on codex
   *    is only reachable in-band, by sending `/plan` after the conversation is
   *    open.
   * 3. An explicit permission-axis flag already in `args` wins over this field,
   *    silently — `--permission-mode` / `--dangerously-skip-permissions` on
   *    claude / claude-code, `-s` / `--sandbox` / `-a` / `--ask-for-approval` /
   *    `--dangerously-bypass-approvals-and-sandbox` on codex. The two
   *    `--dangerously-*` arms are reachable only for a bypass-class mode; any
   *    other mode paired with them is rejected (400) rather than suppressed.
   * 4. Restrictive rungs (`"plan"`, `"manual"`, `"ask"`) are fully enforced only
   *    with a human at the TUI; unattended, claude's permission dialogs stall
   *    the turn and codex's approval prompts are auto-approved (only the `-s`
   *    sandbox axis still binds).
   * 5. `"bypass"` over the gateway carries no `IS_SANDBOX=1` (chatd has no
   *    `--sandbox-defaults`), so pass `IS_SANDBOX=1` in `env` or an
   *    `input_policy` with `by_kind: {"trust_prompt": …}` — otherwise
   *    claude-code stops on its acceptance screen as a `trust_prompt` input
   *    request.
   */
  permissionMode?: PermissionMode;
  /**
   * Landlock containment for the conversation, fixed for its life. Sent only
   * after `GET /v1/capabilities` lists the kind: a harness-chatd built before
   * containment would silently drop the field and run the harness
   * uncontained, so against such a server `open()` throws
   * `containment_unsupported` without posting anything. The server's echo of
   * the applied policy is verified too: an open response without it throws.
   */
  containment?: Containment;
}

export class HarnessChatError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(`${status} ${code}: ${message}`);
  }
}

export class Client {
  private caps: Promise<Capabilities | null> | null = null;

  constructor(private readonly baseUrl: string) {
    this.baseUrl = baseUrl.replace(/\/$/, "");
  }

  /**
   * The server's capabilities, or null for a harness-chatd built before the
   * route existed (404). Cached per client.
   */
  async capabilities(): Promise<Capabilities | null> {
    if (!this.caps) {
      this.caps = this.request<Capabilities>("GET", "/v1/capabilities").catch((e) => {
        if (e instanceof HarnessChatError && e.status === 404) return null;
        this.caps = null; // a transient failure: ask again next time
        throw e;
      });
    }
    return this.caps;
  }

  /** @internal Throws unless the server lists `kind` among its containment kinds. */
  async requireContainment(kind: string): Promise<void> {
    const caps = await this.capabilities();
    if (!caps || !(caps.containment?.kinds ?? []).includes(kind)) {
      throw new HarnessChatError(
        0,
        "containment_unsupported",
        caps
          ? `server does not support containment kind "${kind}" (supports: ${(caps.containment?.kinds ?? []).join(", ") || "none"})`
          : "server predates containment (no /v1/capabilities); refusing to send a request it would ignore",
      );
    }
  }

  async open(opts: OpenOptions): Promise<Conversation> {
    if (opts.containment) await this.requireContainment(opts.containment.kind);
    const body = {
      harness: opts.harness,
      binary_path: opts.binaryPath,
      args: opts.args ?? [],
      working_dir: opts.workingDir ?? "",
      env: opts.env ?? [],
      cols: opts.cols ?? 0,
      rows: opts.rows ?? 0,
      // Unset knobs are dropped by JSON.stringify below, so omitting them
      // yields a byte-identical body to the pre-effort/model client. An
      // explicit "" is deliberately sent as "" (presence, not truthiness).
      effort: opts.effort,
      model: opts.model,
      permission_mode: opts.permissionMode,
      containment: opts.containment ? containmentBody(opts.containment) : undefined,
    };
    const res = await this.request<{ id: string; containment?: AppliedContainment }>(
      "POST",
      "/v1/conversations",
      body,
    );
    const conv = new Conversation(this, res.id, res.containment ?? null);
    if (opts.containment && !res.containment) {
      await conv.close().catch(() => {});
      throw new HarnessChatError(
        0,
        "containment_not_applied",
        "the server opened the conversation without echoing an applied containment policy",
      );
    }
    return conv;
  }

  async list(): Promise<
    Array<{ id: string; harness: string; session_id?: string; containment?: AppliedContainment }>
  > {
    return (await this.request("GET", "/v1/conversations")) as any;
  }

  /** @internal */
  async request<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
    const init: RequestInit = { method, headers: { Accept: "application/json" } };
    if (body !== undefined) {
      (init.headers as Record<string, string>)["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    const res = await fetch(this.baseUrl + path, init);
    if (!res.ok) {
      let code = "";
      let msg = res.statusText;
      try {
        const j = (await res.json()) as { error?: string; code?: string };
        code = j.code ?? "";
        msg = j.error ?? msg;
      } catch {
        /* non-JSON error body */
      }
      throw new HarnessChatError(res.status, code, msg);
    }
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }

  /** @internal */
  baseURL(): string {
    return this.baseUrl;
  }
}

export class Conversation {
  private token: string | null = null;

  /**
   * @param containment the applied containment policy the server echoed at
   *   open, or null for an uncontained conversation.
   */
  constructor(
    public client: Client,
    public id: string,
    public containment: AppliedContainment | null = null,
  ) {}

  async acquire(): Promise<string> {
    const res = await this.client.request<{ token: string }>(
      "POST",
      `/v1/conversations/${this.id}/control`,
    );
    this.token = res.token;
    return res.token;
  }

  async release(): Promise<void> {
    if (!this.token) return;
    const tok = this.token;
    this.token = null;
    await this.client.request("DELETE", `/v1/conversations/${this.id}/control/${tok}`);
  }

  async withControl<T>(fn: () => Promise<T>): Promise<T> {
    await this.acquire();
    try {
      return await fn();
    } finally {
      await this.release();
    }
  }

  /**
   * Send one message. `opts.containment` may restate a contained
   * conversation's policy (it must be the same policy); on an uncontained
   * conversation the server refuses it — containment is chosen at open.
   */
  async send(text: string, opts?: { containment?: Containment }): Promise<string> {
    if (!this.token) throw new HarnessChatError(409, "no_control", "acquire control before send()");
    const body: Record<string, unknown> = { token: this.token, text };
    if (opts?.containment) {
      await this.client.requireContainment(opts.containment.kind);
      body.containment = containmentBody(opts.containment);
    }
    const res = await this.client.request<{ turn_id: string }>(
      "POST",
      `/v1/conversations/${this.id}/messages`,
      body,
    );
    return res.turn_id;
  }

  /**
   * Interrupt the turn in flight. Needs no control token. Resolves with what
   * the harness did — "stopped", "cancelled", "too_late" or "no_turn" — and
   * `error` when a cancelled turn's prompt would not clear from the composer.
   * The interrupted turn itself arrives on the event stream with state
   * "interrupted".
   */
  async interrupt(): Promise<{ result: "stopped" | "cancelled" | "too_late" | "no_turn" | (string & {}); error?: string }> {
    return this.client.request("POST", `/v1/conversations/${this.id}/interrupt`);
  }

  async history(): Promise<Turn[]> {
    const res = await this.client.request<{ turns: Turn[] }>(
      "GET",
      `/v1/conversations/${this.id}/history`,
    );
    return res.turns ?? [];
  }

  async close(): Promise<void> {
    try {
      await this.client.request("DELETE", `/v1/conversations/${this.id}`);
    } catch (e) {
      if (e instanceof HarnessChatError && e.status === 404) return;
      throw e;
    }
  }

  async *events(signal?: AbortSignal): AsyncIterable<TurnEvent> {
    const res = await fetch(`${this.client.baseURL()}/v1/conversations/${this.id}/events`, {
      headers: { Accept: "text/event-stream" },
      signal,
    });
    if (!res.ok || !res.body) {
      throw new HarnessChatError(res.status, "stream_failed", res.statusText);
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
      while (true) {
        const { value, done } = await reader.read();
        if (done) return;
        buf += decoder.decode(value, { stream: true });
        buf = yield* drainSSEFrames(buf);
      }
    } finally {
      reader.cancel().catch(() => {});
    }
  }
}

/**
 * Collect the `data:` payload of one SSE block (frame text between blank
 * lines). Comment lines (`:`) are skipped; multiple data lines are joined
 * with newlines. Returns null when the block carries no data lines.
 */
function parseSSEBlock(block: string): string | null {
  const dataLines: string[] = [];
  for (const line of block.split("\n")) {
    if (line.startsWith(":")) continue;
    if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^\s/, ""));
  }
  return dataLines.length === 0 ? null : dataLines.join("\n");
}

/**
 * Yield a TurnEvent for every complete `\n\n`-delimited frame in `buf`,
 * returning the leftover (incomplete) tail for the next read.
 */
function* drainSSEFrames(buf: string): Generator<TurnEvent, string> {
  let idx: number;
  while ((idx = buf.indexOf("\n\n")) >= 0) {
    const block = buf.slice(0, idx);
    buf = buf.slice(idx + 2);
    const data = parseSSEBlock(block);
    if (data !== null) yield JSON.parse(data) as TurnEvent;
  }
  return buf;
}
