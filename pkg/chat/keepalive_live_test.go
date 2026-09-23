package chat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// TestKeepAliveLive is the idle-kill against the real binary. A reply that
// names a rate limit, then the idle between messages: without keep-alive the
// wrapper ends claude as blocked_by_cost once its idle gate opens; with it, the
// same process answers the next message. The two modes run side by side. It
// spends three one-line turns and about a minute:
//
//	HW_LIVE_IDLE=1 go test ./pkg/chat -run KeepAliveLive -v
//
// Two facts about claude 2.1.280 shape it, both recorded from its own output:
//
//   - It places the words of a freshly painted line with cursor moves, not
//     blanks, so a phrase with a space in it never reaches the classifier
//     whole. The reply asks for "rate-limit", hyphenated.
//   - It is not silent at its composer for a full minute. It paints a usage
//     notice a few seconds after a turn ("You've used 92% of your weekly
//     limit · resets 6pm") and clears it, and after ~59s it rings the bell,
//     which resets a 60s idle gate just before it opens. So the classify
//     window here is 20s, which the quiet stretch between those reaches.
func TestKeepAliveLive(t *testing.T) {
	if os.Getenv("HW_LIVE_IDLE") != "1" {
		t.Skip("live idle-kill: set HW_LIVE_IDLE=1 (needs the real claude binary; spends three short turns and ~1 min)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const classify = 20 * time.Second
	// Past the usage notice's clearing plus the classify window, and short of
	// the bell.
	const idle = 45 * time.Second

	open := func(t *testing.T, keepAlive bool) *Conversation {
		t.Helper()
		conv, err := Open(context.Background(), Options{
			Harness:                   chatClaudeCode,
			BinaryPath:                bin,
			WorkingDir:                wd,
			Env:                       harnessenv.Cleaned(),
			Store:                     newFakeStore(),
			KeepAliveOnClassification: keepAlive,
			wrapperQuiet:              5 * time.Second,
			wrapperClassify:           classify,
			InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
				"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
			}},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = conv.Close(context.Background()) })
		sendOneTurnWithin(t, conv, "Reply with exactly: the rate-limit test passed", 2*time.Minute)
		if turn := waitForTerminalTurn(t, conv, 2*time.Minute); turn.State != TurnStateComplete {
			t.Fatalf("first turn = %+v, want complete", turn)
		}
		return conv
	}

	t.Run("default mode ends it", func(t *testing.T) {
		t.Parallel()
		conv := open(t, false)
		res, ended := wrapperExited(conv, idle)
		if !ended || res.Status != wrapper.StatusBlockedByCost {
			t.Fatalf("after %s idle: Result = %+v (ended %v), want blocked_by_cost", idle, res, ended)
		}
	})
	t.Run("keep-alive answers after the idle", func(t *testing.T) {
		t.Parallel()
		conv := open(t, true)
		pid := conv.Wrapper().PID()
		if res, ended := wrapperExited(conv, idle); ended {
			t.Fatalf("the harness ended while idle: %+v", res)
		}
		sendOneTurnWithin(t, conv, "Reply with exactly: still here", 2*time.Minute)
		turn := waitForTerminalTurn(t, conv, 2*time.Minute)
		if turn.State != TurnStateComplete || !strings.Contains(turn.Text, "still here") {
			t.Fatalf("second turn = %+v, want the answer", turn)
		}
		if got := conv.Wrapper().PID(); got != pid {
			t.Fatalf("pid %d answered, want the original %d", got, pid)
		}
	})
}

// sendOneTurnWithin is sendOneTurn with a caller-chosen bound, for a real
// harness whose startup and reply take longer than the fake's.
func sendOneTurnWithin(t *testing.T, conv *Conversation, text string, limit time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()
	if _, err := conv.Send(ctx, text); err != nil {
		t.Fatalf("Send(%q): %v", text, err)
	}
}
