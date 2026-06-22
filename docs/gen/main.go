// Static site generator for the harness-wrapper docs.
//
// `go run . build` (default) reads the canonical markdown under docs/md/,
// renders it with chroma-highlighted code + theme-aware inlined SVG diagrams,
// wraps each page in the shared layout, and writes docs/html/<slug>.html. The
// landing page is built from docs/md/README.md with an enriched hero and the
// architecture chart. The theme assets (docs/gen/assets/) are copied to
// docs/html/assets/.
//
// `go run . serve` previews the built site at http://localhost:4321.
//
// docs/html/ is a gitignored build artifact; the .md files remain canonical.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

func main() {
	log.SetFlags(0)
	cmd := "build"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	genDir, mdDir, htmlDir, err := resolveDirs()
	if err != nil {
		log.Fatalf("✗ %v", err)
	}

	switch cmd {
	case "build":
		if err := build(genDir, mdDir, htmlDir); err != nil {
			log.Fatalf("✗ build failed: %v", err)
		}
	case "serve":
		if err := serve(htmlDir); err != nil {
			log.Fatalf("✗ serve failed: %v", err)
		}
	default:
		log.Fatalf("✗ unknown command %q (want: build | serve)", cmd)
	}
}

// resolveDirs locates docs/gen, docs/md and docs/html whether the tool is run
// from docs/gen (the Makefile's `cd docs/gen && go run .`) or from the repo root
// (`go run ./docs/gen`).
func resolveDirs() (genDir, mdDir, htmlDir string, err error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", "", err
	}
	// Run from docs/gen.
	if filepath.Base(wd) == "gen" && isDir(filepath.Join(wd, "assets")) {
		docs := filepath.Dir(wd)
		return wd, filepath.Join(docs, "md"), filepath.Join(docs, "html"), nil
	}
	// Run from repo root.
	if isDir(filepath.Join(wd, "docs", "gen")) {
		docs := filepath.Join(wd, "docs")
		return filepath.Join(docs, "gen"), filepath.Join(docs, "md"), filepath.Join(docs, "html"), nil
	}
	return "", "", "", fmt.Errorf("run from docs/gen or the repo root (cwd=%s)", wd)
}

func build(genDir, mdDir, htmlDir string) error {
	// Fresh output dir.
	if err := os.RemoveAll(htmlDir); err != nil {
		return err
	}
	assetsOut := filepath.Join(htmlDir, "assets")
	if err := os.MkdirAll(assetsOut, 0o755); err != nil {
		return err
	}

	// Copy theme assets (styles, app.js).
	for _, name := range []string{"styles.css", "app.js"} {
		if err := copyFile(filepath.Join(genDir, "assets", name), filepath.Join(assetsOut, name)); err != nil {
			return err
		}
	}
	// Generate dual-theme code-highlight CSS.
	light, err := chromaCSS("github", "")
	if err != nil {
		return err
	}
	dark, err := chromaCSS("github-dark", `[data-theme="dark"]`)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(assetsOut, "chroma.css"), []byte(light+"\n"+dark+"\n"), 0o644); err != nil {
		return err
	}

	diagramsDir := filepath.Join(mdDir, "diagrams")
	count := 0

	// Content pages.
	for _, page := range AllPages {
		src := filepath.Join(mdDir, filepath.FromSlash(page.Source))
		data, readErr := os.ReadFile(src)
		if readErr != nil {
			log.Printf("! skipping %s: missing source %s", page.Slug, page.Source)
			continue
		}
		body, headings, renderErr := render(page.Source, data)
		if renderErr != nil {
			return fmt.Errorf("%s: %w", page.Source, renderErr)
		}
		withDiagrams := inlineDiagrams(body, diagramsDir)
		full := renderPage(layoutOptions{
			Slug:     page.Slug,
			Title:    page.Title,
			Body:     withDiagrams,
			Headings: headings,
		})
		if err := os.WriteFile(filepath.Join(htmlDir, page.Slug+".html"), []byte(full), 0o644); err != nil {
			return err
		}
		count++
	}

	// Landing page from docs/md/README.md (+ an enriched hero and the architecture chart).
	if err := buildLanding(mdDir, htmlDir, diagramsDir); err != nil {
		return err
	}
	count++

	fmt.Printf("✓ docs:build → %d pages in %s\n", count, htmlDir)
	return nil
}

func buildLanding(mdDir, htmlDir, diagramsDir string) error {
	data, err := os.ReadFile(filepath.Join(mdDir, "README.md"))
	if err != nil {
		return fmt.Errorf("landing: %w", err)
	}
	body, headings, err := render("README.md", data)
	if err != nil {
		return err
	}
	body = inlineDiagrams(body, diagramsDir)

	hero := `<div class="hero">
  <h1 class="hero-title">` + escapeHTML(SiteTitle) + `</h1>
  <p class="hero-tagline">` + escapeHTML(SiteTagline) + `</p>
  <p class="hero-sub">Run Claude Code, Codex, Gemini and friends under a PTY, classify their
  state into a small normalized vocabulary, and drive them as multi-turn chat sessions — from
  Go, the CLI, or any language over HTTP.</p>
  <div class="hero-actions">
    <a class="btn primary" href="getting-started.html">Get started →</a>
    <a class="btn" href="architecture.html">Architecture</a>
    <a class="btn" href="adapters.html">Supported harnesses</a>
  </div>
</div>`

	arch := ""
	if isFile(filepath.Join(diagramsDir, "architecture.svg")) {
		arch = inlineDiagrams(`<img src="diagrams/architecture.svg" alt="System architecture">`, diagramsDir)
	}

	full := renderPage(layoutOptions{
		Slug:      "index",
		Title:     "Overview",
		Body:      hero + "\n" + arch + "\n" + body,
		Headings:  headings,
		IsLanding: true,
	})
	return os.WriteFile(filepath.Join(htmlDir, "index.html"), []byte(full), 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
