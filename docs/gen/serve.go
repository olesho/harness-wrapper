// Tiny static file server over docs/html/ for previewing the generated site.
// `go run . serve` -> http://localhost:4321 (override with PORT).
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func serve(htmlDir string) error {
	if _, err := os.Stat(htmlDir); err != nil {
		return fmt.Errorf("docs/html/ not found — run `go run . build` first")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "4321"
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/":
			p = "/index.html"
		case !strings.Contains(filepath.Base(p), "."):
			// Pretty URLs: /agent -> /agent.html
			p += ".html"
		}
		clean := filepath.Clean(filepath.Join(htmlDir, filepath.FromSlash(p)))
		if !strings.HasPrefix(clean, htmlDir) {
			http.NotFound(w, r)
			return
		}
		if info, err := os.Stat(clean); err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, clean)
	})

	log.Printf("docs site → http://localhost:%s", port)
	return http.ListenAndServe(":"+port, handler)
}
