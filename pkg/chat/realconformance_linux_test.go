//go:build linux

package chat_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// TestRealClaudeConversations is the stored-conversation half of the
// authenticated conformance runs that activate the claude-code profile (G5 in
// the containment plan's Validation). Three real claude conversations share one
// working directory: one contained in private state, one contained as the login
// in a caller StateDir, and one uncontained. Each must keep its own harness
// session, transcript and history, and a Reopen must resume each from its own
// state — the private one from the state its record names.
//
// It needs HW_REAL_CLAUDE (the pinned binary), HW_REAL_CLAUDE_STATE_DIR (a
// StateDir signed in with contain-login), HW_REAL_CLAUDE_OAUTH_TOKEN (a token
// for the other two, as `claude setup-token` prints), cgroup supervision, and
// the account's quota.
func TestRealClaudeConversations(t *testing.T) {
	bin, stateDir, token := os.Getenv("HW_REAL_CLAUDE"), os.Getenv("HW_REAL_CLAUDE_STATE_DIR"), os.Getenv("HW_REAL_CLAUDE_OAUTH_TOKEN")
	if bin == "" || stateDir == "" || token == "" {
		t.Skip("set HW_REAL_CLAUDE, HW_REAL_CLAUDE_STATE_DIR and HW_REAL_CLAUDE_OAUTH_TOKEN to run real claude conversations")
	}
	if _, err := landlock.Probe(landlock.MinimumABI); err != nil {
		t.Fatalf("Landlock unavailable: %v", err)
	}
	t.Cleanup(contain.ActivateProfilesForTest())
	// A short state root: claude refuses a private TMPDIR over 79 bytes.
	state, err := os.MkdirTemp("", "hw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	t.Setenv("XDG_STATE_HOME", state)

	wd := t.TempDir()
	// The uncontained conversation's own config root, onboarded and trusting wd.
	config := filepath.Join(t.TempDir(), ".claude")
	seed, _ := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               map[string]any{wd: map[string]any{"hasTrustDialogAccepted": true}},
	})
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, ".claude.json"), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8", "HOME=" + os.Getenv("HOME")}
	tcp := []uint16{443}
	store := memstore.New()
	convs := []*realConv{
		{
			name: "private", word: realNonce("heron"), env: append(base, "CLAUDE_CODE_OAUTH_TOKEN="+token),
			req: &wrapper.Containment{Kind: wrapper.ContainmentLandlock, RestrictTCP: true, ConnectTCP: tcp},
		},
		{
			name: "caller", word: realNonce("ibis"), env: base,
			req: &wrapper.Containment{Kind: wrapper.ContainmentLandlock, RestrictTCP: true, ConnectTCP: tcp, StateDir: stateDir},
		},
		{
			name: "uncontained", word: realNonce("egret"),
			env: append(base, "CLAUDE_CODE_OAUTH_TOKEN="+token, "CLAUDE_CONFIG_DIR="+config),
		},
	}

	// All three open at once, in the same working directory.
	for _, c := range convs {
		conv, err := chat.Open(context.Background(), chat.Options{
			Harness: "claude-code", BinaryPath: bin, WorkingDir: wd, Env: c.env, Model: "sonnet",
			Store: store, Containment: c.req, OnInputRequest: acceptTrust,
		})
		if err != nil {
			t.Fatalf("%s: Open: %v", c.name, err)
		}
		c.conv, c.id = conv, conv.SessionID()
		t.Cleanup(func() { _ = c.conv.Close(context.Background()) })
	}
	for _, c := range convs {
		c.reply(t, "This is an automated test. Reply with only the word "+c.word+".", c.word)
	}
	// claude names its session as it exits; the line tap records it.
	for _, c := range convs {
		c.quit(t)
		c.harness = c.harnessID(t, store)
		if a := c.conv.Containment(); a != nil {
			t.Logf("%s: harness session %s, state %s (%s), supervision %s", c.name, c.harness, a.State.HarnessState, a.State.Mode, a.Supervision.Mode)
		} else {
			t.Logf("%s: harness session %s, uncontained", c.name, c.harness)
		}
	}
	seen := map[string]string{}
	for _, c := range convs {
		if other, dup := seen[c.harness]; dup {
			t.Fatalf("%s and %s share harness session %s", c.name, other, c.harness)
		}
		seen[c.harness] = c.name
		c.checkHistory(t, convs, 1)
		if err := c.conv.Close(context.Background()); err != nil {
			t.Fatalf("%s: Close: %v", c.name, err)
		}
	}

	// Each resumes from its own state, and remembers only its own word.
	for _, c := range convs {
		conv, err := chat.Reopen(context.Background(), chat.ReopenOptions{
			SessionID: c.id, BinaryPath: bin, Env: c.env, Model: "sonnet", Store: store, OnInputRequest: acceptTrust,
		})
		if err != nil {
			t.Fatalf("%s: Reopen: %v", c.name, err)
		}
		c.conv = conv
		c.reply(t, "This is an automated test. Reply with only the word you replied with before.", c.word)
		c.quit(t)
		if id := c.harnessID(t, store); id != c.harness {
			t.Errorf("%s: resumed as harness session %s, want %s", c.name, id, c.harness)
		}
		c.checkHistory(t, convs, 2)
		if err := c.conv.Close(context.Background()); err != nil {
			t.Fatalf("%s: Close: %v", c.name, err)
		}
	}
}

// realConv is one of TestRealClaudeConversations' conversations.
type realConv struct {
	name, word string
	env        []string
	req        *wrapper.Containment
	conv       *chat.Conversation
	id         string
	harness    string
}

// reply sends text and waits for a completed reply containing want.
func (c *realConv) reply(t *testing.T, text, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	release, err := c.conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("%s: AcquireControl: %v", c.name, err)
	}
	defer release()
	if _, err := c.conv.Send(ctx, text); err != nil {
		t.Fatalf("%s: Send: %v", c.name, err)
	}
	for {
		select {
		case ev := <-c.conv.Events():
			if ev.Type != chat.EventTurn || ev.Turn.Role != chat.RoleAssistant {
				continue
			}
			switch ev.Turn.State {
			case chat.TurnStateComplete:
				if !strings.Contains(ev.Turn.Text, want) {
					t.Fatalf("%s: reply %q, want %s", c.name, ev.Turn.Text, want)
				}
				return
			case chat.TurnStateErrored:
				t.Fatalf("%s: turn errored: %s (%s)", c.name, ev.Turn.Reason, ev.Turn.Text)
			}
		case <-ctx.Done():
			t.Fatalf("%s: no reply to %q", c.name, text)
		}
	}
}

// quit has claude exit on its own, printing the session id the line tap
// records, and waits for the process to end.
func (c *realConv) quit(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.conv.Quit(ctx); err != nil {
		t.Fatalf("%s: Quit: %v", c.name, err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = c.conv.Wrapper().Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("%s: claude did not exit after quitting", c.name)
	}
}

// harnessID waits for the conversation's stored record to name its harness
// session.
func (c *realConv) harnessID(t *testing.T, store chat.Store) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		rec, err := store.GetSession(context.Background(), c.id)
		if err != nil {
			t.Fatalf("%s: GetSession: %v", c.name, err)
		}
		if id := rec.HarnessID(); id != "" {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no harness session id discovered", c.name)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// checkHistory reads the conversation's history from its harness transcript:
// its own word, in each of its replies so far, and none of the others' words.
func (c *realConv) checkHistory(t *testing.T, all []*realConv, replies int) {
	t.Helper()
	turns, src, err := c.conv.HistoryWithSource(context.Background())
	if err != nil {
		t.Fatalf("%s: History: %v", c.name, err)
	}
	if src != chat.HistorySourceTranscript {
		t.Fatalf("%s: history came from %s, want the harness transcript", c.name, src)
	}
	var text strings.Builder
	got := 0
	for _, tr := range turns {
		text.WriteString(tr.Text + "\n")
		if tr.Role == chat.RoleAssistant && strings.Contains(tr.Text, c.word) {
			got++
		}
	}
	if got < replies {
		t.Errorf("%s: %d replies with %s in its history, want %d:\n%s", c.name, got, c.word, replies, text.String())
	}
	for _, o := range all {
		if o != c && strings.Contains(text.String(), o.word) {
			t.Errorf("%s: its history holds %s's word %s", c.name, o.name, o.word)
		}
	}
}

// acceptTrust answers claude's folder-trust dialog for a working directory a
// StateDir has not seen before.
func acceptTrust(req chat.InputRequest) (chat.InputAnswer, bool) {
	if req.Kind != "trust_prompt" {
		return chat.InputAnswer{}, false
	}
	for _, o := range req.Options {
		if strings.HasPrefix(strings.ToLower(o.Label), "yes") {
			return chat.InputAnswer{OptionID: o.ID}, true
		}
	}
	return chat.InputAnswer{}, false
}

func realNonce(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
}
