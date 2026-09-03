package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/versions"
)

func fakeRegistry(t *testing.T, m map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for pkg, ver := range m {
		v := ver
		mux.HandleFunc("/"+pkg+"/latest", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"` + v + `"}`))
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		name     string
		anyErr   bool
		anyDrift bool
		want     int
	}{
		{"all match", false, false, 0},
		{"drift", false, true, 1},
		{"probe error", true, false, 2},
		{"error dominates drift", true, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.anyErr, tc.anyDrift); got != tc.want {
				t.Errorf("exitCode(%v, %v) = %d, want %d", tc.anyErr, tc.anyDrift, got, tc.want)
			}
		})
	}
}

func TestCheckMatch(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{"@foo/bar": "1.2.3"})
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	rows, anyErr, anyDrift := check(client, srv.URL, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.2.3"},
	})
	if anyErr || anyDrift {
		t.Fatalf("expected no error/drift; got rows=%+v err=%v drift=%v", rows, anyErr, anyDrift)
	}
	if rows[0].Status != "match" {
		t.Errorf("expected status=match, got %q", rows[0].Status)
	}
}

func TestCheckDrift(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{"@foo/bar": "1.2.4"})
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	rows, _, anyDrift := check(client, srv.URL, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.2.3"},
	})
	if !anyDrift {
		t.Errorf("expected drift, got rows=%+v", rows)
	}
	if rows[0].Status != "drift" {
		t.Errorf("expected status=drift, got %q", rows[0].Status)
	}
}

func TestCheckUnpinned(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{"@foo/bar": "1.2.3"})
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	rows, anyErr, anyDrift := check(client, srv.URL, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: ""},
	})
	if anyErr || anyDrift {
		t.Errorf("expected no error/drift on unpinned, got err=%v drift=%v", anyErr, anyDrift)
	}
	if rows[0].Status != "unpinned" {
		t.Errorf("expected status=unpinned, got %q", rows[0].Status)
	}
	if rows[0].Latest != "1.2.3" {
		t.Errorf("expected latest to be reported even when unpinned, got %q", rows[0].Latest)
	}
}

func TestCheckRegistryError(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{}) // no packages → 404 fallthrough
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	rows, anyErr, _ := check(client, srv.URL, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.0.0"},
	})
	if !anyErr {
		t.Fatalf("expected error, got %+v", rows)
	}
	if rows[0].Status != "error" {
		t.Errorf("expected status=error, got %q", rows[0].Status)
	}
	if !strings.Contains(rows[0].Error, "404") {
		t.Errorf("expected error to mention HTTP 404, got %q", rows[0].Error)
	}
}

func TestCheckMalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/@foo/bar/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	rows, anyErr, _ := check(client, srv.URL, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.0.0"},
	})
	if !anyErr {
		t.Fatalf("expected error for malformed JSON, got %+v", rows)
	}
}

func TestCheckEmptyVersionField(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{"@foo/bar": ""})
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	rows, anyErr, _ := check(client, srv.URL, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.0.0"},
	})
	if !anyErr {
		t.Fatalf("expected error for empty version, got %+v", rows)
	}
}

// The verdict line is what text consumers (the release-check cron greps for
// "drift detected") actually read, because `go run` collapses this command's
// exit code. These cases drive the same path the real run takes — check()
// against an httptest registry, then writeVerdict on its result — so a future
// change that folds the error case back into the drift case fails here.

func verdictFor(t *testing.T, registry string, client *http.Client, all map[string]versions.Entry) string {
	t.Helper()
	_, anyErr, anyDrift := check(client, registry, all)
	var buf strings.Builder
	writeVerdict(&buf, anyErr, anyDrift)
	return buf.String()
}

func TestVerdictRegistryErrorIsNotReportedAsDrift(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{}) // every request 404s
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	got := verdictFor(t, srv.URL, client, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.0.0"},
	})
	if !strings.Contains(got, "✗ could not query the npm registry") {
		t.Errorf("expected the registry-error verdict, got %q", got)
	}
	if strings.Contains(got, "drift detected") {
		t.Errorf("an unreachable registry must not claim drift, got %q", got)
	}
	if strings.Contains(got, "all pins match") {
		t.Errorf("an unreachable registry must not claim an all-clear, got %q", got)
	}
}

func TestVerdictDrift(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{"@foo/bar": "1.2.4"})
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	got := verdictFor(t, srv.URL, client, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.2.3"},
	})
	if !strings.Contains(got, "⚠ drift detected") {
		t.Errorf("expected the drift verdict, got %q", got)
	}
}

func TestVerdictAllMatch(t *testing.T) {
	srv := fakeRegistry(t, map[string]string{"@foo/bar": "1.2.3"})
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	got := verdictFor(t, srv.URL, client, map[string]versions.Entry{
		"foo": {Package: "@foo/bar", Pinned: "1.2.3"},
	})
	if !strings.Contains(got, "✓ all pins match latest") {
		t.Errorf("expected the all-clear verdict, got %q", got)
	}
}

// An error alongside real drift still reports the error: a partial probe
// cannot be trusted to have seen every pin, which is why exitCode() lets
// anyErr dominate too.
func TestVerdictErrorDominatesDrift(t *testing.T) {
	var buf strings.Builder
	writeVerdict(&buf, true, true)
	if !strings.Contains(buf.String(), "✗ could not query") {
		t.Errorf("expected the error verdict to dominate, got %q", buf.String())
	}
}

// A partial probe — one package answered, one did not — must still exit 2. The
// pin that WAS compared says nothing about the one that wasn't, so reporting
// this as a clean drift verdict (exit 1) would be the same mislabel one row
// smaller. anyErr dominating anyDrift in exitCode() is what prevents it.
func TestMixedSuccessAndFailureExitsTwo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/@ok/pkg/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"9.9.9"}`))
	})
	mux.HandleFunc("/@bad/pkg/latest", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second

	rows, anyErr, anyDrift := check(client, srv.URL, map[string]versions.Entry{
		"good": {Package: "@ok/pkg", Pinned: "1.0.0"},  // genuine drift
		"bad":  {Package: "@bad/pkg", Pinned: "1.0.0"}, // unreachable
	})
	if !anyErr {
		t.Fatalf("expected an error row, got %+v", rows)
	}
	if !anyDrift {
		t.Fatalf("expected the reachable package to read as drift, got %+v", rows)
	}
	if got := exitCode(anyErr, anyDrift); got != 2 {
		t.Errorf("a partial probe must exit 2, got %d", got)
	}

	byHarness := map[string]Row{}
	for _, r := range rows {
		byHarness[r.Harness] = r
	}
	bad := byHarness["bad"]
	if bad.Status != "error" {
		t.Fatalf("expected the unreachable row to be status=error, got %q", bad.Status)
	}
	if d := detailCell(bad); !strings.Contains(d, "500") {
		t.Errorf("the error row's detail must carry the cause, got %q", d)
	}
	if d := detailCell(byHarness["good"]); d != "" {
		t.Errorf("a non-error row must have a blank detail, got %q", d)
	}
}

func TestWriteTableDetailColumn(t *testing.T) {
	var buf strings.Builder
	writeTable(&buf, []Row{
		{Harness: "good", Package: "@ok/pkg", Pinned: "1.0.0", Latest: "1.0.0", Status: "match"},
		{Harness: "bad", Package: "@bad/pkg", Pinned: "1.0.0", Status: "error", Error: "tls: failed to verify certificate: x509: OSStatus -26276"},
	})
	out := buf.String()

	if !strings.Contains(out, "| harness | package | pinned | latest | status | detail |") {
		t.Errorf("expected the 6-column header, got:\n%s", out)
	}
	if !strings.Contains(out, "|---|---|---|---|---|---|") {
		t.Errorf("expected a 6-column separator, got:\n%s", out)
	}
	if !strings.Contains(out, "OSStatus -26276") {
		t.Errorf("the error row must show its cause in the table, got:\n%s", out)
	}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "| good ") && !strings.HasSuffix(line, "| match |  |") {
			t.Errorf("a non-error row's detail cell must be blank, got %q", line)
		}
	}
}

// A long error must not escape its cell: every row of a Markdown table is one
// line, and a stray `|` shifts every column after it.
func TestWriteTableDetailTruncatesAndFlattens(t *testing.T) {
	long := "line one | with a pipe\nline two\t" + strings.Repeat("x", 300)
	var buf strings.Builder
	writeTable(&buf, []Row{
		{Harness: "bad", Package: "@bad/pkg", Pinned: "1.0.0", Status: "error", Error: long},
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header, separator and one row; got %d lines:\n%s", len(lines), buf.String())
	}
	row := lines[2]
	if got := strings.Count(row, "|"); got != 7 {
		t.Errorf("a 6-column row must have exactly 7 pipes, got %d in %q", got, row)
	}

	cells := strings.Split(row, "|")
	detail := strings.TrimSpace(cells[len(cells)-2])
	if n := len([]rune(detail)); n > detailCellMax {
		t.Errorf("detail cell is %d runes, want <= %d", n, detailCellMax)
	}
	if !strings.HasSuffix(detail, "…") {
		t.Errorf("an over-long detail must be marked as truncated, got %q", detail)
	}
	if strings.Contains(detail, "\n") || strings.Contains(detail, "\t") {
		t.Errorf("detail cell must be single-line, got %q", detail)
	}
}
