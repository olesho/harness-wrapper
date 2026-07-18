// Inline bespoke diagram SVGs into the rendered page HTML.
//
// Markdown references each diagram as a normal relative image link, e.g.
// ![System architecture](../diagrams/architecture.svg). On GitHub that renders
// as a static image. At build time we replace the <img> with the SVG's source
// inlined into the page, so the chart inherits the page CSS variables (via
// currentColor / var(--...)) and is theme-aware and crisp at any zoom.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// diagramRe matches an <img> whose src points at diagrams/<name>.svg, capturing
// the bare <name> (depth-independent: ../diagrams/x.svg and ../../diagrams/x.svg
// both match).
var (
	diagramRe = regexp.MustCompile(`<img\s+[^>]*src="[^"]*diagrams/([\w-]+)\.svg"[^>]*>`)
	altRe     = regexp.MustCompile(`alt="([^"]*)"`)
)

// inlineDiagrams replaces every diagram <img> in html with the inlined SVG from
// diagramsDir. The alt text (if any) becomes a <figcaption>.
func inlineDiagrams(html, diagramsDir string) string {
	return diagramRe.ReplaceAllStringFunc(html, func(imgTag string) string {
		name := diagramRe.FindStringSubmatch(imgTag)[1]
		data, err := os.ReadFile(filepath.Join(diagramsDir, name+".svg"))
		if err != nil {
			return imgTag // leave the img if the asset is missing
		}
		svg := strings.TrimSpace(string(data))
		caption := ""
		if a := altRe.FindStringSubmatch(imgTag); a != nil && a[1] != "" {
			caption = "<figcaption>" + a[1] + "</figcaption>"
		}
		return `<figure class="diagram">` + svg + caption + `</figure>`
	})
}
