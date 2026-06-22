// Chroma syntax-highlight CSS generation.
//
// Code blocks are rendered class-based (see markdown.go), so the same HTML works
// for both themes; the colors come from two generated stylesheets. The light
// stylesheet (github) applies by default; the dark one (github-dark) is scoped
// under [data-theme="dark"] so it wins only in dark mode. This mirrors orche's
// Shiki dual-theme setup, but with chroma.
package main

import (
	"bytes"
	"regexp"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
)

// chromaCSS returns the class-based CSS for a chroma style. When scope is
// non-empty (e.g. [data-theme="dark"]) every selector is prefixed with it.
func chromaCSS(styleName, scope string) (string, error) {
	style := styles.Get(styleName) // never nil — falls back to a default
	formatter := chromahtml.New(chromahtml.WithClasses(true))
	var buf bytes.Buffer
	if err := formatter.WriteCSS(&buf, style); err != nil {
		return "", err
	}
	css := buf.String()
	if scope != "" {
		css = scopeCSS(css, scope)
	}
	return css, nil
}

// selectorRe matches the selector portion of a single chroma CSS rule, allowing
// an optional leading "/* comment */". chroma writes one rule per line.
var selectorRe = regexp.MustCompile(`(?m)^(/\*.*?\*/\s*)?([^{]+?)(\s*\{)`)

// scopeCSS prefixes every selector in css with scope, preserving any leading
// comment. Comma-separated selector lists are handled element-by-element.
func scopeCSS(css, scope string) string {
	return selectorRe.ReplaceAllStringFunc(css, func(match string) string {
		m := selectorRe.FindStringSubmatch(match)
		comment, selectors, brace := m[1], m[2], m[3]
		parts := strings.Split(selectors, ",")
		for i, p := range parts {
			parts[i] = scope + " " + strings.TrimSpace(p)
		}
		return comment + strings.Join(parts, ", ") + brace
	})
}
