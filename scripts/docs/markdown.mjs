// Markdown -> HTML rendering for the docs site.
//
// markdown-it (GFM tables via a small subset + html:true) + markdown-it-anchor
// (heading ids for the TOC + hover "#" permalinks) + highlight.js for
// class-based, dual-theme syntax highlighting (see highlight.mjs). In-doc
// links are rewritten to the flat generated <slug>.html. Mirrors orche's
// markdown-it + markdown-it-anchor + Shiki pipeline (and this repo's own Go
// generator, docs/gen/markdown.go, which mirrors the same pipeline with
// goldmark + chroma).
//
// Port note (generalized vs. docs/gen/markdown.go): `normalizeLink` there
// resolves everything relative to a single docs/md/ root and leaves unknown
// .md targets unchanged. Here `sourcePath` — and every manifest `source` — is
// relative to docs/ instead, since the docs tree has more than one root in
// practice (docs/md/ today; docs/env/, docs/design/ are legitimate future
// siblings and in-doc links already cross out of docs/md/). normalizeLink
// also: treats a bare `<dir>/` href as an implicit `<dir>/README.md` *before*
// the .md-suffix check (so `modules/`/`../modules/` resolve exactly like a
// spelled-out link); and, for a resolved .md target with no manifest entry,
// checks whether it's a real on-disk file and — if so — re-expresses it
// relative to the flat docs/site/ output root instead of leaving the
// original (now-wrong-depth) relative path in place. A genuinely missing
// target is a build-time warning, not a silent broken link.

import MarkdownIt from "markdown-it"
import markdownItAnchor from "markdown-it-anchor"
import { existsSync } from "node:fs"
import { posix as pathPosix } from "node:path"

import { SOURCE_TO_SLUG } from "./manifest.mjs"
import { escapeHtml, highlightCode } from "./highlight.mjs"

const slugify = (s) =>
  encodeURIComponent(String(s).trim().toLowerCase().replace(/\s+/g, "-"))

// normalizeLink resolves an in-doc link so it works on the flat generated
// site. Doc .md sources (and bare `<dir>/` links, treated as `<dir>/README.md`)
// map to their output slug <slug>.html (preserving any #anchor). A resolved
// target that isn't a manifest source but is a real file gets re-based
// relative to docs/site/; a target that resolves nowhere is left unchanged
// and reported via `warn`. Non-doc links (http(s), anchors, mailto) pass
// through unchanged.
export function normalizeLink(href, sourcePath, opts = {}) {
  const { sourceToSlug = SOURCE_TO_SLUG, repoRoot, warn = (msg) => console.warn(msg) } = opts

  if (href.includes("://") || href.startsWith("#") || href.startsWith("mailto:")) {
    return href
  }

  let [pathPart, hash] = splitHash(href)
  if (pathPart.endsWith("/")) {
    // Implicit directory index — modules/ and ../modules/ resolve like
    // modules/README.md and ../modules/README.md.
    pathPart += "README.md"
  }
  if (!pathPart.endsWith(".md")) {
    return href
  }

  const baseDir = pathPosix.dirname(sourcePath)
  const resolvedDocsRelative = pathPosix.normalize(pathPosix.join(baseDir, pathPart))

  const slug = sourceToSlug.get(resolvedDocsRelative)
  if (slug) {
    return hash ? `${slug}.html#${hash}` : `${slug}.html`
  }

  // Unknown to the manifest — resolve it as a real repo file (re-based to the
  // flat docs/site/ output root) rather than leaving a link whose original
  // relative depth no longer matches the flattened output.
  const repoRelative = pathPosix.normalize(pathPosix.join("docs", resolvedDocsRelative))
  if (repoRoot && existsSync(pathPosix.join(repoRoot, repoRelative))) {
    const rebased = pathPosix.normalize(pathPosix.join("../..", repoRelative))
    return hash ? `${rebased}#${hash}` : rebased
  }

  warn(`docs: dangling link "${href}" in ${sourcePath} (resolved to docs/${resolvedDocsRelative}, no such file)`)
  return href
}

function splitHash(href) {
  const i = href.indexOf("#")
  if (i < 0) return [href, ""]
  return [href.slice(0, i), href.slice(i + 1)]
}

function newMarkdownIt(sourcePath, linkOpts) {
  const md = new MarkdownIt({
    html: true, // existing docs embed raw HTML
    linkify: false,
    typographer: false,
    highlight(code, lang) {
      const highlighted = highlightCode(code, lang)
      const langClass = lang ? ` language-${escapeHtml(lang.trim().toLowerCase())}` : ""
      return `<pre><code class="hljs${langClass}">${highlighted}</code></pre>`
    },
  })

  md.renderer.rules.link_open = (tokens, idx, options, _env, self) => {
    const token = tokens[idx]
    const hrefIndex = token.attrIndex("href")
    if (hrefIndex >= 0) {
      token.attrs[hrefIndex][1] = normalizeLink(token.attrs[hrefIndex][1], sourcePath, linkOpts)
    }
    return self.renderToken(tokens, idx, options)
  }

  return md
}

// render converts markdown to HTML for the page at sourcePath (relative to
// docs/) and collects its h2/h3 headings for the TOC.
export function render(sourcePath, markdown, linkOpts = {}) {
  const md = newMarkdownIt(sourcePath, linkOpts)
  md.use(markdownItAnchor, { slugify, level: [2, 3], permalink: markdownItAnchor.permalink.linkInsideHeader({ symbol: "#", placement: "before" }) })

  const tokens = md.parse(markdown, {})
  const headings = collectHeadings(markdown, slugify)
  const html = md.renderer.render(tokens, md.options, {})
  return { html, headings }
}

// collectHeadings parses with a minimal markdown-it (auto heading ids only,
// no permalink injection) so the heading text is clean of the injected
// anchor link. The slugify function is identical to the one `render` uses,
// so the ids match those in the rendered HTML.
function collectHeadings(markdown, slugifyFn) {
  const md = new MarkdownIt({ html: true })
  md.use(markdownItAnchor, { slugify: slugifyFn, level: [2, 3] })
  const tokens = md.parse(markdown, {})

  const headings = []
  for (let i = 0; i < tokens.length; i++) {
    const token = tokens[i]
    if (token.type !== "heading_open" || (token.tag !== "h2" && token.tag !== "h3")) continue
    const id = token.attrGet("id")
    if (!id) continue
    const inline = tokens[i + 1]
    const text = inline && inline.type === "inline" ? extractPlainText(inline) : ""
    headings.push({ level: Number(token.tag.slice(1)), id, text })
  }
  return headings
}

// extractPlainText concatenates the text-bearing children of an inline token
// (text + inline code), skipping formatting markers — the markdown-it
// analogue of docs/gen/markdown.go's nodeText.
function extractPlainText(inlineToken) {
  let out = ""
  for (const child of inlineToken.children || []) {
    if (child.type === "text" || child.type === "code_inline") {
      out += child.content
    } else if (child.type === "softbreak" || child.type === "hardbreak") {
      out += " "
    }
  }
  return out
}
