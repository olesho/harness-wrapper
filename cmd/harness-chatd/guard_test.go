package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRequireLoopbackBind(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:8080", false},
		{"127.0.0.2:8080", false}, // all of 127.0.0.0/8 is loopback
		{"localhost:8080", false},
		{"LOCALHOST:8080", false},
		{"[::1]:8080", false},
		{"0.0.0.0:8080", true},
		{":8080", true}, // an empty host listens on every interface
		{"[::]:8080", true},
		{"192.168.1.5:8080", true},
		{"example.com:8080", true},
		{"127.0.0.1", true}, // no port
	} {
		if err := requireLoopbackBind(tc.addr); (err != nil) != tc.wantErr {
			t.Errorf("requireLoopbackBind(%q) = %v, want error %v", tc.addr, err, tc.wantErr)
		}
	}
}

func TestRun_RefusesNonLoopbackBind(t *testing.T) {
	if err := run([]string{"--bind", "0.0.0.0:0"}); err == nil {
		t.Fatal("run --bind 0.0.0.0:0: want an error, got nil")
	}
}

func TestLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		host        string
		header      map[string]string
		allowRemote bool
		want        int
		wantCode    string
	}{
		{name: "loopback ip", host: "127.0.0.1:8080", want: http.StatusOK},
		{name: "ipv6 loopback", host: "[::1]:8080", want: http.StatusOK},
		{name: "localhost no port", host: "localhost", want: http.StatusOK},
		{name: "own navigation", host: "localhost:8080", header: map[string]string{"Sec-Fetch-Site": "none"}, want: http.StatusOK},
		{name: "node fetch", host: "127.0.0.1:8080", header: map[string]string{"Sec-Fetch-Mode": "cors"}, want: http.StatusOK},
		{name: "rebound name", host: "attacker.example:8080", want: http.StatusForbidden, wantCode: "host_refused"},
		{name: "lan ip", host: "192.168.1.5:8080", want: http.StatusForbidden, wantCode: "host_refused"},
		{name: "remote host allowed", host: "chatd.internal:8080", allowRemote: true, want: http.StatusOK},
		{name: "cross-site origin", host: "127.0.0.1:8080", header: map[string]string{"Origin": "https://evil.example"}, want: http.StatusForbidden, wantCode: "browser_refused"},
		{name: "null origin", host: "127.0.0.1:8080", header: map[string]string{"Origin": "null"}, want: http.StatusForbidden, wantCode: "browser_refused"},
		{name: "same origin", host: "127.0.0.1:8080", header: map[string]string{"Origin": "http://127.0.0.1:8080"}, want: http.StatusForbidden, wantCode: "browser_refused"},
		{name: "cross-site no origin", host: "127.0.0.1:8080", header: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: http.StatusForbidden, wantCode: "browser_refused"},
		{name: "rebound same-origin", host: "attacker.example:8080", header: map[string]string{"Sec-Fetch-Site": "same-origin"}, want: http.StatusForbidden, wantCode: "browser_refused"},
		{name: "browser despite allow-remote", host: "chatd.internal:8080", header: map[string]string{"Origin": "https://evil.example"}, allowRemote: true, want: http.StatusForbidden, wantCode: "browser_refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}), tc.allowRemote)
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Host = tc.host
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if tc.wantCode == "" {
				return
			}
			var got errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode error body %q: %v", rec.Body, err)
			}
			if got.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", got.Code, tc.wantCode)
			}
		})
	}
}

// TestDriveByTurnLaunchesNothing sends the request a web page can make
// without a preflight: a text/plain POST to /v1/turns carrying Origin. The
// binary it names would create a file; the guard must refuse before any
// handler reads the body.
func TestDriveByTurnLaunchesNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "payload.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer().Routes())
	defer ts.Close()

	body, _ := json.Marshal(runTurnRequest{Harness: "claude", BinaryPath: script, Prompt: "x"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/turns", bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/turns: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the payload ran")
	}
}
