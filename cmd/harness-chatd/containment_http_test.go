package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/containment"
)

func postRaw(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestCapabilities: the route clients consult before sending containment
// lists landlock exactly where it can be supported.
func TestCapabilities(t *testing.T) {
	ts := httptest.NewServer(NewServer().Routes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var caps capabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, decode %v", resp.StatusCode, err)
	}
	want := runtime.GOOS == "linux"
	if got := slices.Contains(caps.Containment.Kinds, containment.KindLandlock); got != want {
		t.Fatalf("kinds %v on %s", caps.Containment.Kinds, runtime.GOOS)
	}
	if caps.Containment.Kinds == nil {
		t.Fatal("kinds must be a list, never null")
	}
}

// TestPreviousClientBodyStillOpens pins G9 on the wire: the exact body the
// previous release's clients post opens an uncontained conversation, with no
// containment key in the response.
func TestPreviousClientBodyStillOpens(t *testing.T) {
	bin := fakeHarnessBin(t)
	env := fakeScriptEnv(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build())
	envJSON, _ := json.Marshal(env)
	ts := httptest.NewServer(NewServer().Routes())
	defer ts.Close()
	body := `{"harness":"claude-code","binary_path":"` + bin + `","args":[],"working_dir":"","env":` + string(envJSON) + `,"cols":0,"rows":0}`
	status, raw := postRaw(t, ts.URL+"/v1/conversations", body)
	if status != http.StatusCreated {
		t.Fatalf("status %d: %s", status, raw)
	}
	var open map[string]any
	_ = json.Unmarshal(raw, &open)
	if _, ok := open["containment"]; ok {
		t.Fatalf("uncontained open echoed containment: %s", raw)
	}
	id, _ := open["id"].(string)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/conversations/"+id, nil)
	_, _ = http.DefaultClient.Do(req)
}

// TestContainmentOnUncontainedConversationRefused: a message carrying
// containment on an uncontained conversation is refused before the text
// reaches the harness.
func TestContainmentOnUncontainedConversationRefused(t *testing.T) {
	bin := fakeHarnessBin(t)
	env := fakeScriptEnv(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build())
	ts := httptest.NewServer(NewServer().Routes())
	defer ts.Close()
	id := openConversation(t, ts, openRequest{Harness: "claude-code", BinaryPath: bin, Env: env})
	resp, err := http.Post(ts.URL+"/v1/conversations/"+id+"/control", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var ctl controlResponse
	_ = json.NewDecoder(resp.Body).Decode(&ctl)
	_ = resp.Body.Close()
	body, _ := json.Marshal(sendRequest{Token: ctl.Token, Text: "hello", Containment: &containment.Request{Kind: containment.KindLandlock}})
	status, raw := postRaw(t, ts.URL+"/v1/conversations/"+id+"/messages", string(body))
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "invalid_config") {
		t.Fatalf("status %d: %s", status, raw)
	}
}

// TestOpenContainmentRefusedOffLinux: off Linux the request is invalid
// config, never a silent uncontained launch.
func TestOpenContainmentRefusedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux supports containment")
	}
	bin := fakeHarnessBin(t)
	ts := httptest.NewServer(NewServer().Routes())
	defer ts.Close()
	for _, path := range []string{"/v1/conversations", "/v1/turns"} {
		var body []byte
		if path == "/v1/turns" {
			body, _ = json.Marshal(runTurnRequest{Harness: "claude", BinaryPath: bin, Prompt: "hi", Containment: &containment.Request{Kind: containment.KindLandlock}})
		} else {
			body, _ = json.Marshal(openRequest{Harness: "claude-code", BinaryPath: bin, Containment: &containment.Request{Kind: containment.KindLandlock}})
		}
		resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "invalid_config") {
			t.Fatalf("%s: status %d: %s", path, resp.StatusCode, raw)
		}
	}
}
