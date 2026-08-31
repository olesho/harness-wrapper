// Command check-versions compares the upstream-version pins in
// versions.json against the npm registry's latest published version
// for each declared package. Run via `make check-versions`.
//
// In table mode the run ends with a one-line verdict on stdout — one of
//
//	✓ all pins match latest
//	⚠ drift detected — see docs/md/internal/versions-drift.md when ready
//	✗ could not query the npm registry
//
// The verdict is printed HERE rather than derived from the exit code by a
// caller, because a caller cannot rely on seeing the exit code: `go run`
// collapses any non-zero child status to 1, so under it a registry outage is
// indistinguishable from real drift. `make check-versions` used to invoke this
// through `go run` and switch on that status, which made every npm outage
// announce "drift detected"; the target now builds the binary first so the
// codes below survive, but the verdict stays here so any text consumer gets
// the truth regardless of how it was launched.
//
// Exit codes:
//
//	0 — all pinned versions match latest (or are intentionally blank)
//	1 — at least one pinned version is behind latest
//	2 — could not read versions.json or reach the npm registry
//
// No authentication is required. The tool hits the public npm registry
// endpoint `https://registry.npmjs.org/<package>/latest` and reads the
// `version` field. A 5-second per-request timeout caps the total wall
// clock at ~15s for the three currently-tracked harnesses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/olesho/harness-wrapper/pkg/versions"
)

const npmRegistry = "https://registry.npmjs.org"

func main() {
	var (
		registry = flag.String("registry", npmRegistry, "npm registry base URL (override for testing)")
		timeout  = flag.Duration("timeout", 5*time.Second, "per-package HTTP timeout")
		format   = flag.String("format", "table", "output format: table | json")
	)
	flag.Parse()

	all, err := versions.All()
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-versions: %v\n", err)
		if *format != "json" {
			writeVerdict(os.Stdout, true, false)
		}
		os.Exit(exitCode(true, false))
	}

	client := &http.Client{Timeout: *timeout}
	report, anyErr, anyDrift := check(client, *registry, all)

	switch *format {
	case "json":
		_ = json.NewEncoder(os.Stdout).Encode(report)
	default:
		writeTable(os.Stdout, report)
		writeVerdict(os.Stdout, anyErr, anyDrift)
	}

	os.Exit(exitCode(anyErr, anyDrift))
}

// exitCode maps a check() result to the process exit code.
//
//	0 = all match, 1 = drift, 2 = probe/read error.
func exitCode(anyErr, anyDrift bool) int {
	switch {
	case anyErr:
		return 2
	case anyDrift:
		return 1
	default:
		return 0
	}
}

// writeVerdict prints the one-line human verdict. It takes anyErr/anyDrift
// directly — the two are still separable here, which is exactly what the exit
// code loses on its way through `go run`. anyErr wins: a probe that never
// reached the registry says nothing about whether the pins are current, so
// "could not query" must not be reported as either drift or an all-clear.
func writeVerdict(w io.Writer, anyErr, anyDrift bool) {
	_, _ = fmt.Fprintln(w)
	switch {
	case anyErr:
		_, _ = fmt.Fprintln(w, "✗ could not query the npm registry")
	case anyDrift:
		_, _ = fmt.Fprintln(w, "⚠ drift detected — see docs/md/internal/versions-drift.md when ready")
	default:
		_, _ = fmt.Fprintln(w, "✓ all pins match latest")
	}
}

// Row is one harness's drift status. Exported for the JSON output.
type Row struct {
	Harness string `json:"harness"`
	Package string `json:"package"`
	Pinned  string `json:"pinned"`
	Latest  string `json:"latest"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

func check(client *http.Client, registry string, all map[string]versions.Entry) ([]Row, bool, bool) {
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]Row, 0, len(all))
	var anyErr, anyDrift bool

	for _, name := range names {
		e := all[name]
		row := Row{Harness: name, Package: e.Package, Pinned: e.Pinned}
		latest, err := fetchLatest(client, registry, e.Package)
		switch {
		case err != nil:
			row.Status = "error"
			row.Error = err.Error()
			anyErr = true
		case e.Pinned == "":
			row.Latest = latest
			row.Status = "unpinned"
		case latest == e.Pinned:
			row.Latest = latest
			row.Status = "match"
		default:
			row.Latest = latest
			row.Status = "drift"
			anyDrift = true
		}
		rows = append(rows, row)
	}
	return rows, anyErr, anyDrift
}

// fetchLatest hits <registry>/<package>/latest and returns the parsed
// `version` field.
func fetchLatest(client *http.Client, registry, pkg string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), client.Timeout+time.Second)
	defer cancel()
	url := registry + "/" + pkg + "/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if payload.Version == "" {
		return "", fmt.Errorf("registry response had empty version field")
	}
	return payload.Version, nil
}

// writeTable emits a 4-column Markdown table.
func writeTable(w io.Writer, rows []Row) {
	_, _ = fmt.Fprintln(w, "| harness | package | pinned | latest | status |")
	_, _ = fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, r := range rows {
		latest := r.Latest
		if latest == "" {
			latest = "—"
		}
		pinned := r.Pinned
		if pinned == "" {
			pinned = "—"
		}
		_, _ = fmt.Fprintf(w, "| %s | `%s` | %s | %s | %s |\n", r.Harness, r.Package, pinned, latest, r.Status)
	}
}
