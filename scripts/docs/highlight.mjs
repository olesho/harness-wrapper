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

// selectorRe matches the selector portion of a single highlight.js CSS rule,
// allowing an optional leading "/* comment */". Mirrors docs/gen/highlight.go's
// selectorRe.
const selectorRe = /^(\/\*.*?\*\/\s*)?([^{]+?)(\s*\{)/gm

// scopeCSS prefixes every selector in css with scope, preserving any leading
// comment. Comma-separated selector lists are handled element-by-element.
export function scopeCSS(css, scope) {
  return css.replace(selectorRe, (match, comment, selectors, brace) => {
    const parts = selectors.split(",").map((p) => `${scope} ${p.trim()}`)
    return `${comment || ""}${parts.join(", ")}${brace}`
  })
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
