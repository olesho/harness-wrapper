import { test } from "node:test";
import assert from "node:assert/strict";
import { Client, Conversation, HarnessChatError } from "../src/index.js";
import { startStub } from "./stub.js";

/** A conversation that already holds `tok`, without a control round trip. */
function controlled(url: string): Conversation {
  const conv = new Conversation(new Client(url), "c1");
  (conv as unknown as { token: string }).token = "tok";
  return conv;
}

test("answer requires control and posts nothing without it", async () => {
  const stub = await startStub((_req, res) => {
    res.writeHead(204);
    res.end();
  });
  try {
    await assert.rejects(new Conversation(new Client(stub.url), "c1").answer("r1", { optionId: "proceed" }), HarnessChatError);
    assert.equal(stub.bodies.length, 0);
  } finally {
    await stub.close();
  }
});

test("answer posts only the fields given to /input", async () => {
  const paths: string[] = [];
  const stub = await startStub((req, res) => {
    paths.push(`${req.method} ${req.url}`);
    res.writeHead(204);
    res.end();
  });
  try {
    const conv = controlled(stub.url);
    await conv.answer("r1", { optionId: "proceed" });
    await conv.answer("r2", { optionIds: ["a", "b"] });
    await conv.answer("", { text: "hello" });
    assert.deepEqual(paths, Array(3).fill("POST /v1/conversations/c1/input"));
    assert.deepEqual(JSON.parse(stub.bodies[0]), { token: "tok", request_id: "r1", option_id: "proceed" });
    assert.deepEqual(JSON.parse(stub.bodies[1]), { token: "tok", request_id: "r2", option_ids: ["a", "b"] });
    assert.deepEqual(JSON.parse(stub.bodies[2]), { token: "tok", text: "hello" });
  } finally {
    await stub.close();
  }
});

test("screen reads GET /screen", async () => {
  const snap = { text: "hi", cols: 80, rows: 24, cursor_col: 1, cursor_row: 2, generation: 7 };
  let seen = "";
  const stub = await startStub((req, res) => {
    seen = `${req.method} ${req.url}`;
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(snap));
  });
  try {
    assert.deepEqual(await new Conversation(new Client(stub.url), "c1").screen(), snap);
    assert.equal(seen, "GET /v1/conversations/c1/screen");
  } finally {
    await stub.close();
  }
});
