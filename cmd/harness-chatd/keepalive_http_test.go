package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
)

// TestOpenConversationIsKeptAlive: a gateway conversation lives until its
// client deletes it, idle between messages, so it is opened with keep-alive
// supervision — a reply that mentions a rate limit, followed by that idle,
// must not end it (ADR-006). The seam shows the options the open used;
// pkg/chat's keep-alive tests show what the option does to a live harness.
func TestOpenConversationIsKeptAlive(t *testing.T) {
	var got chat.Options
	orig := chatOpen
	t.Cleanup(func() { chatOpen = orig })
	chatOpen = func(ctx context.Context, opts chat.Options) (*chat.Conversation, error) {
		got = opts
		return orig(ctx, opts)
	}

	bin := fakeHarnessBin(t)
	env := fakeScriptEnv(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build())
	envJSON, _ := json.Marshal(env)
	ts := httptest.NewServer(NewServer().Routes())
	defer ts.Close()
	status, raw := postRaw(t, ts.URL+"/v1/conversations",
		`{"harness":"claude-code","binary_path":"`+bin+`","env":`+string(envJSON)+`}`)
	if status != http.StatusCreated {
		t.Fatalf("status %d: %s", status, raw)
	}
	var open openResponse
	_ = json.Unmarshal(raw, &open)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/conversations/"+open.ID, nil)
	_, _ = http.DefaultClient.Do(req)

	if !got.KeepAliveOnClassification {
		t.Fatal("the gateway opened a conversation without keep-alive supervision")
	}
}
