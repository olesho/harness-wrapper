import { test } from "node:test";
import assert from "node:assert/strict";
import { Client, Conversation, type TurnEvent } from "../src/index.js";
import { startStub } from "./stub.js";

test("input_request frames carry no turn; turn frames do", async () => {
  const stub = await startStub((_req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    res.end(
      'data: {"type":"input_request","input":{"id":"i1","kind":"trust","prompt":"?"}}\n\n' +
        'data: {"type":"turn","turn":{"id":"t1","state":"running"}}\n\n',
    );
  });
  try {
    const evs: TurnEvent[] = [];
    for await (const ev of new Conversation(new Client(stub.url), "c1").events()) evs.push(ev);
    // Guard rail: must stay above the assertions below, whose assertion
    // signatures narrow `turn` and would make the directive itself unused.
    // @ts-expect-error `turn` is optional on the envelope — this must never compile.
    void evs[1].turn.id;
    assert.equal(evs[0].type, "input_request");
    assert.equal(evs[0].turn, undefined);
    assert.equal(evs[1].turn?.id, "t1");
  } finally {
    await stub.close();
  }
});
