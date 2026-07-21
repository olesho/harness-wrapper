// Deterministic parser conformance: the shared, cross-language model-picker
// corpus (test/corpus/models) is the single source of truth. It is vendored
// byte-identically into meta-harness and harness-wrapper (canonical); the two
// repos are in sync iff their test/corpus/models/MANIFEST.sha256 match. For
// every recorded picker this test renders bytes.raw through a Screen and
// asserts (1) the render byte-equals the pinned expected.txt (screen-render
// drift), and (2) parseModelPicker(text, harness) deep-equals the canonical
// meta.json.expected (parser drift). It mirrors Go's models_corpus_test.go so
// the Go and TS parsers must agree on every fixture — no hardcoded ids. It also
// recomputes MANIFEST.sha256 and fails on drift (the symmetric vendored --check
// analogue enforced in CI). No live CLI.

import { createHash } from "node:crypto";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";
import { newScreen } from "../../src/screen/index.ts";
import {
  parseModelPicker,
  isKnownModel,
  knownModels,
  defaultModel,
  type ModelInfo,
} from "../../src/discovery/models.ts";

const thisDir = dirname(fileURLToPath(import.meta.url));

function repoRoot(): string {
  let dir = thisDir;
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "test", "corpus"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("could not locate test/corpus from " + thisDir);
}

// The shared corpus root — one canonical tree, byte-identical across repos.
const corpusRoot = join(repoRoot(), "test", "corpus", "models");

// The canonical meta.json.expected serialization matches Go's []Info exactly:
// capitalized field names {ID, Label, Description, Current, IsDefault}. Map it
// onto the TS ModelInfo shape before comparing with the parser output.
interface CanonicalInfo {
  ID: string;
  Label: string;
  Description: string;
  Current: boolean;
  IsDefault: boolean;
}

interface CorpusMeta {
  harness: string;
  binary_version: string;
  recorded_at: string;
  cols: number;
  rows: number;
  notes: string;
  expected: CanonicalInfo[];
}

interface CorpusCase {
  name: string; // "<harness>/<case>", forward-slashed
  meta: CorpusMeta;
  raw: Uint8Array; // bytes.raw
  expected: string; // expected.txt
}

// Go's screen.Snapshot().Text appends a single terminal newline after the last
// row; the TS Screen.snapshot().text does not. expected.txt is the canonical
// Go-rendered snapshot (byte-identical across repos, pinned by MANIFEST.sha256),
// so it carries that trailing newline. This is a Snapshot serialization
// convention, NOT a render drift — every rendered COLUMN, including per-line
// trailing whitespace, is asserted byte-for-byte; only the file-terminating
// newline is normalized away (mirrors pkg/versions/parity_test.go comparing
// parsed values, not bytes, across the two repos' different formatters).
function normalizeRender(s: string): string {
  return s.replace(/\n+$/, "");
}

function toModelInfo(c: CanonicalInfo): ModelInfo {
  return {
    id: c.ID,
    label: c.Label,
    description: c.Description,
    current: c.Current,
    isDefault: c.IsDefault,
  };
}

/** Walk the corpus tree and load every case (dir containing a meta.json). */
function loadCorpus(): CorpusCase[] {
  const cases: CorpusCase[] = [];
  const walk = (dir: string) => {
    for (const name of readdirSync(dir).sort()) {
      const p = join(dir, name);
      if (statSync(p).isDirectory()) {
        walk(p);
        continue;
      }
      if (name !== "meta.json") continue;
      const meta = JSON.parse(readFileSync(p, "utf8")) as CorpusMeta;
      const raw = new Uint8Array(readFileSync(join(dir, "bytes.raw")));
      const expected = readFileSync(join(dir, "expected.txt"), "utf8");
      const rel = relative(corpusRoot, dir).split(sep).join("/");
      cases.push({ name: rel, meta, raw, expected });
    }
  };
  walk(corpusRoot);
  cases.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return cases;
}

const cases = loadCorpus();

describe("models corpus conformance", () => {
  test("corpus is non-empty", () => {
    expect(cases.length).toBeGreaterThan(0);
  });

  for (const c of cases) {
    describe(c.name, () => {
      test("render(bytes.raw) matches expected.txt (per-column byte-equal)", async () => {
        const scr = newScreen(c.meta.cols, c.meta.rows);
        await scr.write(c.raw);
        expect(normalizeRender(scr.snapshot().text)).toBe(
          normalizeRender(c.expected),
        );
      });

      test("parseModelPicker deep-equals meta.json.expected", () => {
        const got = parseModelPicker(c.expected, c.meta.harness);
        expect(got).toEqual(c.meta.expected.map(toModelInfo));
      });
    });
  }
});

// TestModelsCorpusManifest analogue: recompute MANIFEST.sha256 and assert it is
// current. This is the symmetric vendored guard (mirror of Go's
// TestModelsCorpusManifest and scripts/sync-models-corpus.sh --check): a fixture
// edit that forgets to regenerate the manifest — or a drift from the canonical
// harness-wrapper copy — fails here in meta-harness's own unit gate. The line
// format (lowercase hex, two spaces, forward-slashed path, sorted by path,
// trailing newline) must byte-match the generator, the Go test, and the
// harness-wrapper committed manifest.
describe("models corpus manifest", () => {
  test("MANIFEST.sha256 is current", () => {
    const files: string[] = [];
    const walk = (dir: string) => {
      for (const name of readdirSync(dir).sort()) {
        const p = join(dir, name);
        if (statSync(p).isDirectory()) {
          walk(p);
        } else if (name !== "MANIFEST.sha256") {
          files.push(relative(corpusRoot, p).split(sep).join("/"));
        }
      }
    };
    walk(corpusRoot);
    files.sort();
    const computed = files
      .map((rel) => {
        const hash = createHash("sha256")
          .update(readFileSync(join(corpusRoot, rel)))
          .digest("hex");
        return `${hash}  ${rel}\n`;
      })
      .join("");
    const committed = readFileSync(
      join(corpusRoot, "MANIFEST.sha256"),
      "utf8",
    );
    expect(computed).toBe(committed);
  });
});

describe("parseModelPicker: guards", () => {
  test("non-picker screen yields no models", () => {
    expect(
      parseModelPicker("1. not a picker  just a list", "claude-code"),
    ).toEqual([]);
  });
  test("unsupported harness yields no models", () => {
    const claude = cases.find((c) => c.meta.harness === "claude-code");
    expect(claude).toBeDefined();
    expect(parseModelPicker(claude!.expected, "opencode")).toEqual([]);
  });
});

describe("knownModels / isKnownModel / defaultModel", () => {
  test("claude-code known ids and aliases", () => {
    expect(knownModels("claude-code")).toContain("opus");
    expect(knownModels("claude-code")).toContain("claude-opus-4-8");
    expect(isKnownModel("claude-code", "Opus")).toBe(true); // case-insensitive
    expect(isKnownModel("claude", "sonnet")).toBe(true); // harness normalized
    expect(isKnownModel("claude-code", "gpt-5.5")).toBe(false);
    expect(defaultModel("claude-code")).toBe("opus");
  });
  test("codex known ids", () => {
    expect(isKnownModel("codex", "gpt-5.4-mini")).toBe(true);
    expect(isKnownModel("codex", "o3")).toBe(false);
    expect(defaultModel("codex")).toBe("gpt-5.5");
  });
  test("unknown harness / empty model", () => {
    expect(knownModels("opencode")).toEqual([]);
    expect(isKnownModel("opencode", "anything")).toBe(false);
    expect(isKnownModel("claude-code", "")).toBe(false);
  });
});
