// Information architecture for the generated docs site.
//
// The markdown under docs/md/ is canonical; this manifest only decides the
// ordered navigation and maps each page's slug -> source .md. build.go walks it
// to emit docs/html/<slug>.html. The landing page (index.html) is built
// separately from docs/md/README.md, so it is NOT listed here.
package main

// PageEntry is one documentation page.
type PageEntry struct {
	Slug   string // output filename without extension -> docs/html/<slug>.html
	Title  string // nav label + <title>
	Source string // source markdown, relative to docs/md/
}

// NavSection is a labeled group of pages within a top-level block.
type NavSection struct {
	// Block is the top-level documentation block this section belongs to. The
	// blocks render as separate, labeled groups in the sidebar: customer-facing
	// usage vs. internal development docs.
	Block string // "User Guide" | "Developer"
	Title string
	Pages []PageEntry
}

const (
	SiteTitle   = "harness-wrapper"
	SiteTagline = "Supervise CLI agent harnesses as programmable chat sessions"
)

// Sections is the ordered sidebar. Add a page by adding a PageEntry here and a
// matching docs/md/<source>.
var Sections = []NavSection{
	{Block: "User Guide", Title: "Get Started", Pages: []PageEntry{
		{"getting-started", "Getting Started", "guide/getting-started.md"},
	}},
	{Block: "User Guide", Title: "Drive Conversations", Pages: []PageEntry{
		{"cli", "CLI", "guide/cli.md"},
		{"chat", "Chat API", "guide/chat.md"},
		{"gateway", "HTTP Gateway", "guide/gateway.md"},
		{"clients", "Client Libraries", "guide/clients.md"},
	}},
	{Block: "User Guide", Title: "Harnesses", Pages: []PageEntry{
		{"adapters", "Adapter Matrix", "guide/adapters.md"},
		{"permissions", "Permissions & Sandboxing", "guide/permissions.md"},
		{"troubleshooting", "Troubleshooting", "guide/troubleshooting.md"},
	}},
	{Block: "Developer", Title: "Internals", Pages: []PageEntry{
		{"architecture", "Architecture", "internal/architecture.md"},
		{"packages", "Repository Map", "internal/packages.md"},
		{"wrapper", "Wrapper & Status", "internal/wrapper.md"},
		{"turns", "Turns & Adapters", "internal/turns.md"},
		{"screen", "Screen (vt100)", "internal/screen.md"},
		{"transcript", "Transcripts", "internal/transcript.md"},
	}},
	{Block: "Developer", Title: "Running One Job", Pages: []PageEntry{
		{"harness", "Harness Profiles & Runs", "internal/harness.md"},
		{"oneshot", "One-shot Turns", "internal/oneshot.md"},
		{"turnproto", "Structured Turn Protocol", "internal/turnproto.md"},
		{"env", "Execution Environments", "internal/env.md"},
	}},
	{Block: "Developer", Title: "Versions & Drift", Pages: []PageEntry{
		{"discovery", "Discovery", "internal/discovery.md"},
		{"versions-drift", "Versions & Drift", "internal/versions-drift.md"},
	}},
	{Block: "Developer", Title: "Testing", Pages: []PageEntry{
		{"testing", "Testing Tiers", "internal/testing/README.md"},
		{"corpus", "Corpus", "internal/testing/corpus.md"},
		{"fakeharness", "Fake Harness", "internal/testing/fakeharness.md"},
		{"conformance", "Conformance Corpus", "internal/testing/conformance.md"},
	}},
	{Block: "Developer", Title: "Decisions", Pages: []PageEntry{
		{"adr-001-vt100", "ADR-001 · vt100", "internal/decisions/adr-001-vt100.md"},
		{"adr-002-interactive-input", "ADR-002 · Interactive input", "internal/decisions/adr-002-interactive-input.md"},
		{"adr-003-env-visibility", "ADR-003 · Env visibility", "internal/decisions/adr-003-env-visibility.md"},
	}},
	{Block: "Developer", Title: "Roadmap", Pages: []PageEntry{
		{"roadmap-v1", "Roadmap v1", "internal/roadmap-v1.md"},
	}},
	{Block: "Developer", Title: "Reference", Pages: []PageEntry{
		{"glossary", "Glossary", "internal/glossary.md"},
	}},
}

// AllPages is the flat list of every page, in nav order.
var AllPages = flattenPages()

// SourceToSlug maps every source .md path (relative to docs/md/) -> its output
// slug, for link rewriting.
var SourceToSlug = buildSourceToSlug()

func flattenPages() []PageEntry {
	var out []PageEntry
	for _, s := range Sections {
		out = append(out, s.Pages...)
	}
	return out
}

func buildSourceToSlug() map[string]string {
	m := make(map[string]string, len(AllPages))
	for _, s := range Sections {
		for _, p := range s.Pages {
			m[p.Source] = p.Slug
		}
	}
	return m
}
