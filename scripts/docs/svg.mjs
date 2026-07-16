// Inline bespoke diagram SVGs into the rendered page HTML.
//
// Port of docs/gen/svg.go. Markdown references each diagram as a normal
// relative image link, e.g. ![System architecture](../diagrams/architecture.svg).
// On GitHub that renders as a static image. At build time we replace the
// <img> with the SVG's source inlined into the page, so the chart inherits
// the page CSS variables (via currentColor / var(--...)) and is theme-aware
// and crisp at any zoom.

import { readFileSync } from "node:fs"
import { join } from "node:path"

// diagramRe matches an <img> whose src points at diagrams/<name>.svg,
// capturing the bare <name> (depth-independent: ../diagrams/x.svg and
// ../../diagrams/x.svg both match).
const diagramRe = /<img\s+[^>]*src="[^"]*diagrams\/([\w-]+)\.svg"[^>]*>/g
const altRe = /alt="([^"]*)"/

// inlineDiagrams replaces every diagram <img> in html with the inlined SVG
// from diagramsDir. The alt text (if any) becomes a <figcaption>.
export function inlineDiagrams(html, diagramsDir) {
  return html.replace(diagramRe, (imgTag, name) => {
    let svg
    try {
      svg = readFileSync(join(diagramsDir, `${name}.svg`), "utf8").trim()
    } catch {
      return imgTag // leave the img if the asset is missing
    }
    const altMatch = altRe.exec(imgTag)
    altRe.lastIndex = 0
    const caption = altMatch && altMatch[1] ? `<figcaption>${altMatch[1]}</figcaption>` : ""
    return `<figure class="diagram">${svg}${caption}</figure>`
  })
}
