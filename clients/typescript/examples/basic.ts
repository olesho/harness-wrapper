import { Client } from "../src/index.js";

async function main() {
  const [, , binary, harness] = process.argv;
  if (!binary || !harness) {
    console.error("usage: tsx examples/basic.ts <binary_path> <harness>");
    process.exit(2);
  }

  const client = new Client("http://127.0.0.1:8080");
  const conv = await client.open({ harness, binaryPath: binary });
  console.log(`opened conversation ${conv.id}`);

  try {
    await conv.withControl(async () => {
      const turnId = await conv.send("hello");
      console.log(`sent turn ${turnId}`);
      for await (const ev of conv.events()) {
        console.log(`  event: turn=${ev.turn.id} state=${ev.turn.state}`);
        if (ev.turn.id === turnId && (ev.turn.state === "complete" || ev.turn.state === "errored")) {
          break;
        }
      }
    });
    for (const t of await conv.history()) {
      console.log(`  ${t.role}: ${JSON.stringify((t.text ?? "").slice(0, 80))}`);
    }
  } finally {
    await conv.close();
  }
}

try {
  await main();
} catch (e) {
  console.error(e);
  process.exit(1);
}
