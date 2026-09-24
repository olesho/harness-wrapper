package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// replayedTurn is how a replayed turn ended, and what its Interrupt got.
type replayedTurn struct {
	Turn
	interrupt InterruptResult
}

// replayStreamCapture feeds a recorded claude session's frames through the
// stream-json driver's frame handling, turn by turn, and returns how each
// turn ended. A message the capture sent while a turn ran waits for it, as
// the Conversation never has two in flight.
func replayStreamCapture(t *testing.T, name string) (turns []replayedTurn, prompts int) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "test", "corpus", "claude-code-stream", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	c := &Conversation{
		opts:    Options{Harness: chatClaudeCode},
		store:   newFakeStore(),
		eventCh: make(chan ConversationEvent, 4096),
		done:    make(chan struct{}),
		closed:  make(chan struct{}),
		stream:  &streamProc{stdin: nopWriteCloser{}, control: map[string]chan streamControlResult{}},
	}
	type queued struct{ uuid string }
	var waiting []queued
	ops := map[string]*streamInterruptOp{}
	start := func(uuid string) {
		turn := Turn{ID: newID(), Role: RoleAssistant, State: TurnStatePending}
		_ = c.store.AppendTurn(context.Background(), &turn)
		c.currentTurn = &turn
		c.stream.turn = &streamTurn{uuid: uuid, receipt: make(chan struct{})}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, streamFrameMax)
	for sc.Scan() {
		var rec struct {
			Dir   string          `json:"dir"`
			Frame json.RawMessage `json:"frame"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Frame == nil {
			continue
		}
		var fr streamFrame
		if err := json.Unmarshal(rec.Frame, &fr); err != nil {
			t.Fatalf("%s: a frame does not decode: %v", name, err)
		}
		switch rec.Dir {
		case "in":
			switch {
			case fr.Type == "user":
				var u struct {
					UUID string `json:"uuid"`
				}
				_ = json.Unmarshal(rec.Frame, &u)
				if c.currentTurn == nil {
					start(u.UUID)
				} else {
					waiting = append(waiting, queued{u.UUID})
				}
			case fr.Type == "control_request" && strings.Contains(string(fr.Request), `"interrupt"`):
				if st := c.stream.turn; st != nil {
					op := &streamInterruptOp{done: make(chan struct{})}
					st.interrupt = op
					ops[c.currentTurn.ID] = op
				}
			}
		case "out":
			if fr.Type == "control_request" {
				prompts++
			}
			c.onStreamFrame(&fr, rec.Frame)
			if c.currentTurn == nil && len(waiting) > 0 {
				start(waiting[0].uuid)
				waiting = waiting[1:]
			}
		}
	}
	close(c.eventCh)
	for ev := range c.eventCh {
		if ev.Type == EventTurn && ev.Turn.State != TurnStatePending {
			rt := replayedTurn{Turn: ev.Turn}
			if op := ops[ev.Turn.ID]; op != nil {
				<-op.done
				rt.interrupt = op.result
			}
			turns = append(turns, rt)
		}
	}
	return turns, prompts
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

var _ io.WriteCloser = nopWriteCloser{}

// TestStreamCorpus replays claude 2.1.281's recorded stream-json sessions
// (test/corpus/claude-code-stream) and checks each turn's ending.
func TestStreamCorpus(t *testing.T) {
	type want struct {
		state     TurnState
		text      string // prefix
		interrupt InterruptResult
		reason    string // substring
		code      int
	}
	done := func(text string) want { return want{state: TurnStateComplete, text: text} }
	for _, tc := range []struct {
		name    string
		turns   []want
		prompts int
	}{
		{"01-multi-message", []want{done("PONG 0"), done("PONG 1"), done("PONG 2")}, 0},
		{"01d-command-lifecycle", []want{done("slow0 slow1 slow2 slow3 slow4 slow5"), done("PONG queued")}, 0},
		{"02a-interrupt-before-first-token", []want{{state: TurnStateInterrupted, interrupt: InterruptCancelled}, done("PONG after-interrupt")}, 0},
		{"02b-interrupt-mid-text", []want{{state: TurnStateInterrupted, interrupt: InterruptStopped, text: "slow"}, done("PONG after-interrupt")}, 0},
		{"02c-interrupt-mid-tool", []want{{state: TurnStateInterrupted, interrupt: InterruptStopped}, done("PONG after-interrupt")}, 0},
		{"02d-interrupt-no-turn", []want{done("PONG idle"), done("PONG after")}, 0},
		{"02e-interrupt-with-queued-cancel", []want{{state: TurnStateInterrupted, interrupt: InterruptStopped}}, 0},
		{"04b-host-prompt", []want{done("TOOL DONE"), done("TOOL DONE: probe denies")}, 2},
		{"05b-resume", []want{done("PONG second-life")}, 0},
		{"08a-overloaded-529-x2", []want{done("RECOVERED"), done("PONG after-error")}, 0},
		{"08c-overloaded-529-x20", []want{{state: TurnStateErrored, reason: "harness tag: server_error", code: 529}, done("PONG after-error")}, 0},
		{"08d-api-500-x2", []want{done("RECOVERED"), done("PONG after-error")}, 0},
		{"13-session-state", []want{done("PONG state"), done("TOOL DONE"), {state: TurnStateInterrupted}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, prompts := replayStreamCapture(t, tc.name)
			if len(got) != len(tc.turns) {
				t.Fatalf("%d turns ended, want %d: %+v", len(got), len(tc.turns), got)
			}
			for i, w := range tc.turns {
				g := got[i]
				if g.State != w.state || !strings.HasPrefix(g.Text, w.text) ||
					(w.interrupt != "" && g.interrupt != w.interrupt) ||
					!strings.Contains(g.Reason, w.reason) || g.HTTPCode != w.code {
					t.Errorf("turn %d = %s %q (interrupt %q, reason %q, http %d); want %+v",
						i, g.State, g.Text, g.interrupt, g.Reason, g.HTTPCode, w)
				}
			}
			if prompts != tc.prompts {
				t.Errorf("%d permission prompts, want %d", prompts, tc.prompts)
			}
		})
	}
}
