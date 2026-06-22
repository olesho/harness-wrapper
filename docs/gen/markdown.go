// Markdown -> HTML rendering for the docs site.
//
// goldmark (GFM tables) + auto heading ids (for the TOC) + an anchor extension
// (hover "#" permalinks) + chroma for class-based, dual-theme syntax
// highlighting (see highlight.go). In-doc .md links are rewritten to the flat
// generated <slug>.html. Mirrors orche's markdown-it + markdown-it-anchor +
// Shiki pipeline.
package main

import (
	"bytes"
	"path"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"go.abhg.dev/goldmark/anchor"
)

// Heading is an h2/h3 collected for the in-page TOC.
type Heading struct {
	Level int
	ID    string
	Text  string
}

// render converts markdown to HTML for the page at sourcePath and collects its
// h2/h3 headings for the TOC.
func render(sourcePath string, markdown []byte) (string, []Heading, error) {
	md := newMarkdown(sourcePath)
	var buf bytes.Buffer
	if err := md.Convert(markdown, &buf); err != nil {
		return "", nil, err
	}
	return buf.String(), collectHeadings(markdown), nil
}

// newMarkdown builds a goldmark instance bound to one source path (for link
// rewriting).
func newMarkdown(sourcePath string) goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			highlighting.NewHighlighting(
				highlighting.WithStyle("github"),
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
			),
			&anchor.Extender{
				Texter:   anchor.Text("#"),
				Position: anchor.Before,
			},
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(linkRewriter{sourcePath}, 100)),
		),
		goldmark.WithRendererOptions(
			html.WithUnsafe(), // existing docs embed raw HTML
		),
	)
}

// collectHeadings parses with a minimal goldmark (auto heading ids only) so the
// heading text is clean of the injected anchor link. The auto-id algorithm is
// deterministic, so the ids match those rendered by newMarkdown.
func collectHeadings(markdown []byte) []Heading {
	md := goldmark.New(goldmark.WithParserOptions(parser.WithAutoHeadingID()))
	doc := md.Parser().Parse(text.NewReader(markdown))
	var out []Heading
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok || (h.Level != 2 && h.Level != 3) {
			return ast.WalkContinue, nil
		}
		id := ""
		if v, ok := h.AttributeString("id"); ok {
			if b, ok := v.([]byte); ok {
				id = string(b)
			} else if s, ok := v.(string); ok {
				id = s
			}
		}
		if id == "" {
			return ast.WalkContinue, nil
		}
		out = append(out, Heading{Level: h.Level, ID: id, Text: nodeText(n, markdown)})
		return ast.WalkContinue, nil
	})
	return out
}

// nodeText extracts the plain-text content of a node (text + string segments,
// e.g. inside emphasis or code spans).
func nodeText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(source))
		case *ast.String:
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// linkRewriter rewrites in-doc .md links to flat <slug>.html destinations.
type linkRewriter struct{ source string }

func (t linkRewriter) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if link, ok := n.(*ast.Link); ok {
			link.Destination = []byte(normalizeLink(string(link.Destination), t.source))
		}
		return ast.WalkContinue, nil
	})
}

// normalizeLink resolves an in-doc link so it works on the flat generated site.
// Doc .md sources map to their output slug <slug>.html (preserving any #anchor).
// Non-doc links (http(s), code paths, anchors, mailto) pass through unchanged.
func normalizeLink(href, sourcePath string) string {
	if strings.Contains(href, "://") || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
		return href
	}
	pathPart, hash, _ := strings.Cut(href, "#")
	if !strings.HasSuffix(pathPart, ".md") {
		return href
	}
	baseDir := ""
	if i := strings.LastIndex(sourcePath, "/"); i >= 0 {
		baseDir = sourcePath[:i]
	}
	resolved := path.Clean(path.Join(baseDir, pathPart))
	slug, ok := SourceToSlug[resolved]
	if !ok {
		return href // unknown target — leave as-is rather than fabricate
	}
	if hash != "" {
		return slug + ".html#" + hash
	}
	return slug + ".html"
}
