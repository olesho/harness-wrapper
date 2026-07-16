// highlight.js class-based syntax highlighting + dual-theme CSS generation.
//
// Port of docs/gen/highlight.go. Code blocks are rendered class-based (see
// markdown.mjs), so the same HTML works for both themes; the colors come
// from two generated stylesheets. The light stylesheet (github) applies by
// default; the dark one (github-dark) is scoped under [data-theme="dark"]
// so it wins only in dark mode — a direct port of chroma's
// chromaCSS/scopeCSS dual-theme trick.

import { createRequire } from "node:module"
import { readFileSync } from "node:fs"
import hljs from "highlight.js"

const require = createRequire(import.meta.url)

const htmlEscapes = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }
export function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => htmlEscapes[c])
}

// highlightCode returns class-based HTML (highlight.js's own span classes,
// e.g. hljs-keyword) for `code` in `lang`. Falls back to escaped plain text
// for an unknown/absent language, mirroring chroma's graceful fallback.
export function highlightCode(code, lang) {
  const language = (lang || "").trim().toLowerCase()
  try {
    if (language && hljs.getLanguage(language)) {
      return hljs.highlight(code, { language, ignoreIllegals: true }).value
    }
    return escapeHtml(code)
  } catch {
    return escapeHtml(code)
  }
}

// scopeCSS prefixes every selector in css with scope, preserving comments and
// rule bodies verbatim. Comma-separated selector lists are handled
// element-by-element.
//
// docs/gen/highlight.go's scopeCSS is a single regex pass, safe only because
// chroma emits one rule per line (no newlines between a selector and its
// "{"). highlight.js ships normally-formatted, human-readable CSS instead —
// selector lists and leading doc comments both span multiple lines — so a
// line-oriented regex would walk a non-greedy match straight through a rule
// body into the next selector. This is a small forward-scanning state
// machine instead: outside a rule body, text is buffered as a candidate
// selector until a "{" or a "/* */" comment is seen; inside a rule body,
// everything up to the matching "}" passes through untouched.
export function scopeCSS(css, scope) {
  let out = ""
  let selectorBuf = ""
  let inRule = false
  let i = 0

  while (i < css.length) {
    if (!inRule && css.startsWith("/*", i)) {
      out += selectorBuf
      selectorBuf = ""
      const end = css.indexOf("*/", i + 2)
      const commentEnd = end === -1 ? css.length : end + 2
      out += css.slice(i, commentEnd)
      i = commentEnd
      continue
    }
    const ch = css[i]
    if (!inRule && ch === "{") {
      out += `${prefixSelectors(selectorBuf, scope)} {`
      selectorBuf = ""
      inRule = true
    } else if (inRule && ch === "}") {
      out += "}"
      inRule = false
    } else if (inRule) {
      out += ch
    } else {
      selectorBuf += ch
    }
    i++
  }
  return out + selectorBuf
}

function prefixSelectors(raw, scope) {
  const leading = raw.match(/^\s*/)[0]
  const trimmed = raw.trim()
  if (!trimmed) return leading
  return leading + trimmed.split(",").map((p) => `${scope} ${p.trim()}`).join(", ")
}

function readThemeCSS(themeName) {
  const cssPath = require.resolve(`highlight.js/styles/${themeName}.css`)
  return readFileSync(cssPath, "utf8").trim()
}

// buildHighlightCSS returns the light (github, unscoped) + dark (github-dark,
// scoped under [data-theme="dark"]) stylesheet, concatenated — the dual-theme
// port of chroma's chromaCSS("github", "") + chromaCSS("github-dark", ...).
export function buildHighlightCSS() {
  const light = readThemeCSS("github")
  const dark = scopeCSS(readThemeCSS("github-dark"), '[data-theme="dark"]')
  return `${light}\n${dark}\n`
}
