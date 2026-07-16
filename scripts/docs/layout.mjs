// The single HTML shell: <head>, sidebar nav (from the manifest), header with
// theme toggle, content slot, in-page TOC, footer. Relative asset paths so
// every page works over a static server AND file://.
//
// Port of docs/gen/layout.go. Theme persistence key is `mh-theme` (Go's
// `hw-theme`, renamed per the meta-harness `hw-` -> `mh-` convention rather
// than baking harness-wrapper's own storage key into the ported asset).

import { SECTIONS, SITE_TITLE, SITE_TAGLINE } from "./manifest.mjs"
import { escapeHtml } from "./highlight.mjs"

function navSection(section, currentSlug) {
  const links = section.pages
    .map((p) => {
      const active = p.slug === currentSlug ? ` class="active" aria-current="page"` : ""
      return `<li><a href="${p.slug}.html"${active}>${escapeHtml(p.title)}</a></li>`
    })
    .join("\n")
  return `<div class="nav-section">
  <h4>${escapeHtml(section.title)}</h4>
  <ul>
${links}
  </ul>
</div>`
}

// sidebarNav groups sections by their top-level block, preserving
// first-seen block order. `extraLinks` (e.g. a copied-in visual.html) render
// right after the "Overview" home link.
function sidebarNav(currentSlug, extraLinks = []) {
  const blocks = []
  const findBlock = (name) => {
    let b = blocks.find((x) => x.block === name)
    if (!b) {
      b = { block: name, sections: [] }
      blocks.push(b)
    }
    return b
  }
  for (const section of SECTIONS) {
    findBlock(section.block).sections.push(section)
  }

  const rendered = blocks
    .map(
      (b) => `<div class="nav-block">
  <p class="nav-block-title">${escapeHtml(b.block)}</p>
${b.sections.map((s) => navSection(s, currentSlug)).join("\n")}
</div>`
    )
    .join("\n")

  const homeActive = currentSlug === "index" ? " active" : ""
  const extra = extraLinks
    .map((l) => {
      const active = l.slug === currentSlug ? ` class="nav-home active" aria-current="page"` : ` class="nav-home"`
      return `<a${active} href="${l.href}">${escapeHtml(l.title)}</a>`
    })
    .join("\n")

  return `<nav class="sidebar-nav" aria-label="Documentation">
  <a class="nav-home${homeActive}" href="index.html">Overview</a>
${extra}
${rendered}
</nav>`
}

function tocList(headings) {
  if (headings.length < 2) return ""
  const items = headings
    .map((h) => `<li class="toc-h${h.level}"><a href="#${h.id}">${escapeHtml(h.text)}</a></li>`)
    .join("\n")
  return `<aside class="toc" aria-label="On this page">
  <h4>On this page</h4>
  <ul>
${items}
  </ul>
</aside>`
}

// renderPage wraps a rendered page body in the shared HTML shell.
// opts: { slug, title, body, headings, isLanding, extraNavLinks }
export function renderPage(opts) {
  const { slug, title, body, headings = [], isLanding = false, extraNavLinks = [] } = opts

  const toc = isLanding ? "" : tocList(headings)
  const contentClass = isLanding ? "content landing" : "content"
  const mainClass = toc ? "with-toc" : ""

  return `<!doctype html>
<html lang="en" data-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${escapeHtml(title)} · ${escapeHtml(SITE_TITLE)}</title>
<meta name="description" content="${escapeHtml(SITE_TAGLINE)}">
<link rel="stylesheet" href="assets/styles.css">
<link rel="stylesheet" href="assets/highlight.css">
<script>
  // Apply the stored theme before paint to avoid a flash.
  try {
    var t = localStorage.getItem('mh-theme');
    if (t) document.documentElement.setAttribute('data-theme', t);
    else if (matchMedia('(prefers-color-scheme: dark)').matches)
      document.documentElement.setAttribute('data-theme', 'dark');
  } catch (e) {}
</script>
</head>
<body>
<a class="skip-link" href="#main">Skip to content</a>
<header class="site-header">
  <button class="nav-toggle" aria-label="Toggle navigation" aria-expanded="false">☰</button>
  <a class="brand" href="index.html">
    <span class="brand-mark">❯</span>
    <span class="brand-name">${escapeHtml(SITE_TITLE)}</span>
  </a>
  <span class="brand-tagline">${escapeHtml(SITE_TAGLINE)}</span>
  <button class="theme-toggle" aria-label="Toggle light/dark theme" title="Toggle theme">
    <span class="theme-icon-light">☀</span><span class="theme-icon-dark">☾</span>
  </button>
</header>
<div class="layout">
  <aside class="sidebar" id="sidebar">
${sidebarNav(slug, extraNavLinks)}
  </aside>
  <main id="main" class="${mainClass}">
    <article class="${contentClass}">
${body}
    </article>
${toc}
  </main>
</div>
<footer class="site-footer">
  <p>Generated from the canonical <code>docs/md/*.md</code> with <code>npm run docs</code>.
  The markdown is the source of truth; this site is a build artifact.</p>
</footer>
<script src="assets/app.js" defer></script>
</body>
</html>`
}
