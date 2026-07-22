// Minimal in-process HTTP stub used by the client tests: it records every
// request body it sees and lets the test drive the response.

import http from "node:http";
import type { AddressInfo } from "node:net";

export interface Stub {
  /** Base URL to hand to `new Client(...)`. */
  url: string;
  /** Raw request bodies, in arrival order. */
  bodies: string[];
  close(): Promise<void>;
}

export async function startStub(
  handle: (req: http.IncomingMessage, res: http.ServerResponse) => void,
): Promise<Stub> {
  const bodies: string[] = [];
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      bodies.push(Buffer.concat(chunks).toString("utf8"));
      handle(req, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    bodies,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}
