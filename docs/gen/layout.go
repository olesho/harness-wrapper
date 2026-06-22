// The single HTML shell: <head>, sidebar nav (from the manifest), header with
// theme toggle, content slot, in-page TOC, footer. Relative asset paths so every
// page works over a static server AND file://.
package main

import "strings"

type layoutOptions struct {
	Slug      string    // output slug of the current page; "index" for the landing
	Title     string    // page <title> / header title
	Body      string    // rendered + diagram-inlined body HTML
	Headings  []Heading // headings for the in-page TOC
	IsLanding bool      // true for the landing page (hero styling, no TOC)
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }

func navSection(s NavSection, currentSlug string) string {
	var links strings.Builder
	for _, p := range s.Pages {
		active := ""
		if p.Slug == currentSlug {
			active = ` class="active" aria-current="page"`
		}
		links.WriteString(`<li><a href="` + p.Slug + `.html"` + active + `>` + escapeHTML(p.Title) + "</a></li>\n")
	}
	return `<div class="nav-section">
  <h4>` + escapeHTML(s.Title) + `</h4>
  <ul>
` + links.String() + `  </ul>
</div>`
}

func sidebarNav(currentSlug string) string {
	// Group sections by their top-level block, preserving first-seen block order.
	type bucket struct {
		block    string
		sections []NavSection
	}
	var blocks []*bucket
	find := func(b string) *bucket {
		for _, x := range blocks {
			if x.block == b {
				return x
			}
		}
		nb := &bucket{block: b}
		blocks = append(blocks, nb)
		return nb
	}
	for _, s := range Sections {
		b := find(s.Block)
		b.sections = append(b.sections, s)
	}

	var rendered strings.Builder
	for _, b := range blocks {
		rendered.WriteString(`<div class="nav-block">
  <p class="nav-block-title">` + escapeHTML(b.block) + `</p>
`)
		for _, s := range b.sections {
			rendered.WriteString(navSection(s, currentSlug) + "\n")
		}
		rendered.WriteString("</div>\n")
	}

	homeActive := ""
	if currentSlug == "index" {
		homeActive = " active"
	}
	return `<nav class="sidebar-nav" aria-label="Documentation">
  <a class="nav-home` + homeActive + `" href="index.html">Overview</a>
` + rendered.String() + `</nav>`
}

func tocList(headings []Heading) string {
	if len(headings) < 2 {
		return ""
	}
	var items strings.Builder
	for _, h := range headings {
		items.WriteString(`<li class="toc-h` + itoa(h.Level) + `"><a href="#` + h.ID + `">` + escapeHTML(h.Text) + "</a></li>\n")
	}
	return `<aside class="toc" aria-label="On this page">
  <h4>On this page</h4>
  <ul>
` + items.String() + `  </ul>
</aside>`
}

func itoa(n int) string {
	if n == 2 {
		return "2"
	}
	return "3"
}

func renderPage(opts layoutOptions) string {
	toc := ""
	if !opts.IsLanding {
		toc = tocList(opts.Headings)
	}
	contentClass := "content"
	if opts.IsLanding {
		contentClass = "content landing"
	}
	mainClass := ""
	if toc != "" {
		mainClass = "with-toc"
	}

	return `<!doctype html>
<html lang="en" data-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + escapeHTML(opts.Title) + ` · ` + escapeHTML(SiteTitle) + `</title>
<meta name="description" content="` + escapeHTML(SiteTagline) + `">
<link rel="stylesheet" href="assets/styles.css">
<link rel="stylesheet" href="assets/chroma.css">
<script>
  // Apply the stored theme before paint to avoid a flash.
  try {
    var t = localStorage.getItem('hw-theme');
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
    <span class="brand-name">` + escapeHTML(SiteTitle) + `</span>
  </a>
  <span class="brand-tagline">` + escapeHTML(SiteTagline) + `</span>
  <button class="theme-toggle" aria-label="Toggle light/dark theme" title="Toggle theme">
    <span class="theme-icon-light">☀</span><span class="theme-icon-dark">☾</span>
  </button>
</header>
<div class="layout">
  <aside class="sidebar" id="sidebar">
` + sidebarNav(opts.Slug) + `
  </aside>
  <main id="main" class="` + mainClass + `">
    <article class="` + contentClass + `">
` + opts.Body + `
    </article>
` + toc + `
  </main>
</div>
<footer class="site-footer">
  <p>Generated from the canonical <code>docs/md/*.md</code> with <code>make docs</code>.
  The markdown is the source of truth; this site is a build artifact.</p>
</footer>
<script src="assets/app.js" defer></script>
</body>
</html>`
}
