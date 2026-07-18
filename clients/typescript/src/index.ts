// Minimal client for harness-chatd (HTTP + SSE). Node 18+ / modern browsers.

export interface Turn {
  id: string;
  session_id: string;
  role: "user" | "assistant" | "system" | string;
  state: "pending" | "streaming" | "complete" | "errored" | string;
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

export interface TurnEvent {
  turn: Turn;
  error?: string;
}

export interface OpenOptions {
  harness: string;
  binaryPath: string;
  args?: string[];
  workingDir?: string;
  env?: string[];
  cols?: number;
  rows?: number;
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
        let idx: number;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const block = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          const dataLines: string[] = [];
          for (const line of block.split("\n")) {
            if (line.startsWith(":")) continue;
            if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^\s/, ""));
          }
          if (dataLines.length === 0) continue;
          yield JSON.parse(dataLines.join("\n")) as TurnEvent;
        }
      }
    } finally {
      reader.cancel().catch(() => {});
    }
  }
}
