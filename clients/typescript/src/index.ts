// Minimal client for harness-chatd (HTTP + SSE). Node 18+ / modern browsers.

export interface Turn {
  id: string;
  session_id: string;
  role: "user" | "assistant" | "system" | (string & {});
  state: "pending" | "streaming" | "complete" | "errored" | (string & {});
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
  type: "turn" | "input_request" | "input_resolved" | (string & {});
  turn?: Turn;
  input?: InputRequest;
  error?: string;
}

/**
 * Reasoning-effort levels the wrapper accepts. Mirrors the server-side enum;
 * this is compile-time typo protection only — the client performs no runtime
 * validation and sends whatever it is given.
 */
export type Effort = "low" | "medium" | "high" | "xhigh" | "max";

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
}

export class HarnessChatError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(`${status} ${code}: ${message}`);
  }
}

export class Client {
  constructor(private readonly baseUrl: string) {
    this.baseUrl = baseUrl.replace(/\/$/, "");
  }

  async open(opts: OpenOptions): Promise<Conversation> {
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
    };
    const res = await this.request<{ id: string }>("POST", "/v1/conversations", body);
    return new Conversation(this, res.id);
  }

  async list(): Promise<Array<{ id: string; harness: string; session_id?: string }>> {
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

  constructor(public client: Client, public id: string) {}

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

  async send(text: string): Promise<string> {
    if (!this.token) throw new HarnessChatError(409, "no_control", "acquire control before send()");
    const res = await this.client.request<{ turn_id: string }>(
      "POST",
      `/v1/conversations/${this.id}/messages`,
      { token: this.token, text },
    );
    return res.turn_id;
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
