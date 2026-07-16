#!/usr/bin/env node
// Static site generator for the docs.
//
// `node scripts/docs/build.mjs` reads the canonical markdown under docs/md/,
// renders it with highlight.js-highlighted code + theme-aware inlined SVG
// diagrams, wraps each page in the shared layout, and writes
// docs/site/<slug>.html. The landing page is built from docs/md/README.md
// with an enriched hero and the architecture chart. The theme assets
// (scripts/docs/assets/) are copied to docs/site/assets/.
//
// docs/site/ is a gitignored build artifact; the .md files remain canonical.
// The committed, hand-authored docs/html/index.html is never overwritten —
// if present, it is copied in as docs/site/visual.html with its own nav
// entry, and README.md's own link to it is rewritten to match (see
// buildLanding below).
//
// Port of docs/gen/main.go.

import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

import { ALL_PAGES, SITE_TITLE, SITE_TAGLINE } from "./manifest.mjs"
import { render } from "./markdown.mjs"
import { renderPage } from "./layout.mjs"
import { inlineDiagrams } from "./svg.mjs"
import { buildHighlightCSS, escapeHtml } from "./highlight.mjs"

const here = dirname(fileURLToPath(import.meta.url))
const repoRootDefault = join(here, "..", "..")

// buildSite renders every manifest page + the landing page into `siteDir`.
// Returns { count, siteDir }. Overridable for tests (tmpdir smoke builds).
export function buildSite(opts = {}) {
  const repoRoot = opts.repoRoot || repoRootDefault
  const docsDir = opts.docsDir || join(repoRoot, "docs")
  const mdDir = opts.mdDir || join(docsDir, "md")
  const siteDir = opts.siteDir || join(docsDir, "site")
  const assetsSrcDir = opts.assetsSrcDir || join(here, "assets")
  const warn = opts.warn || ((msg) => console.warn(msg))
  const diagramsDir = join(mdDir, "diagrams")

  rmSync(siteDir, { recursive: true, force: true })
  const assetsOut = join(siteDir, "assets")
  mkdirSync(assetsOut, { recursive: true })

  for (const name of ["styles.css", "app.js"]) {
    copyFileSync(join(assetsSrcDir, name), join(assetsOut, name))
  }
  writeFileSync(join(assetsOut, "highlight.css"), buildHighlightCSS())

  // Never overwrite the committed, hand-authored docs/html/index.html —
  // copy it into the site as visual.html with a nav entry instead. A no-op
  // when no such file exists (this repo doesn't commit one today; the guard
  // mirrors the isFile() pattern main.go already uses for the diagram).
  let extraNavLinks = []
  const visualSrc = join(docsDir, "html", "index.html")
  if (existsSync(visualSrc)) {
    copyFileSync(visualSrc, join(siteDir, "visual.html"))
    extraNavLinks = [{ slug: "visual", title: "HTML overview", href: "visual.html" }]
  }

  const linkOpts = { repoRoot, warn }
  let count = 0

  for (const page of ALL_PAGES) {
    const srcPath = join(docsDir, page.source)
    if (!existsSync(srcPath)) {
      warn(`! skipping ${page.slug}: missing source docs/${page.source}`)
      continue
    }
    const text = readFileSync(srcPath, "utf8")
    const { html, headings } = render(page.source, text, linkOpts)
    const withDiagrams = inlineDiagrams(html, diagramsDir)
    const full = renderPage({
      slug: page.slug,
      title: page.title,
      body: withDiagrams,
      headings,
      extraNavLinks,
    })
    writeFileSync(join(siteDir, `${page.slug}.html`), full)
    count++
  }

  buildLanding({ docsDir, mdDir, siteDir, diagramsDir, linkOpts, extraNavLinks })
  count++

  return { count, siteDir }
}

// buildLanding renders docs/md/README.md (+ an enriched hero and the
// architecture chart) as docs/site/index.html. Port of main.go's
// buildLanding.
function buildLanding({ mdDir, siteDir, diagramsDir, linkOpts, extraNavLinks }) {
  const source = "md/README.md"
  const text = readFileSync(join(mdDir, "README.md"), "utf8")
  let { html: body, headings } = render(source, text, linkOpts)
  body = inlineDiagrams(body, diagramsDir)

  // Landing-page special case: docs/md/README.md's own "HTML overview" link
  // (if present) points at the pre-copy docs/html/index.html location — the
  // one place in the corpus normalizeLink can't rewrite on its own, since
  // ../html/index.html doesn't end in .md. Once flattened into docs/site/,
  // that unrewritten path would resolve to the original file instead of the
  // visual.html copy this same build made above. Scoped to this one known
  // string, in the landing-page path only.
  body = body.replaceAll('href="../html/index.html"', 'href="visual.html"')

  const hero = `<div class="hero">
  <h1 class="hero-title">${escapeHtml(SITE_TITLE)}</h1>
  <p class="hero-tagline">${escapeHtml(SITE_TAGLINE)}</p>
  <p class="hero-sub">Run Claude Code, Codex, Gemini and friends under a PTY, classify their
  state into a small normalized vocabulary, and drive them as multi-turn chat sessions — from
  Go, the CLI, or any language over HTTP.</p>
  <div class="hero-actions">
    <a class="btn primary" href="getting-started.html">Get started →</a>
    <a class="btn" href="architecture.html">Architecture</a>
    <a class="btn" href="adapters.html">Supported harnesses</a>
  </div>
</div>`

  let arch = ""
  if (existsSync(join(diagramsDir, "architecture.svg"))) {
    arch = inlineDiagrams(`<img src="diagrams/architecture.svg" alt="System architecture">`, diagramsDir)
  }

  const full = renderPage({
    slug: "index",
    title: "Overview",
    body: `${hero}\n${arch}\n${body}`,
    headings,
    isLanding: true,
    extraNavLinks,
  })
  writeFileSync(join(siteDir, "index.html"), full)
}

function isMain() {
  return process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href
}

if (isMain()) {
  const { count, siteDir } = buildSite()
  console.log(`✓ docs:build → ${count} pages in ${siteDir}`)
}
