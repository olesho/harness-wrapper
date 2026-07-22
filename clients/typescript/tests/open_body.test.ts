import { test } from "node:test";
import assert from "node:assert/strict";
import { Client, type Effort, type OpenOptions, type PermissionMode } from "../src/index.js";
import { startStub } from "./stub.js";

/** The seven keys `open()` has always posted, before the typed knobs existed. */
const BASE_KEYS = ["args", "binary_path", "cols", "env", "harness", "rows", "working_dir"];

async function openBody(opts: OpenOptions): Promise<Record<string, unknown>> {
  const stub = await startStub((_req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ id: "c1" }));
  });
  try {
    await new Client(stub.url).open(opts);
    return JSON.parse(stub.bodies[0]) as Record<string, unknown>;
  } finally {
    await stub.close();
  }
}

test("omitted knobs post exactly the historical seven keys", async () => {
  const body = await openBody({ harness: "codex", binaryPath: "/bin/codex" });
  assert.deepEqual(Object.keys(body).sort(), BASE_KEYS);
});

test("effort/model are posted alongside the seven keys and nothing else", async () => {
  const body = await openBody({
    harness: "codex",
    binaryPath: "/bin/codex",
    effort: "high",
    model: "gpt-5",
  });
  assert.deepEqual(Object.keys(body).sort(), [...BASE_KEYS, "effort", "model"].sort());
  assert.equal(body.effort, "high");
  assert.equal(body.model, "gpt-5");
});

test("an explicit empty effort is sent as \"\" (presence, not truthiness)", async () => {
  // The cast is the point: "" is outside the Effort enum, but the client is a
  // thin transport with no runtime validation, so it forwards it verbatim —
  // matching the Python client, which keys on presence rather than truthiness.
  const body = await openBody({
    harness: "codex",
    binaryPath: "/bin/codex",
    effort: "" as Effort,
  });
  assert.equal(body.effort, "");
});

test("permissionMode is posted as permission_mode alongside the seven keys", async () => {
  const body = await openBody({
    harness: "claude-code",
    binaryPath: "/bin/claude",
    permissionMode: "plan",
  });
  assert.deepEqual(Object.keys(body).sort(), [...BASE_KEYS, "permission_mode"].sort());
  assert.equal(body.permission_mode, "plan");
});

test("an explicit empty permissionMode is sent as \"\" (presence, not truthiness)", async () => {
  // Same contract as effort: the cast forwards a value outside the union
  // verbatim, and "" reaches the server as "" (a no-op there) rather than
  // being dropped by a truthiness check.
  const body = await openBody({
    harness: "codex",
    binaryPath: "/bin/codex",
    permissionMode: "" as PermissionMode,
  });
  assert.equal(body.permission_mode, "");
});
