import { test } from "node:test";
import assert from "node:assert/strict";
import type http from "node:http";
import { Client, HarnessChatError, type Containment } from "../src/index.js";
import { startStub } from "./stub.js";

const APPLIED = {
  schema_version: 1,
  kind: "landlock",
  abi: 9,
  required_abi: 9,
  profile: "claude-code@2.1.270",
  profile_version: 1,
  handled_fs: ["execute"],
  grants: [],
  tcp: { mode: "unrestricted", bind: "unrestricted" },
  pathname_unix_sockets: "denied",
  scopes: ["abstract_unix_socket", "signal"],
  state: { mode: "private", home: "/s/home", tmp: "/s/tmp" },
  supervision: { mode: "cgroup", cgroup: "/sys/fs/cgroup/x" },
  env: ["HOME"],
  fingerprint: "sha256:abc",
};

type Route = (req: http.IncomingMessage, res: http.ServerResponse) => void;

function json(res: http.ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}

/** A stub server that answers /v1/capabilities with caps (null: 404). */
async function server(caps: unknown | null, open: Route) {
  const seen: string[] = [];
  const stub = await startStub((req, res) => {
    seen.push(`${req.method} ${req.url}`);
    if (req.url === "/v1/capabilities") {
      if (caps === null) return json(res, 404, { error: "not found" });
      return json(res, 200, caps);
    }
    open(req, res);
  });
  return { stub, seen };
}

const LANDLOCK = { containment: { kinds: ["landlock"] } };

test("containment is posted as the complete object, unset fields omitted", async () => {
  const { stub, seen } = await server(LANDLOCK, (_req, res) => json(res, 201, { id: "c1", containment: APPLIED }));
  try {
    const c: Containment = { kind: "landlock", readWrite: ["/w"], restrictTcp: true, connectTcp: [443] };
    const conv = await new Client(stub.url).open({ harness: "claude-code", binaryPath: "/bin/claude", containment: c });
    assert.deepEqual(seen, ["GET /v1/capabilities", "POST /v1/conversations"]);
    const body = JSON.parse(stub.bodies[1]) as Record<string, unknown>;
    assert.deepEqual(body.containment, { kind: "landlock", read_write: ["/w"], restrict_tcp: true, connect_tcp: [443] });
    assert.equal(conv.containment?.fingerprint, "sha256:abc");
  } finally {
    await stub.close();
  }
});

test("deny-all TCP keeps restrict_tcp with an empty port list", async () => {
  const { stub } = await server(LANDLOCK, (_req, res) => json(res, 201, { id: "c1", containment: APPLIED }));
  try {
    await new Client(stub.url).open({
      harness: "codex",
      binaryPath: "/bin/codex",
      containment: { kind: "landlock", restrictTcp: true, connectTcp: [] },
    });
    const body = JSON.parse(stub.bodies[1]) as { containment: Record<string, unknown> };
    assert.deepEqual(body.containment, { kind: "landlock", restrict_tcp: true, connect_tcp: [] });
  } finally {
    await stub.close();
  }
});

test("ports without restrictTcp are sent as written, for the server to reject", async () => {
  const { stub } = await server(LANDLOCK, (_req, res) =>
    json(res, 400, { error: "connect ports require RestrictTCP", code: "invalid_config" }),
  );
  try {
    await assert.rejects(
      new Client(stub.url).open({
        harness: "codex",
        binaryPath: "/bin/codex",
        containment: { kind: "landlock", connectTcp: [443] },
      }),
      (e: unknown) => e instanceof HarnessChatError && e.code === "invalid_config",
    );
    const body = JSON.parse(stub.bodies[1]) as { containment: Record<string, unknown> };
    assert.deepEqual(body.containment, { kind: "landlock", connect_tcp: [443] });
  } finally {
    await stub.close();
  }
});

test("a server without /v1/capabilities never receives containment", async () => {
  const { stub, seen } = await server(null, (_req, res) => json(res, 201, { id: "c1" }));
  try {
    await assert.rejects(
      new Client(stub.url).open({ harness: "codex", binaryPath: "/bin/codex", containment: { kind: "landlock" } }),
      (e: unknown) => e instanceof HarnessChatError && e.code === "containment_unsupported",
    );
    assert.deepEqual(seen, ["GET /v1/capabilities"]);
  } finally {
    await stub.close();
  }
});

test("a server that lists no kind never receives containment", async () => {
  const { stub, seen } = await server({ containment: { kinds: [] } }, (_req, res) => json(res, 201, { id: "c1" }));
  try {
    await assert.rejects(
      new Client(stub.url).open({ harness: "codex", binaryPath: "/bin/codex", containment: { kind: "landlock" } }),
      (e: unknown) => e instanceof HarnessChatError && e.code === "containment_unsupported",
    );
    assert.deepEqual(seen, ["GET /v1/capabilities"]);
  } finally {
    await stub.close();
  }
});

test("an open response without the applied policy is refused", async () => {
  const { stub, seen } = await server(LANDLOCK, (req, res) => {
    if (req.method === "DELETE") {
      res.writeHead(204);
      return res.end();
    }
    json(res, 201, { id: "c1" });
  });
  try {
    await assert.rejects(
      new Client(stub.url).open({ harness: "codex", binaryPath: "/bin/codex", containment: { kind: "landlock" } }),
      (e: unknown) => e instanceof HarnessChatError && e.code === "containment_not_applied",
    );
    assert.deepEqual(seen, ["GET /v1/capabilities", "POST /v1/conversations", "DELETE /v1/conversations/c1"]);
  } finally {
    await stub.close();
  }
});

test("an uncontained open never asks for capabilities and posts no containment key", async () => {
  const { stub, seen } = await server(LANDLOCK, (_req, res) => json(res, 201, { id: "c1" }));
  try {
    const conv = await new Client(stub.url).open({ harness: "codex", binaryPath: "/bin/codex" });
    assert.deepEqual(seen, ["POST /v1/conversations"]);
    assert.equal("containment" in JSON.parse(stub.bodies[0]), false);
    assert.equal(conv.containment, null);
  } finally {
    await stub.close();
  }
});

test("send can restate containment; a plain send carries none", async () => {
  const { stub } = await server(LANDLOCK, (req, res) => {
    if (req.url?.endsWith("/control")) return json(res, 200, { token: "t1" });
    json(res, 202, { turn_id: "turn1" });
  });
  try {
    const client = new Client(stub.url);
    const conv = new (await import("../src/index.js")).Conversation(client, "c1");
    await conv.acquire();
    await conv.send("hi");
    await conv.send("again", { containment: { kind: "landlock", readOnly: ["/r"] } });
    const plain = JSON.parse(stub.bodies[1]) as Record<string, unknown>;
    assert.equal("containment" in plain, false);
    const restated = JSON.parse(stub.bodies[stub.bodies.length - 1]) as Record<string, unknown>;
    assert.deepEqual(restated.containment, { kind: "landlock", read_only: ["/r"] });
  } finally {
    await stub.close();
  }
});
