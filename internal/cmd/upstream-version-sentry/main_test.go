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
