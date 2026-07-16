// Tests for the docs site generator (scripts/docs/*.mjs), the Node port of
// docs/gen/*.go (see scripts/docs/manifest.mjs for the port's rationale).

import { describe, expect, test, beforeAll, afterAll } from "vitest"
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

import { normalizeLink } from "../../scripts/docs/markdown.mjs"
import { scopeCSS } from "../../scripts/docs/highlight.mjs"
import { ALL_PAGES } from "../../scripts/docs/manifest.mjs"
import { buildSite } from "../../scripts/docs/build.mjs"

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..")

function tmpDir(prefix: string): string {
  return mkdtempSync(join(tmpdir(), prefix))
}

// ---------------------------------------------------------------------------
// normalizeLink — synthetic fixture reproducing the exact cross-root /
// non-manifest / bare-directory scenarios the manifest generalization exists
// to handle, independent of whatever this repo's docs/md/ corpus happens to
// contain today.
// ---------------------------------------------------------------------------

describe("normalizeLink", () => {
  let fixtureRoot: string
  const sourceToSlug = new Map([
    ["md/modules/README.md", "modules"],
    ["md/modules/cli.md", "cli"],
    ["md/guides/README.md", "guides"],
    ["env/README.md", "env"],
  ])

  beforeAll(() => {
    fixtureRoot = tmpDir("docs-normalize-link-")
    mkdirSync(join(fixtureRoot, "docs", "md", "modules"), { recursive: true })
    mkdirSync(join(fixtureRoot, "docs", "md", "guides"), { recursive: true })
    mkdirSync(join(fixtureRoot, "docs", "env"), { recursive: true })
    mkdirSync(join(fixtureRoot, "src", "cli"), { recursive: true })
    mkdirSync(join(fixtureRoot, "test", "corpus"), { recursive: true })
    for (const p of [
      "docs/md/modules/README.md",
      "docs/md/modules/cli.md",
      "docs/md/guides/README.md",
      "docs/env/README.md",
      "src/cli/PACKAGING.md",
      "test/corpus/README.md",
    ]) {
      writeFileSync(join(fixtureRoot, p), "")
    }
  })

  afterAll(() => {
    rmSync(fixtureRoot, { recursive: true, force: true })
  })

  test("passes through http(s), hash-only, and mailto links unchanged", () => {
    for (const href of ["https://example.com/x.md", "#section", "mailto:a@example.com"]) {
      expect(normalizeLink(href, "md/modules/README.md", { sourceToSlug, repoRoot: fixtureRoot })).toBe(href)
    }
  })

  test("a same-root docs/md/ -> docs/md/ link resolves to its manifest slug", () => {
    expect(normalizeLink("cli.md", "md/modules/README.md", { sourceToSlug, repoRoot: fixtureRoot })).toBe("cli.html")
  })

  test("a cross-root link (docs/md/ -> docs/env/) resolves to its manifest slug", () => {
    // Mirrors docs/md/modules/README.md's real ../../env/README.md -> env.html.
    expect(normalizeLink("../../env/README.md", "md/modules/README.md", { sourceToSlug, repoRoot: fixtureRoot })).toBe(
      "env.html"
    )
  })

  test("a real non-manifest external link is re-based relative to docs/site/, not left unchanged", () => {
    // Mirrors docs/md/modules/cli.md's real ../../../src/cli/PACKAGING.md.
    expect(
      normalizeLink("../../../src/cli/PACKAGING.md", "md/modules/cli.md", { sourceToSlug, repoRoot: fixtureRoot })
    ).toBe("../../src/cli/PACKAGING.md")
  })

  test("another real non-manifest external link, from a different depth", () => {
    expect(
      normalizeLink("../../../test/corpus/README.md", "md/guides/adding-a-harness.md", {
        sourceToSlug,
        repoRoot: fixtureRoot,
      })
    ).toBe("../../test/corpus/README.md")
  })

  test("a bare directory link is treated as an implicit README.md", () => {
    // Mirrors docs/md/README.md's real modules/ -> modules.html.
    expect(normalizeLink("modules/", "md/README.md", { sourceToSlug, repoRoot: fixtureRoot })).toBe("modules.html")
  })

  test("a bare directory link composes with depth-rebasing (not a special case of its own)", () => {
    // Mirrors docs/md/guides/README.md's real ../modules/ -> modules.html.
    expect(normalizeLink("../modules/", "md/guides/README.md", { sourceToSlug, repoRoot: fixtureRoot })).toBe(
      "modules.html"
    )
  })

  test("preserves a #hash fragment through slug rewriting", () => {
    expect(normalizeLink("cli.md#one-shot", "md/modules/README.md", { sourceToSlug, repoRoot: fixtureRoot })).toBe(
      "cli.html#one-shot"
    )
  })

  test("a genuinely dangling .md ref warns and is left unchanged", () => {
    const warnings: string[] = []
    const href = "../../../nonexistent/ghost.md"
    const result = normalizeLink(href, "md/modules/cli.md", {
      sourceToSlug,
      repoRoot: fixtureRoot,
      warn: (msg: string) => warnings.push(msg),
    })
    expect(result).toBe(href)
    expect(warnings).toHaveLength(1)
    expect(warnings[0]).toContain("ghost.md")
  })
})

// ---------------------------------------------------------------------------
// scopeCSS — the chroma-style dual-theme scoping port.
// ---------------------------------------------------------------------------

describe("scopeCSS", () => {
  test("prefixes every selector in a comma-separated list, preserving leading comments", () => {
    const css = `/* a comment */
.foo, .bar {
  color: red;
}
.baz {
  color: blue;
}
`
    const scoped = scopeCSS(css, '[data-theme="dark"]')
    expect(scoped).toContain('[data-theme="dark"] .foo, [data-theme="dark"] .bar {')
    expect(scoped).toContain('[data-theme="dark"] .baz {')
    expect(scoped).toContain("/* a comment */")
    expect(scoped).not.toMatch(/^\.foo/m)
  })
})

// ---------------------------------------------------------------------------
// Build integration — against this repo's real docs/md/ corpus, into a
// tmpdir output so the committed docs/site/ (gitignored) is never touched by
// the test run.
// ---------------------------------------------------------------------------

describe("manifest sources exist on disk", () => {
  // Turns a future dangling manifest ref into a test failure automatically.
  test.each(ALL_PAGES)("docs/$source exists", (page) => {
    expect(existsSync(join(repoRoot, "docs", page.source))).toBe(true)
  })
})

describe("docs build (tmpdir output, real corpus)", () => {
  let outDir: string
  let result: { count: number; siteDir: string }

  beforeAll(() => {
    outDir = tmpDir("docs-site-build-")
    result = buildSite({ siteDir: outDir, warn: () => {} })
  })

  afterAll(() => {
    rmSync(outDir, { recursive: true, force: true })
  })

  test("emits exactly one page per manifest entry, plus the landing page", () => {
    expect(result.count).toBe(ALL_PAGES.length + 1)
    for (const page of ALL_PAGES) {
      expect(existsSync(join(outDir, `${page.slug}.html`))).toBe(true)
    }
    expect(existsSync(join(outDir, "index.html"))).toBe(true)
  })

  test("dual-theme highlight CSS is present (unscoped light + [data-theme=dark]-scoped dark)", () => {
    const css = readFileSync(join(outDir, "assets", "highlight.css"), "utf8")
    expect(css).toMatch(/(^|\n)\.hljs\s*\{/)
    expect(css).toMatch(/\[data-theme="dark"\] \.hljs\s*\{/)
  })

  test("theme assets are copied and use the mh-theme storage key", () => {
    expect(existsSync(join(outDir, "assets", "styles.css"))).toBe(true)
    const appJs = readFileSync(join(outDir, "assets", "app.js"), "utf8")
    expect(appJs).toContain("mh-theme")
    expect(appJs).not.toContain("hw-theme")
  })

  // Closes the coverage gap a single "a rewritten in-doc link" assertion
  // would leave: walks every generated page's anchor hrefs and checks each
  // one actually resolves, rather than trusting normalizeLink's unit tests
  // to be representative of every real link in the corpus.
  test("every generated page's anchor hrefs resolve to a real slug or a real file", () => {
    const validSlugs = new Set([...ALL_PAGES.map((p) => p.slug), "index"])
    const files = [...ALL_PAGES.map((p) => `${p.slug}.html`), "index.html"]
    const anchorHrefRe = /<a\s[^>]*href="([^"]*)"/g

    for (const file of files) {
      const html = readFileSync(join(outDir, file), "utf8")
      for (const match of html.matchAll(anchorHrefRe)) {
        const href = match[1]
        if (!href || href.startsWith("http://") || href.startsWith("https://") || href.startsWith("#") || href.startsWith("mailto:")) {
          continue
        }
        const [pathPart, hash] = href.split("#")
        const slugMatch = pathPart.match(/^([\w-]+)\.html$/)
        if (slugMatch) {
          expect(validSlugs.has(slugMatch[1]), `${file}: href "${href}" -> unknown slug`).toBe(true)
        } else {
          expect(existsSync(join(outDir, pathPart)), `${file}: href "${href}" -> missing file`).toBe(true)
        }
      }
    }
  })
})

// ---------------------------------------------------------------------------
// visual.html copy + landing-page link special case — synthetic fixture,
// since this repo doesn't commit a docs/html/index.html today (docs/html/ is
// itself a gitignored Go build artifact here, not a hand-authored page). The
// mechanism is still real, ported code; this exercises it directly rather
// than asserting something untrue about this repo's current corpus.
// ---------------------------------------------------------------------------

describe("visual.html copy + landing-page link rewrite", () => {
  test("copies a hand-authored docs/html/index.html as visual.html and rewrites README's link to it", () => {
    const tmpRepo = tmpDir("docs-visual-repo-")
    const docsDir = join(tmpRepo, "docs")
    const mdDir = join(docsDir, "md")
    mkdirSync(mdDir, { recursive: true })
    mkdirSync(join(docsDir, "html"), { recursive: true })
    writeFileSync(join(docsDir, "html", "index.html"), "<html><body>hand-authored overview</body></html>")
    writeFileSync(
      join(mdDir, "README.md"),
      "# Title\n\n| See it visually (SVG diagrams) | [HTML overview](../html/index.html) |\n"
    )

    const outDir = tmpDir("docs-visual-out-")
    try {
      const { siteDir } = buildSite({ repoRoot: tmpRepo, docsDir, mdDir, siteDir: outDir, warn: () => {} })

      expect(existsSync(join(siteDir, "visual.html"))).toBe(true)
      expect(readFileSync(join(siteDir, "visual.html"), "utf8")).toContain("hand-authored overview")

      const indexHtml = readFileSync(join(siteDir, "index.html"), "utf8")
      const match = indexHtml.match(/<a\s[^>]*href="([^"]*)"[^>]*>HTML overview<\/a>/)
      expect(match).not.toBeNull()
      expect(match?.[1]).toBe("visual.html")
      expect(indexHtml).not.toContain("../html/index.html")

      // The nav picks up an extra entry for the copied-in page.
      expect(indexHtml).toContain('href="visual.html"')
    } finally {
      rmSync(tmpRepo, { recursive: true, force: true })
      rmSync(outDir, { recursive: true, force: true })
    }
  })
})
