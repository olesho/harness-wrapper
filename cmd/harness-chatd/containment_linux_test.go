//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/containment"
)

// TestContainedConversationOverHTTP opens a contained conversation, reads the
// applied policy back from the open response and the listing, and checks a
// message may restate the same policy but not change it.
func TestContainedConversationOverHTTP(t *testing.T) {
	if _, err := landlock.Probe(); err != nil {
		if v := os.Getenv("HW_LANDLOCK_REQUIRE_ABI"); v != "" && v != "0" {
			t.Fatalf("Landlock ABI 9 required: %v", err)
		}
		t.Skipf("Landlock ABI 9 unavailable: %v", err)
	}
	bin := fakeHarnessBin(t)
	realBin, _ := filepath.EvalSymlinks(bin)
	t.Cleanup(contain.RegisterTestProfile(contain.TestProfile{Harness: "claude-code", ExecDirs: []string{filepath.Dir(realBin)}}))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := fakeScriptEnv(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build())
	var scriptDir string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, fakeharness.EnvVar+"="); ok {
			scriptDir = filepath.Dir(v)
		}
	}
	req := &containment.Request{Kind: containment.KindLandlock, ReadOnly: []string{scriptDir}, PassEnv: []string{fakeharness.EnvVar}}

	ts := httptest.NewServer(NewServer().Routes())
	defer ts.Close()
	body, _ := json.Marshal(openRequest{Harness: "claude-code", BinaryPath: bin, WorkingDir: t.TempDir(), Env: env, Containment: req})
	resp, err := http.Post(ts.URL+"/v1/conversations", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open: %d %s", resp.StatusCode, raw)
	}
	var open openResponse
	_ = json.Unmarshal(raw, &open)
	if open.Containment == nil || open.Containment.Fingerprint == "" {
		t.Fatalf("open response lacks the applied policy: %s", raw)
	}
	defer func() {
		d, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/conversations/"+open.ID, nil)
		_, _ = http.DefaultClient.Do(d)
	}()

	lresp, _ := http.Get(ts.URL + "/v1/conversations")
	var list []conversationSummary
	_ = json.NewDecoder(lresp.Body).Decode(&list)
	_ = lresp.Body.Close()
	if len(list) != 1 || list[0].Containment == nil || list[0].Containment.Fingerprint != open.Containment.Fingerprint {
		t.Fatalf("listing = %+v", list)
	}

	cresp, _ := http.Post(ts.URL+"/v1/conversations/"+open.ID+"/control", "application/json", nil)
	var ctl controlResponse
	_ = json.NewDecoder(cresp.Body).Decode(&ctl)
	_ = cresp.Body.Close()
	other := req.Clone()
	other.RestrictTCP = true
	b, _ := json.Marshal(sendRequest{Token: ctl.Token, Text: "hi", Containment: other})
	status, rawSend := postRaw(t, ts.URL+"/v1/conversations/"+open.ID+"/messages", string(b))
	if status != http.StatusBadRequest || !strings.Contains(string(rawSend), "invalid_config") {
		t.Fatalf("changed policy on a message: %d %s", status, rawSend)
	}
}
