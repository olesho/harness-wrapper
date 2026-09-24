package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harnessenv"
)

// TestStreamLive runs the stream-json transport (ADR-009) against the real
// claude binary, pointed at a local Messages API — no account, no tokens:
//
//	HW_LIVE_STREAM=1 go test ./pkg/chat -run StreamLive -v
//
// The API's cues are interruptAPI's (HW_QUICK, HW_STREAM, HW_DELAY, HW_TOOL)
// plus HW_ERR529 (529 on every attempt) and HW_BAD (a 400, not retried).
func TestStreamLive(t *testing.T) {
	if os.Getenv("HW_LIVE_STREAM") != "1" {
		t.Skip("runs the real claude over stream-json against a local API: set HW_LIVE_STREAM=1 (no tokens)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	api := httptest.NewServer(http.HandlerFunc(streamLiveAPI))
	defer api.Close()

	t.Run("turns-and-transcript-history", func(t *testing.T) {
		lv := openStreamLive(t, bin, api.URL, newFakeStore(), "")
		for i := 0; i < 2; i++ {
			if turn := lv.turn("HW_QUICK hello"); turn.State != TurnStateComplete || turn.Text != "QUICK_REPLY_OK" {
				t.Fatalf("turn %d = %+v, want QUICK_REPLY_OK", i, turn)
			}
		}
		hist, src, err := lv.conv.HistoryWithSource(context.Background())
		if err != nil || src != HistorySourceTranscript || len(hist) < 4 {
			t.Fatalf("History = %d turns from %s, %v; want the transcript's 4", len(hist), src, err)
		}
	})
	t.Run("interrupt-mid-reply", func(t *testing.T) {
		lv := openStreamLive(t, bin, api.URL, newFakeStore(), "")
		id := lv.sendOnly("HW_STREAM a story")
		time.Sleep(2 * time.Second)
		lv.interrupt(InterruptStopped)
		if turn := lv.rig.ended(id); turn.State != TurnStateInterrupted || !strings.HasPrefix(turn.Text, "word 1 word 2") {
			t.Fatalf("turn = %+v, want interrupted with the partial reply", turn)
		}
		if turn := lv.turn("HW_QUICK after"); turn.Text != "QUICK_REPLY_OK" {
			t.Fatalf("next turn = %+v", turn)
		}
	})
	t.Run("interrupt-before-first-token", func(t *testing.T) {
		lv := openStreamLive(t, bin, api.URL, newFakeStore(), "")
		id := lv.sendOnly("HW_DELAY slowly")
		time.Sleep(1500 * time.Millisecond)
		lv.interrupt(InterruptCancelled)
		if turn := lv.rig.ended(id); turn.State != TurnStateInterrupted || turn.Text != "" {
			t.Fatalf("turn = %+v, want interrupted with no text", turn)
		}
		if turn := lv.turn("HW_QUICK after"); turn.Text != "QUICK_REPLY_OK" {
			t.Fatalf("next turn = %+v", turn)
		}
	})
	t.Run("interrupt-mid-tool", func(t *testing.T) {
		lv := openStreamLive(t, bin, api.URL, newFakeStore(), "")
		id := lv.sendOnly("HW_TOOL run it")
		time.Sleep(4 * time.Second)
		lv.interrupt(InterruptStopped)
		if turn := lv.rig.ended(id); turn.State != TurnStateInterrupted {
			t.Fatalf("turn = %+v, want interrupted", turn)
		}
	})
	t.Run("api-error-exhausted", func(t *testing.T) {
		lv := openStreamLive(t, bin, api.URL, newFakeStore(), "", "CLAUDE_CODE_MAX_RETRIES=2")
		turn := lv.turn("HW_ERR529 please")
		if turn.State != TurnStateErrored || turn.HTTPCode != 529 || !strings.Contains(turn.Reason, "harness tag: server_error") {
			t.Fatalf("turn = %+v, want errored 529 tagged server_error", turn)
		}
		if turn := lv.turn("HW_QUICK after"); turn.Text != "QUICK_REPLY_OK" {
			t.Fatalf("next turn = %+v", turn)
		}
	})
	t.Run("api-error-not-retried", func(t *testing.T) {
		lv := openStreamLive(t, bin, api.URL, newFakeStore(), "")
		if turn := lv.turn("HW_BAD request"); turn.State != TurnStateErrored || turn.HTTPCode != 400 {
			t.Fatalf("turn = %+v, want errored 400", turn)
		}
	})
	t.Run("quit-then-reopen", func(t *testing.T) {
		store := newFakeStore()
		lv := openStreamLive(t, bin, api.URL, store, "")
		lv.turn("HW_QUICK first")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := lv.conv.Quit(ctx); err != nil {
			t.Fatal(err)
		}
		<-lv.conv.Done()
		if st := lv.conv.State(); st.Exit == nil || st.Exit.ExitCode != 0 {
			t.Fatalf("exit = %+v, want 0", st.Exit)
		}
		re := openStreamLive(t, bin, api.URL, store, lv.conv.SessionID())
		if turn := re.turn("HW_QUICK second"); turn.Text != "QUICK_REPLY_OK" {
			t.Fatalf("turn after reopen = %+v", turn)
		}
		hist, _, _ := re.conv.HistoryWithSource(ctx)
		if len(hist) < 4 {
			t.Fatalf("History after reopen = %d turns, want both sessions' 4", len(hist))
		}
	})
	t.Run("killed-from-outside", func(t *testing.T) {
		lv := openStreamLive(t, bin, api.URL, newFakeStore(), "")
		id := lv.sendOnly("HW_STREAM a story")
		time.Sleep(time.Second)
		_ = syscall.Kill(lv.conv.State().PID, syscall.SIGKILL)
		if turn := lv.rig.ended(id); turn.State != TurnStateErrored || !strings.Contains(turn.Reason, "harness exited") {
			t.Fatalf("turn = %+v, want errored: harness exited", turn)
		}
		ev := lv.rig.await("exited", func(ev ConversationEvent) bool { return ev.Type == EventExited })
		if ev.Exit.Signal == "" {
			t.Fatalf("exit = %+v, want the signal named", ev.Exit)
		}
	})
}

type streamLive struct {
	rig  *streamRig
	conv *Conversation
}

// openStreamLive opens (or, with reopen set, reopens) the real claude over
// stream-json in a fresh config dir, against api.
func openStreamLive(t *testing.T, bin, api string, store Store, reopen string, env ...string) *streamLive {
	t.Helper()
	var cfg string
	wd := t.TempDir()
	if reopen != "" {
		rec, _ := store.GetSession(context.Background(), reopen)
		wd = rec.WorkingDir
		cfg = filepath.Join(wd, "..", "cfg")
	} else {
		cfg = filepath.Join(wd, "..", "cfg")
		_ = os.MkdirAll(cfg, 0o700)
	}
	realWD, _ := filepath.EvalSymlinks(wd)
	seed := func(file string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(cfg, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if reopen == "" {
		seed(".claude.json", map[string]any{
			"hasCompletedOnboarding": true, "bypassPermissionsModeAccepted": true,
			"projects": map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
		})
		seed("settings.json", map[string]any{"skipDangerousModePermissionPrompt": true})
	}
	fullEnv := append(append(harnessenv.Cleaned(),
		"CLAUDE_CONFIG_DIR="+cfg,
		"ANTHROPIC_BASE_URL="+api,
		"ANTHROPIC_AUTH_TOKEN=placeholder-for-a-local-api"), env...)

	r := &streamRig{t: t, news: make(chan struct{}, 1)}
	onEvent := func(ev ConversationEvent) {
		r.mu.Lock()
		r.evs = append(r.evs, ev)
		r.mu.Unlock()
		select {
		case r.news <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var conv *Conversation
	var err error
	if reopen != "" {
		conv, err = Reopen(ctx, ReopenOptions{
			SessionID: reopen, Transport: TransportStreamJSON, BinaryPath: bin, Env: fullEnv,
			PermissionMode: "bypass", Store: store, OnEvent: onEvent,
		})
	} else {
		conv, err = Open(ctx, Options{
			Harness: chatClaudeCode, BinaryPath: bin, WorkingDir: wd, Env: fullEnv,
			Transport: TransportStreamJSON, PermissionMode: "bypass", Store: store, OnEvent: onEvent,
		})
	}
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	r.conv = conv
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = conv.Close(ctx)
	})
	return &streamLive{rig: r, conv: conv}
}

func (lv *streamLive) sendOnly(text string) string { return lv.rig.send(text) }

func (lv *streamLive) turn(text string) Turn {
	lv.rig.t.Helper()
	id := lv.rig.send(text)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		for _, ev := range lv.rig.events() {
			if ev.Type == EventTurn && ev.Turn.ID == id && ev.Turn.State != TurnStatePending {
				return ev.Turn
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	lv.rig.t.Fatalf("turn %q did not end", text)
	return Turn{}
}

func (lv *streamLive) interrupt(want InterruptResult) {
	lv.rig.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if res, err := lv.conv.Interrupt(ctx); err != nil || res != want {
		lv.rig.t.Fatalf("Interrupt = %q, %v; want %s", res, err, want)
	}
}

// streamLiveAPI answers interruptAPI's cues plus two error cues, keyed on the
// LAST cue in the last user message. After a cancelled or failed turn claude
// keeps that prompt in the history and sends it together with the next one,
// so the earlier cue is in the same message and must not win.
func streamLiveAPI(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	stream := bytes.Contains(body, []byte(`"stream":true`))
	text, toolResult := lastUserText(body)
	cue := ""
	if i := strings.LastIndex(text, "HW_"); i >= 0 {
		cue = text[i:]
		if j := strings.IndexAny(cue, " \n"); j > 0 {
			cue = cue[:j]
		}
	}
	switch {
	case !strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.Contains(r.URL.Path, "count_tokens"):
		interruptAPI(w, r)
	case toolResult:
		writeLocalReply(w, stream, "TOOL_DONE")
	case cue == "HW_ERR529":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	case cue == "HW_BAD":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"local bad request"}}`)
	case cue == "HW_STREAM":
		slowStream(r.Context(), w, 40, 150*time.Millisecond, 0)
	case cue == "HW_DELAY":
		slowStream(r.Context(), w, 3, 100*time.Millisecond, 6*time.Second)
	case cue == "HW_TOOL":
		toolCall(w, "sleep 20")
	case cue == "HW_QUICK":
		writeLocalReply(w, stream, "QUICK_REPLY_OK")
	default:
		writeLocalReply(w, stream, "ok")
	}
}
