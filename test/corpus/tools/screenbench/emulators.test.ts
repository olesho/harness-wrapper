// Crash-survival sanity check for the emulator adapter registry (the TS
// equivalent of the Go bake-off's "does it survive real PTY output"
// discipline; see docs/md/internal/decisions/adr-001-vt100-ts.md). This is
// NOT a fidelity/accuracy test -- it only asserts the registered adapter can
// consume real recorded corpus byte streams without throwing and produces a
// non-empty snapshot afterward.

import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { registry, names } from "./emulators.ts";

const CORPUS_ROOT = path.resolve(import.meta.dirname, "../../../corpus");

interface ScenarioMeta {
  cols: number;
  rows: number;
}

const SCENARIOS = [
  "codex/short-reply",
  "codex/long-markdown",
  "codex/code-block",
  "claude-code/interrupted-mid-reply",
  "claude-code/multi-turn",
  "claude-code/tool-call",
];

describe("emulator registry", () => {
  it("registers the xterm adapter", () => {
    expect(names()).toContain("xterm");
  });

  it("names() returns sorted, stable order", () => {
    const list = names();
    expect(list).toEqual([...list].sort());
  });
});

describe.each(names())("adapter %s survives real corpus recordings", (adapterName) => {
  const factory = registry.get(adapterName)!;

  it.each(SCENARIOS)("does not crash on %s", async (scenario) => {
    const dir = path.join(CORPUS_ROOT, scenario);
    const bytes = fs.readFileSync(path.join(dir, "bytes.raw"));
    const meta: ScenarioMeta = JSON.parse(
      fs.readFileSync(path.join(dir, "meta.json"), "utf8"),
    );

    const emulator = factory(meta.cols, meta.rows);
    expect(emulator.size()).toEqual([meta.cols, meta.rows]);

    // The write must not throw/reject -- this is the crash-survival check.
    await emulator.write(bytes);

    const snapshot = await emulator.snapshot();
    expect(typeof snapshot).toBe("string");
    expect(snapshot.trim().length).toBeGreaterThan(0);

    const [col, row] = emulator.cursor();
    expect(col).toBeGreaterThanOrEqual(0);
    expect(row).toBeGreaterThanOrEqual(0);
    expect(emulator.name()).toBe(adapterName);
  });
});
