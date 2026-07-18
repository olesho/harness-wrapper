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

// imgTagRe matches a whole <img …> tag with a single linear [^>]* scan (no
// nested quantifiers, so no super-linear backtracking). srcDiagramRe then pulls
// the diagram <name> out of the matched tag, depth-independently (../diagrams/x.svg
// and ../../diagrams/x.svg both match). Splitting the two avoids the ReDoS-prone
// [^>]*…[^>]* form of a single combined pattern.
const imgTagRe = /<img\s[^>]*>/g
const srcDiagramRe = /src="[^"]*diagrams\/([\w-]+)\.svg"/
const altRe = /alt="([^"]*)"/

// inlineDiagrams replaces every diagram <img> in html with the inlined SVG
// from diagramsDir. The alt text (if any) becomes a <figcaption>. Non-diagram
// <img> tags are returned unchanged.
export function inlineDiagrams(html, diagramsDir) {
  return html.replace(imgTagRe, (imgTag) => {
    const srcMatch = srcDiagramRe.exec(imgTag)
    if (!srcMatch) return imgTag // not a diagram image
    let svg
    try {
      svg = readFileSync(join(diagramsDir, `${srcMatch[1]}.svg`), "utf8").trim()
    } catch {
      return imgTag // leave the img if the asset is missing
    }
    const altMatch = altRe.exec(imgTag)
    altRe.lastIndex = 0
    const caption = altMatch?.[1] ? `<figcaption>${altMatch[1]}</figcaption>` : ""
    return `<figure class="diagram">${svg}${caption}</figure>`
  })
}
