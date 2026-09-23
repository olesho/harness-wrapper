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
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// TestInterruptLive is the live check of Interrupt (ADR-007) against the real
// claude binary, pointed at a local Messages API that streams slowly, delays its
// first token, or calls a tool on cue — no account, no tokens:
//
//	HW_LIVE_INTERRUPT=1 go test ./pkg/chat -run InterruptLive -v
//
// TestRecordInterrupt runs the same scenarios and also records them into
// test/corpus/claude-code (bytes.raw, meta.json, transcript.jsonl):
//
//	HW_RECORD_INTERRUPT=1 go test ./pkg/chat -run RecordInterrupt -v
func TestInterruptLive(t *testing.T) {
	if os.Getenv("HW_LIVE_INTERRUPT") != "1" {
		t.Skip("interrupts the real claude binary against a local API: set HW_LIVE_INTERRUPT=1 (no tokens)")
	}
	runInterruptScenarios(t, false)
}

func TestRecordInterrupt(t *testing.T) {
	if os.Getenv("HW_RECORD_INTERRUPT") != "1" {
		t.Skip("records the interrupt corpus: set HW_RECORD_INTERRUPT=1 (needs the real claude binary; no tokens)")
	}
	runInterruptScenarios(t, true)
}

// interruptScenario is one recording: run drives the conversation and checks
// what the harness did.
type interruptScenario struct {
	name  string
	notes string
	run   func(t *testing.T, conv *Conversation)
}

func runInterruptScenarios(t *testing.T, record bool) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	version, _ := exec.Command(bin, "--version").Output()
	for _, sc := range []interruptScenario{
		{
			name:  "interrupt-mid-reply",
			notes: "Interrupt while the reply streams: \"⎿  Interrupted · What should Claude do instead?\" lands below the partial reply, the composer is empty, and the transcript holds the partial reply, then a user entry \"[Request interrupted by user]\".",
			run: func(t *testing.T, conv *Conversation) {
				sendOneTurnWithin(t, conv, "HW_STREAM tell me a story", time.Minute)
				awaitScreenWithin(t, conv, "word 6 ", 30*time.Second)
				expectInterrupt(t, conv, InterruptStopped)
				turn := waitForTerminalTurn(t, conv, 10*time.Second)
				if turn.State != TurnStateInterrupted || !strings.HasPrefix(turn.Text, "word 1 word 2 word 3 word 4 word 5 word 6") {
					t.Fatalf("turn = %+v, want interrupted with the partial reply", turn)
				}
			},
		},
		{
			name:  "interrupt-mid-tool",
			notes: "Interrupt while a Bash tool runs: the marker replaces the tool's \"⎿  Running…\" line, and the transcript holds the tool call, a rejected tool_result and \"[Request interrupted by user for tool use]\".",
			run: func(t *testing.T, conv *Conversation) {
				sendOneTurnWithin(t, conv, "HW_TOOL run a command", time.Minute)
				awaitScreenWithin(t, conv, "sleep 20", 30*time.Second)
				time.Sleep(time.Second)
				expectInterrupt(t, conv, InterruptStopped)
				if turn := waitForTerminalTurn(t, conv, 10*time.Second); turn.State != TurnStateInterrupted || turn.Text != "" {
					t.Fatalf("turn = %+v, want interrupted with no reply text", turn)
				}
			},
		},
		{
			name:  "interrupt-before-first-token",
			notes: "Interrupt before the first token: no marker is painted, the prompt's echo disappears and the prompt is put back in the composer; chat clears it (Ctrl-E, Ctrl-K…, Ctrl-U…) and the next prompt is answered alone. The transcript keeps only the cancelled prompt's user entry.",
			run: func(t *testing.T, conv *Conversation) {
				sendOneTurnWithin(t, conv, "HW_DELAY answer slowly", time.Minute)
				time.Sleep(1500 * time.Millisecond)
				expectInterrupt(t, conv, InterruptCancelled)
				if turn := waitForTerminalTurn(t, conv, 10*time.Second); turn.State != TurnStateInterrupted || turn.Text != "" {
					t.Fatalf("turn = %+v, want interrupted with no text", turn)
				}
				if text, ok := conv.Adapter().(turns.Interrupter).ComposerText(conv.ScreenSnapshot()); !ok || text != "" {
					t.Fatalf("composer = %q (readable %v), want it cleared", text, ok)
				}
				sendOneTurnWithin(t, conv, "HW_QUICK after the cancel", time.Minute)
				if turn := waitForTerminalTurn(t, conv, 30*time.Second); turn.State != TurnStateComplete || turn.Text != "QUICK_REPLY_OK" {
					t.Fatalf("next turn = %+v, want it answered", turn)
				}
			},
		},
		{
			name:  "interrupt-second-turn",
			notes: "Two turns, each interrupted mid-reply: the second marker lands below the second prompt's echo while the first turn's marker is still on screen above it.",
			run: func(t *testing.T, conv *Conversation) {
				for _, prompt := range []string{"HW_STREAM first story", "HW_STREAM second story"} {
					sendOneTurnWithin(t, conv, prompt, time.Minute)
					awaitScreenWithin(t, conv, "esc to interrupt", 30*time.Second)
					time.Sleep(time.Second)
					expectInterrupt(t, conv, InterruptStopped)
					if turn := waitForTerminalTurn(t, conv, 10*time.Second); turn.State != TurnStateInterrupted || !strings.HasPrefix(turn.Text, "word 1") {
						t.Fatalf("%q: turn = %+v, want interrupted with its partial reply", prompt, turn)
					}
				}
			},
		},
		{
			name:  "rewind-picker",
			notes: "Two Escs 150 ms apart on an idle, empty composer open claude's Rewind picker (\"Enter to continue · Esc to cancel\"), which paints \"❯ (current)\" where the composer was; a third Esc closes it. Interrupt writes one Esc per turn for this reason, and the composer reading refuses the picker.",
			run: func(t *testing.T, conv *Conversation) {
				sendOneTurnWithin(t, conv, "HW_QUICK say hi", time.Minute)
				if turn := waitForTerminalTurn(t, conv, 30*time.Second); turn.State != TurnStateComplete {
					t.Fatalf("turn = %+v, want complete", turn)
				}
				time.Sleep(2 * time.Second)
				esc := conv.Adapter().(turns.Interrupter).InterruptSequence()
				_ = conv.write(esc)
				time.Sleep(150 * time.Millisecond)
				_ = conv.write(esc)
				awaitScreenWithin(t, conv, "Esc to cancel", 5*time.Second)
				if text, ok := conv.Adapter().(turns.Interrupter).ComposerText(conv.ScreenSnapshot()); ok {
					t.Fatalf("the picker read as a composer holding %q", text)
				}
				time.Sleep(time.Second)
				_ = conv.write(esc)
				awaitScreenWithin(t, conv, "for agents", 5*time.Second)
			},
		},
	} {
		t.Run(sc.name, func(t *testing.T) {
			recordInterruptScenario(t, bin, strings.TrimSpace(string(version)), sc, record)
		})
	}
}

func expectInterrupt(t *testing.T, conv *Conversation, want InterruptResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if res, err := conv.Interrupt(ctx); err != nil || res != want {
		t.Fatalf("Interrupt = %q, %v; want %s\n%s", res, err, want, conv.ScreenSnapshot().Text)
	}
}

func awaitScreenWithin(t *testing.T, conv *Conversation, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !strings.Contains(conv.ScreenSnapshot().Text, want) {
		if time.Now().After(deadline) {
			t.Fatalf("the screen never showed %q:\n%s", want, conv.ScreenSnapshot().Text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func recordInterruptScenario(t *testing.T, bin, version string, sc interruptScenario, record bool) {
	api := httptest.NewServer(http.HandlerFunc(interruptAPI))
	defer api.Close()

	wd := filepath.Join("/tmp", "hw-rebake", sc.name)
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
	// Bypass mode, accepted in advance, runs the tool call without a
	// permission dialog.
	seed(".claude.json", map[string]any{
		"hasCompletedOnboarding":        true,
		"bypassPermissionsModeAccepted": true,
		"projects":                      map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
	})
	seed("settings.json", map[string]any{
		"permissions":                       map[string]any{"defaultMode": "bypassPermissions"},
		"skipDangerousModePermissionPrompt": true,
	})

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
			"trust_prompt":      {Kind: DispositionAnswer, OptionID: "proceed"},
			"bypass_acceptance": {Kind: DispositionAnswer, OptionID: "proceed"},
		}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = conv.Close(context.Background()) }()

	sc.run(t, conv)
	if !record {
		return
	}
	time.Sleep(time.Second) // let the last frame paint
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := conv.Quit(ctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	_, _ = conv.Wrapper().Wait()
	raw := conv.Wrapper().RecentOutput()
	if len(raw) >= 64*1024 {
		t.Fatalf("the session wrote %d bytes, past the recent-output ring; the recording would be truncated", len(raw))
	}
	conv.mu.Lock()
	id := conv.session.HarnessID()
	conv.mu.Unlock()

	out := filepath.Join("..", "..", "test", "corpus", "claude-code", sc.name)
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
		"notes": sc.notes + " Recorded with `HW_RECORD_INTERRUPT=1 go test ./pkg/chat -run RecordInterrupt` " +
			"(pkg/chat/interrupt_record_test.go), interrupting through chat.Conversation.Interrupt: the real claude " +
			"against a local Messages API, in " + wd + " with a fresh CLAUDE_CONFIG_DIR (onboarding done, the directory " +
			"trusted, permissions.defaultMode bypassPermissions, accepted) and a placeholder ANTHROPIC_AUTH_TOKEN, so no account or tokens are involved.",
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
}

// interruptAPI is the local Messages API the interrupt scenarios run against.
// It answers by the cue in the latest user message: HW_STREAM streams forty
// words 150 ms apart, HW_DELAY waits six seconds before its first token,
// HW_TOOL calls Bash with "sleep 20", HW_QUICK replies at once; a tool result
// is answered "TOOL_DONE".
func interruptAPI(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	stream := bytes.Contains(body, []byte(`"stream":true`))
	last, toolResult := lastUserText(body)
	switch {
	case !strings.HasPrefix(r.URL.Path, "/v1/messages"):
		w.WriteHeader(http.StatusNotFound)
	case strings.Contains(r.URL.Path, "count_tokens"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":10}`)
	case toolResult:
		writeLocalReply(w, stream, "TOOL_DONE")
	case strings.Contains(last, "HW_STREAM"):
		slowStream(r.Context(), w, 40, 150*time.Millisecond, 0)
	case strings.Contains(last, "HW_DELAY"):
		slowStream(r.Context(), w, 3, 100*time.Millisecond, 6*time.Second)
	case strings.Contains(last, "HW_TOOL"):
		toolCall(w, "sleep 20")
	case strings.Contains(last, "HW_QUICK"):
		writeLocalReply(w, stream, "QUICK_REPLY_OK")
	default:
		writeLocalReply(w, stream, "ok")
	}
}

// lastUserText returns the text of a Messages API request's last user message,
// and whether it carries a tool result. claude appends a system message after
// it, so the last message is not always the user's.
func lastUserText(body []byte) (string, bool) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", false
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			return text, false
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(m.Content, &blocks)
		var b strings.Builder
		tool := false
		for _, bl := range blocks {
			tool = tool || bl.Type == "tool_result"
			b.WriteString(bl.Text)
			b.WriteByte('\n')
		}
		return b.String(), tool
	}
	return "", false
}

// sseWriter writes server-sent events, flushing each.
func sseWriter(w http.ResponseWriter) func(name, data string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	return func(name, data string) {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if fl != nil {
			fl.Flush()
		}
	}
}

// slowStream streams n words "word 1 " … gap apart, after waiting first.
func slowStream(ctx context.Context, w http.ResponseWriter, n int, gap, first time.Duration) {
	select {
	case <-time.After(first):
	case <-ctx.Done():
		return
	}
	event := sseWriter(w)
	event("message_start", `{"type":"message_start","message":{"id":"msg_local","type":"message","role":"assistant","model":"claude-local","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for i := 1; i <= n; i++ {
		event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"word %d "}}`, i))
		select {
		case <-time.After(gap):
		case <-ctx.Done():
			return
		}
	}
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}`)
	event("message_stop", `{"type":"message_stop"}`)
}

// toolCall answers with one Bash tool call running command.
func toolCall(w http.ResponseWriter, command string) {
	input, _ := json.Marshal(map[string]string{"command": command, "description": "Wait"})
	partial, _ := json.Marshal(string(input))
	event := sseWriter(w)
	event("message_start", `{"type":"message_start","message":{"id":"msg_local","type":"message","role":"assistant","model":"claude-local","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_local1","name":"Bash","input":{}}}`)
	event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(partial)+`}}`)
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`)
	event("message_stop", `{"type":"message_stop"}`)
}
