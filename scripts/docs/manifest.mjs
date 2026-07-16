// Information architecture for the generated docs site.
//
// Port of docs/gen/manifest.go, generalized so `source` is relative to docs/
// (not docs/md/) — the docs tree has more than one root (docs/md/ today;
// docs/env/, docs/design/ etc. are legitimate future siblings, and real
// in-doc links already cross out of docs/md/, e.g. internal/decisions and
// internal/testing pages linking back into guide/). Keying every source
// relative to docs/ lets normalizeLink (markdown.mjs) resolve any of those
// roots uniformly instead of assuming everything lives under docs/md/.
//
// The markdown under docs/md/ is canonical; this manifest only decides the
// ordered navigation and maps each page's slug -> source. build.mjs walks it
// to emit docs/site/<slug>.html. The landing page (index.html) is built
// separately from docs/md/README.md, so it is NOT listed here.

export const SITE_TITLE = "harness-wrapper"
export const SITE_TAGLINE = "Supervise CLI agent harnesses as programmable chat sessions"

// SECTIONS is the ordered sidebar. Add a page by adding an entry here and a
// matching docs/<source>.
export const SECTIONS = [
  {
    block: "User Guide",
    title: "Get Started",
    pages: [{ slug: "getting-started", title: "Getting Started", source: "md/guide/getting-started.md" }],
  },
  {
    block: "User Guide",
    title: "Drive Conversations",
    pages: [
      { slug: "cli", title: "CLI", source: "md/guide/cli.md" },
      { slug: "chat", title: "Chat API", source: "md/guide/chat.md" },
      { slug: "gateway", title: "HTTP Gateway", source: "md/guide/gateway.md" },
    ],
  },
  {
    block: "User Guide",
    title: "Harnesses",
    pages: [
      { slug: "adapters", title: "Adapter Matrix", source: "md/guide/adapters.md" },
      { slug: "troubleshooting", title: "Troubleshooting", source: "md/guide/troubleshooting.md" },
    ],
  },
  {
    block: "Developer",
    title: "Internals",
    pages: [
      { slug: "architecture", title: "Architecture", source: "md/internal/architecture.md" },
      { slug: "wrapper", title: "Wrapper & Status", source: "md/internal/wrapper.md" },
      { slug: "turns", title: "Turns & Adapters", source: "md/internal/turns.md" },
      { slug: "screen", title: "Screen (vt100)", source: "md/internal/screen.md" },
      { slug: "transcript", title: "Transcripts", source: "md/internal/transcript.md" },
    ],
  },
  {
    block: "Developer",
    title: "Versions & Drift",
    pages: [{ slug: "versions-drift", title: "Versions & Drift", source: "md/internal/versions-drift.md" }],
  },
  {
    block: "Developer",
    title: "Testing",
    pages: [
      { slug: "testing", title: "Testing Tiers", source: "md/internal/testing/README.md" },
      { slug: "corpus", title: "Corpus", source: "md/internal/testing/corpus.md" },
      { slug: "fakeharness", title: "Fake Harness", source: "md/internal/testing/fakeharness.md" },
    ],
  },
  {
    block: "Developer",
    title: "Decisions",
    pages: [
      { slug: "adr-001-vt100", title: "ADR-001 · vt100", source: "md/internal/decisions/adr-001-vt100.md" },
      {
        slug: "adr-002-interactive-input",
        title: "ADR-002 · Interactive input",
        source: "md/internal/decisions/adr-002-interactive-input.md",
      },
    ],
  },
  {
    block: "Developer",
    title: "Roadmap",
    pages: [{ slug: "roadmap-v1", title: "Roadmap v1", source: "md/internal/roadmap-v1.md" }],
  },
  {
    block: "Developer",
    title: "Reference",
    pages: [{ slug: "glossary", title: "Glossary", source: "md/internal/glossary.md" }],
  },
]

// ALL_PAGES is the flat list of every page, in nav order.
export const ALL_PAGES = SECTIONS.flatMap((s) => s.pages)

// SOURCE_TO_SLUG maps every source path (relative to docs/) -> its output
// slug, for link rewriting.
export const SOURCE_TO_SLUG = new Map(ALL_PAGES.map((p) => [p.source, p.slug]))
