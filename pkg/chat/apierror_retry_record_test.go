package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// TestRecordAPIErrorRetry records claude's API-error retry into the corpus, and
// is the live check that the turn ends as the harness's own record says:
// the real binary, pointed at a local Messages API (ANTHROPIC_BASE_URL) that
// answers 529, with a fresh config root and a placeholder token, so it costs
// nothing and carries no account or machine path but the rebake directory.
//
//	HW_RECORD_API_RETRY=1 go test ./pkg/chat -run RecordAPIErrorRetry -v
//
// It writes two scenarios under test/corpus/claude-code:
//
//   - api-error-retry-recovers: 529 twice, then a reply. claude paints
//     "✻ API error · Retrying in 1s · attempt 1/10" in its status line through
//     the backoff and answers; its transcript holds no error entry.
//   - api-error-retry-gives-up: 529 on every attempt. claude retries, gives up
//     ("API Error: Repeated 529 Overloaded errors …") and writes one tagged
//     entry (error: server_error) when it does.
//
// Each gets bytes.raw (the whole PTY stream), meta.json and transcript.jsonl
// (user and assistant entries, reduced to the fields the readers use).
func TestRecordAPIErrorRetry(t *testing.T) {
	if os.Getenv("HW_RECORD_API_RETRY") != "1" {
		t.Skip("records the API-error retry corpus: set HW_RECORD_API_RETRY=1 (needs the real claude binary; no tokens)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	version, _ := exec.Command(bin, "--version").Output()
	for _, sc := range []struct {
		name   string
		failed int // main requests answered 529 before a reply; -1 for all
		want   TurnState
		wantIn string // in the turn's Text when complete, in its Reason when errored
		notes  string
	}{
		{"api-error-retry-recovers", 2, TurnStateComplete, "RECOVERED_REPLY_OK", "529 twice, then a reply: the status line shows the retry backoff (\"✻ API error · Retrying in Ns · attempt N/10\") in place of the spinner, the footer drops \"esc to interrupt\", and the turn ends with the reply; the transcript holds no error entry."},
		{"api-error-retry-gives-up", -1, TurnStateErrored, "harness tag: server_error", "529 on every attempt: claude retries, gives up with \"API Error: Repeated 529 Overloaded errors …\" painted as the reply and a settled end-of-turn summary, and writes one tagged entry (isApiErrorMessage, error: server_error) to its transcript when it gives up."},
	} {
		t.Run(sc.name, func(t *testing.T) {
			turn := recordAPIErrorScenario(t, bin, strings.TrimSpace(string(version)), sc.name, sc.failed, sc.notes)
			if got := turn.Text + turn.Reason; turn.State != sc.want || !strings.Contains(got, sc.wantIn) {
				t.Fatalf("turn = %+v, want %s carrying %q", turn, sc.want, sc.wantIn)
			}
		})
	}
}

// recordAPIErrorScenario records one scenario and returns the turn it ended
// with, for the caller to check against the harness's own outcome.
func recordAPIErrorScenario(t *testing.T, bin, version, name string, failed int, notes string) Turn {
	var mu sync.Mutex
	mainCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case !strings.HasPrefix(r.URL.Path, "/v1/messages"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "count_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":10}`)
		case bytes.Contains(body, []byte("HW_RECORD_PROMPT")):
			mu.Lock()
			mainCalls++
			n := mainCalls
			mu.Unlock()
			if failed < 0 || n <= failed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(529)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
				return
			}
			writeLocalReply(w, bytes.Contains(body, []byte(`"stream":true`)), "RECOVERED_REPLY_OK")
		default:
			writeLocalReply(w, bytes.Contains(body, []byte(`"stream":true`)), "ok")
		}
	}))
	defer api.Close()

	wd := filepath.Join("/tmp", "hw-rebake", name)
	if err := os.RemoveAll(wd); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	realWD, _ := filepath.EvalSymlinks(wd)
	cfg := t.TempDir()
	seed := func(file string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(cfg, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed(".claude.json", map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
	})
	seed("settings.json", map[string]any{"permissions": map[string]any{"defaultMode": "auto"}})

	conv, err := Open(context.Background(), Options{
		Harness:    chatClaudeCode,
		BinaryPath: bin,
		WorkingDir: wd,
		Env: append(harnessenv.Cleaned(),
			"CLAUDE_CONFIG_DIR="+cfg,
			"ANTHROPIC_BASE_URL="+api.URL,
			"ANTHROPIC_AUTH_TOKEN=placeholder-for-a-local-api"),
		Store:                     newFakeStore(),
		KeepAliveOnClassification: true,
		InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
		}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	sendOneTurnWithin(t, conv, "HW_RECORD_PROMPT: reply with exactly RECOVERED_REPLY_OK", 2*time.Minute)
	turn := waitForTerminalTurn(t, conv, 2*time.Minute)
	t.Logf("%s: turn %s %q (%s)", name, turn.State, turn.Text, turn.Reason)
	time.Sleep(2 * time.Second) // let the settled frame paint
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := conv.Quit(ctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	_, _ = conv.Wrapper().Wait()
	// The whole stream, from the harness's first byte to its exit: the ring
	// holds it all while the session stays under its 64 KB.
	raw := conv.Wrapper().RecentOutput()
	if len(raw) >= 64*1024 {
		t.Fatalf("the session wrote %d bytes, past the recent-output ring; the recording would be truncated", len(raw))
	}
	conv.mu.Lock()
	id := conv.session.HarnessID()
	conv.mu.Unlock()
	_ = conv.Close(ctx)

	out := filepath.Join("..", "..", "test", "corpus", "claude-code", name)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "bytes.raw"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.MarshalIndent(map[string]any{
		"harness":        "claude-code",
		"binary_version": strings.Fields(version)[0],
		"recorded_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"cols":           120,
		"rows":           40,
		"notes": notes + " Recorded with `HW_RECORD_API_RETRY=1 go test ./pkg/chat -run RecordAPIErrorRetry` " +
			"(pkg/chat/apierror_retry_record_test.go): the real claude against a local Messages API, in " + wd +
			" with a fresh CLAUDE_CONFIG_DIR (onboarding done, the directory trusted, permissions.defaultMode auto) " +
			"and a placeholder ANTHROPIC_AUTH_TOKEN, so no account or tokens are involved.",
	}, "", " ")
	if err := os.WriteFile(filepath.Join(out, "meta.json"), append(meta, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "projects", transcriptcc.EncodedCWD(realWD), id+".jsonl")
	lines, err := reducedTranscript(path)
	if err != nil {
		t.Fatalf("transcript %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(out, "transcript.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return turn
}

// reducedTranscript keeps the user and assistant entries of a claude
// transcript, reduced to the fields the transcript readers consume — the shape
// test/corpus/apierror vendors.
func reducedTranscript(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if kind := entry["type"]; kind != "user" && kind != "assistant" {
			continue
		}
		keep := map[string]any{}
		for _, k := range []string{"type", "uuid", "timestamp", "isApiErrorMessage", "error", "version"} {
			if v, ok := entry[k]; ok {
				keep[k] = v
			}
		}
		if msg, ok := entry["message"].(map[string]any); ok {
			keep["message"] = map[string]any{"role": msg["role"], "model": msg["model"], "content": msg["content"]}
		}
		b, _ := json.Marshal(keep)
		out = append(out, string(b))
	}
	return out, nil
}

// writeLocalReply answers a Messages API request with a one-block text reply,
// streamed as server-sent events when the request asked for a stream.
func writeLocalReply(w http.ResponseWriter, stream bool, text string) {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{
			"id": "msg_local", "type": "message", "role": "assistant", "model": "claude-local",
			"content": []map[string]any{{"type": "text", "text": text}}, "stop_reason": "end_turn",
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
		_, _ = w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name, data string) { _, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
	event("message_start", `{"type":"message_start","message":{"id":"msg_local","type":"message","role":"assistant","model":"claude-local","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`)
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`)
	event("message_stop", `{"type":"message_stop"}`)
}
