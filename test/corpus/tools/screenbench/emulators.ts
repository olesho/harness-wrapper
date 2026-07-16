// Adapter registry for candidate vt100 emulator libraries, mirroring the Go
// bake-off's internal/screenbench/emulator package. Each adapter wraps one
// upstream library directly (not any future src/screen port) and exposes only
// what the bench needs: feed bytes, then read a text snapshot of the
// resulting screen state.
//
// Downstream tooling (metrics.ts/scenario.ts/bench.ts, added separately)
// depends on these exact export names: `register`, `names`, and the
// registered factory looked up by key from `registry`. Keep them stable.

import { Terminal } from "@xterm/headless";

/**
 * BenchEmulator is the minimal surface the bake-off cares about, mirroring
 * Go's `Emulator` interface (internal/screenbench/emulator/emulator.go).
 *
 * write feeds raw PTY bytes (ANSI escapes intact) into the emulator. Some
 * upstream libraries parse asynchronously (see the xterm adapter below), so
 * write returns a Promise that resolves once the bytes have been fully
 * parsed and the screen state is safe to read.
 *
 * snapshot returns the current visible-screen contents as a plain-text
 * rendering, with trailing whitespace per line preserved (callers normalize
 * for comparisons). cursor returns the 0-indexed cursor position [col, row].
 * size returns the terminal dimensions [cols, rows].
 */
export interface BenchEmulator {
  write(bytes: Buffer | Uint8Array): void | Promise<void>;
  snapshot(): string | Promise<string>;
  cursor(): [col: number, row: number];
  size(): [cols: number, rows: number];
  name(): string;
}

/** Factory constructs a BenchEmulator at a given terminal size. */
export type Factory = (cols: number, rows: number) => BenchEmulator;

/**
 * Registry of available emulator factories, keyed by a name identifying the
 * underlying library/approach (mirrors Go's package-level `Registry` map).
 */
export const registry = new Map<string, Factory>();

/** Register adds (or replaces) a factory under `name`. */
export function register(name: string, factory: Factory): void {
  registry.set(name, factory);
}

/** names returns the registered emulator names in stable sorted order. */
export function names(): string[] {
  return Array.from(registry.keys()).sort();
}

/**
 * xterm adapter — wraps `@xterm/headless`'s `Terminal` directly.
 *
 * `@xterm/headless` runs in plain Node with no DOM/browser shims. Its write
 * path is asynchronous: `Terminal#write` parses off a callback (queued via
 * the internal write buffer, not synchronously inline), so a snapshot taken
 * immediately after calling `write()` without waiting for that callback can
 * race the parser and observe a stale/partial screen. `write()` below
 * returns a Promise that resolves only once xterm's own write callback
 * fires, so callers that `await emulator.write(...)` are guaranteed the
 * screen reflects those bytes before they call `snapshot()`.
 */
class XtermHeadlessAdapter implements BenchEmulator {
  private readonly term: Terminal;

  constructor(cols: number, rows: number) {
    this.term = new Terminal({ cols, rows, allowProposedApi: true });
  }

  write(bytes: Buffer | Uint8Array): Promise<void> {
    return new Promise((resolve) => {
      this.term.write(bytes, resolve);
    });
  }

  snapshot(): string {
    const buf = this.term.buffer.active;
    const lines: string[] = [];
    for (let i = 0; i < this.term.rows; i++) {
      const line = buf.getLine(i);
      lines.push(line ? line.translateToString(false) : "");
    }
    return lines.join("\n");
  }

  cursor(): [number, number] {
    const buf = this.term.buffer.active;
    return [buf.cursorX, buf.cursorY];
  }

  size(): [number, number] {
    return [this.term.cols, this.term.rows];
  }

  name(): string {
    return "xterm";
  }
}

register("xterm", (cols, rows) => new XtermHeadlessAdapter(cols, rows));
