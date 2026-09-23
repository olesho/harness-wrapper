package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Keep-alive conversations (Options.KeepAliveOnClassification, ADR-006) over
// the real fake harness, with the wrapper's idle thresholds shrunk so its idle
// classification is reached in a fraction of a second.

const (
	testWrapperQuiet    = 50 * time.Millisecond
	testWrapperClassify = 200 * time.Millisecond
)

func withWrapperThresholds(keepAlive bool) func(*Options) {
	return func(o *Options) {
		o.KeepAliveOnClassification = keepAlive
		o.wrapperQuiet = testWrapperQuiet
		o.wrapperClassify = testWrapperClassify
	}
}

// phraseThenAnswer replies with prose that merely mentions a rate limit, then
// answers a second message.
func phraseThenAnswer() fakeharness.Script {
	return fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "I added rate limiting to the login endpoint.", "Baked", "1s").
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "second: "+fakeharness.PromptRef(), "Brewed", "2s").
		StayAliveUntilStopped().
		Build()
}

// wrapperExited reports whether the harness process has ended, and how.
func wrapperExited(conv *Conversation, within time.Duration) (wrapper.Result, bool) {
	done := make(chan wrapper.Result, 1)
	go func() {
		res, _ := conv.Wrapper().Wait()
		done <- res
	}()
	select {
	case res := <-done:
		return res, true
	case <-time.After(within):
		return wrapper.Result{}, false
	}
}

// TestKeepAlive_ConversationSurvivesAPhraseAndIdle: the reply mentions a rate
// limit and the conversation then idles past IdleClassify — the resting state
// between messages. It must still answer the next Send.
func TestKeepAlive_ConversationSurvivesAPhraseAndIdle(t *testing.T) {
	conv := openFake(t, phraseThenAnswer(), withWrapperThresholds(true))
	sendOneTurn(t, conv, "add rate limiting")
	if first := waitForTerminalTurn(t, conv, 10*time.Second); first.State != TurnStateComplete {
		t.Fatalf("first turn = %+v, want complete", first)
	}
	if res, ended := wrapperExited(conv, 3*testWrapperClassify); ended {
		t.Fatalf("the harness ended while idle: %+v", res)
	}
	sendOneTurn(t, conv, "and now?")
	second := waitForTerminalTurn(t, conv, 10*time.Second)
	if second.State != TurnStateComplete || !strings.Contains(second.Text, "second: and now?") {
		t.Fatalf("second turn = %+v, want the harness's answer", second)
	}
}

// TestKeepAlive_DefaultConversationIsEndedByAPhrase pins what the option
// changes: without it the same conversation is ended as blocked_by_cost once
// the reply has sat quiet for IdleClassify.
func TestKeepAlive_DefaultConversationIsEndedByAPhrase(t *testing.T) {
	conv := openFake(t, phraseThenAnswer(), withWrapperThresholds(false))
	sendOneTurn(t, conv, "add rate limiting")
	waitForTerminalTurn(t, conv, 10*time.Second)
	res, ended := wrapperExited(conv, 5*time.Second)
	if !ended || res.Status != wrapper.StatusBlockedByCost {
		t.Fatalf("Result = %+v (ended %v), want blocked_by_cost", res, ended)
	}
}

// TestReopen_CarriesKeepAlive: a reopened conversation is kept alive when its
// ReopenOptions ask for it.
func TestReopen_CarriesKeepAlive(t *testing.T) {
	first := openFake(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build())
	store := first.store
	id := first.SessionID()
	deadline := time.Now().Add(5 * time.Second)
	for harnessIDOfStored(t, store, id) == "" {
		if time.Now().After(deadline) {
			t.Fatal("the first launch never recorded a harness session id")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	env := fakeEnv(t, phraseThenAnswer())
	conv, err := Reopen(context.Background(), ReopenOptions{
		SessionID:                 id,
		BinaryPath:                buildFakeHarness(t),
		Env:                       env,
		Store:                     store,
		KeepAliveOnClassification: true,
		idleGap:                   testIdleGap,
		markerGap:                 testMarkerGap,
		wrapperQuiet:              testWrapperQuiet,
		wrapperClassify:           testWrapperClassify,
	})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	t.Cleanup(func() { _ = conv.Close(context.Background()) })
	sendOneTurn(t, conv, "add rate limiting")
	if turn := waitForTerminalTurn(t, conv, 10*time.Second); turn.State != TurnStateComplete {
		t.Fatalf("turn = %+v, want complete", turn)
	}
	if res, ended := wrapperExited(conv, 3*testWrapperClassify); ended {
		t.Fatalf("the reopened harness ended while idle: %+v", res)
	}
}

func harnessIDOfStored(t *testing.T, store Store, id string) string {
	t.Helper()
	rec, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return rec.HarnessID()
}

// fakeEnv writes script for the fake and returns the launch environment.
func fakeEnv(t *testing.T, script fakeharness.Script) []string {
	t.Helper()
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), fakeharness.EnvVar+"="+path)
}

// TestResumeAt_FromTheWall: a usage-limited turn carries the reset time the
// wall names, from the screen relabel and from the harness's rate_limit tag,
// and none when the wall names none.
func TestResumeAt_FromTheWall(t *testing.T) {
	before := time.Now()
	t.Run("screen relabel", func(t *testing.T) {
		c := &Conversation{opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New()}
		const wall = "You've hit your session limit · resets 10:20pm (Europe/Warsaw)"
		turn := &Turn{State: TurnStateComplete, Text: wall}
		if !c.usageLimitRelabel(turn, screen.Snapshot{Text: "⏺ " + wall + "\n\n✻ Brewed for 0s\n\n❯ \n"}) {
			t.Fatal("usageLimitRelabel declined")
		}
		warsaw, err := time.LoadLocation("Europe/Warsaw")
		if err != nil {
			t.Fatal(err)
		}
		if at := turn.ResumeAt.In(warsaw); !turn.ResumeAt.After(before) || at.Hour() != 22 || at.Minute() != 20 {
			t.Fatalf("ResumeAt = %v, want the next 22:20 in Europe/Warsaw", turn.ResumeAt)
		}
	})
	t.Run("rate_limit tag", func(t *testing.T) {
		c := convWithTranscript(t, "sess-resume", userLine("go"),
			errorLine("rate_limit", "You've hit your session limit · resets 6:40pm (UTC)"))
		turn := &Turn{State: TurnStateComplete}
		if !c.apiErrorRelabel(turn) || turn.Code != CodeUsageLimited {
			t.Fatalf("turn = %+v, want usage_limited", turn)
		}
		if at := turn.ResumeAt.UTC(); !turn.ResumeAt.After(before) || at.Hour() != 18 || at.Minute() != 40 {
			t.Fatalf("ResumeAt = %v, want the next 18:40 UTC", turn.ResumeAt)
		}
	})
	t.Run("a wall naming no reset time", func(t *testing.T) {
		c := convWithTranscript(t, "sess-no-resume", userLine("go"), errorLine("rate_limit", "API Error: 429"))
		turn := &Turn{State: TurnStateComplete}
		if !c.apiErrorRelabel(turn) || !turn.ResumeAt.IsZero() {
			t.Fatalf("turn = %+v, want usage_limited with no ResumeAt", turn)
		}
	})
	t.Run("not a usage wall", func(t *testing.T) {
		c := convWithTranscript(t, "sess-server", userLine("go"),
			errorLine("server_error", "API Error: 529 Overloaded · resets 6:40pm (UTC)"))
		turn := &Turn{State: TurnStateComplete}
		if !c.apiErrorRelabel(turn) || !turn.ResumeAt.IsZero() {
			t.Fatalf("turn = %+v, want errored with no ResumeAt", turn)
		}
	})
}
