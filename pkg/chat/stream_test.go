package chat

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// streamRig is a stream-json Conversation over the fake claude, with every
// event it delivers.
type streamRig struct {
	t    *testing.T
	conv *Conversation
	mu   sync.Mutex
	evs  []ConversationEvent
	news chan struct{}
}

func openStreamRig(t *testing.T, mutate func(*Options)) *streamRig {
	t.Helper()
	r := &streamRig{t: t, news: make(chan struct{}, 1)}
	opts := fakeStreamOptions()
	opts.Store = newFakeStore()
	opts.WorkingDir = t.TempDir()
	opts.OnEvent = func(ev ConversationEvent) {
		r.mu.Lock()
		r.evs = append(r.evs, ev)
		r.mu.Unlock()
		select {
		case r.news <- struct{}{}:
		default:
		}
	}
	if mutate != nil {
		mutate(&opts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conv, err := Open(ctx, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.conv = conv
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = conv.Close(ctx)
	})
	return r
}

// send sends text holding the control token.
func (r *streamRig) send(text string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := r.conv.AcquireControl(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	defer release()
	id, err := r.conv.Send(ctx, text)
	if err != nil {
		r.t.Fatalf("Send(%q): %v", text, err)
	}
	return id
}

// await returns the first event matching pred, waiting up to 15 s.
func (r *streamRig) await(what string, pred func(ConversationEvent) bool) ConversationEvent {
	r.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		r.mu.Lock()
		for _, ev := range r.evs {
			if pred(ev) {
				r.mu.Unlock()
				return ev
			}
		}
		r.mu.Unlock()
		select {
		case <-r.news:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			r.t.Fatalf("no event: %s; got %+v", what, r.events())
		}
	}
}

func (r *streamRig) events() []ConversationEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ConversationEvent(nil), r.evs...)
}

// ended waits for turn id's terminal event.
func (r *streamRig) ended(id string) Turn {
	r.t.Helper()
	return r.await("turn "+id+" ended", func(ev ConversationEvent) bool {
		return ev.Type == EventTurn && ev.Turn.ID == id && ev.Turn.State != TurnStatePending && ev.Turn.State != TurnStateStreaming
	}).Turn
}

func TestStream_TurnsInOrder(t *testing.T) {
	r := openStreamRig(t, nil)
	for _, x := range []string{"one", "two"} {
		id := r.send("PING " + x)
		if turn := r.ended(id); turn.State != TurnStateComplete || turn.Text != "PONG "+x {
			t.Fatalf("turn = %+v, want complete PONG %s", turn, x)
		}
	}
	var got []string
	for _, ev := range r.events() {
		if ev.Type == EventTurn {
			got = append(got, string(ev.Turn.Role)+":"+string(ev.Turn.State))
		}
	}
	want := "user:complete assistant:pending assistant:complete user:complete assistant:pending assistant:complete"
	if strings.Join(got, " ") != want {
		t.Fatalf("turn events = %v, want %s", got, want)
	}
	st := r.conv.State()
	if !st.Alive || st.PID == 0 || st.Busy || st.Turn != nil || st.LastOutputAt.IsZero() {
		t.Fatalf("State = %+v, want alive, idle, with a pid and output", st)
	}
	if snap := r.conv.ScreenSnapshot(); snap.Text != "" {
		t.Fatalf("ScreenSnapshot = %q, want empty: there is no screen", snap.Text)
	}
	if err := r.conv.Resize(80, 24); err != nil || r.conv.Wrapper() != nil {
		t.Fatalf("Resize = %v, Wrapper = %v; want no-ops", err, r.conv.Wrapper())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.conv.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	evs := r.events()
	if last := evs[len(evs)-1]; last.Type != EventExited || last.Exit == nil {
		t.Fatalf("last event = %+v, want EventExited", last)
	}
	for range r.conv.Events() {
	} // closes after EventExited
}

func TestStream_LaunchArgs(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want []string
		not  []string
	}{
		{"bypass", []string{"-p", "--input-format\nstream-json", "--output-format\nstream-json", "--verbose", "--include-partial-messages", "--permission-mode\nbypassPermissions", "--session-id"}, []string{"--permission-prompt-tool"}},
		{"ask", []string{"--permission-mode\nacceptEdits", "--permission-prompt-tool\nstdio"}, nil},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			argv := filepath.Join(t.TempDir(), "argv")
			openStreamRig(t, func(o *Options) {
				o.PermissionMode = tc.mode
				o.Env = append(o.Env, streamFakeArgvEnv+"="+argv)
			})
			b, err := os.ReadFile(argv)
			if err != nil {
				t.Fatal(err)
			}
			got := "\n" + string(b) + "\n"
			for _, w := range tc.want {
				if !strings.Contains(got, "\n"+w+"\n") {
					t.Errorf("argv lacks %q:\n%s", w, b)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, "\n"+n+"\n") {
					t.Errorf("argv has %q:\n%s", n, b)
				}
			}
		})
	}
}

func interruptNow(t *testing.T, c *Conversation) (InterruptResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.Interrupt(ctx)
}

func TestStream_Interrupt(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		after        time.Duration
		want         InterruptResult
		textPrefix   string
	}{
		{"mid-reply", "SLOW 50", 450 * time.Millisecond, InterruptStopped, "chunk0 chunk1 chunk2"},
		{"before-first-token", "DELAY", 300 * time.Millisecond, InterruptCancelled, ""},
		{"mid-tool", "TOOL", 300 * time.Millisecond, InterruptStopped, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := openStreamRig(t, nil)
			id := r.send(tc.prompt)
			time.Sleep(tc.after)
			res, err := interruptNow(t, r.conv)
			if err != nil || res != tc.want {
				t.Fatalf("Interrupt = %v, %v; want %v", res, err, tc.want)
			}
			turn := r.ended(id)
			if turn.State != TurnStateInterrupted || !strings.HasPrefix(turn.Text, tc.textPrefix) ||
				(tc.textPrefix == "" && turn.Text != "") || !strings.Contains(turn.Reason, "interrupted") ||
				strings.Contains(turn.Reason, "at the terminal") {
				t.Fatalf("turn = %+v, want interrupted by the call with text %q…", turn, tc.textPrefix)
			}
			next := r.send("PING after")
			if turn := r.ended(next); turn.State != TurnStateComplete || turn.Text != "PONG after" {
				t.Fatalf("next turn = %+v, want it answered", turn)
			}
		})
	}
	t.Run("no-turn", func(t *testing.T) {
		r := openStreamRig(t, nil)
		if res, err := interruptNow(t, r.conv); err != nil || res != InterruptNoTurn {
			t.Fatalf("Interrupt = %v, %v; want no_turn", res, err)
		}
	})
	t.Run("concurrent-callers-join", func(t *testing.T) {
		r := openStreamRig(t, nil)
		r.send("SLOW 50")
		time.Sleep(300 * time.Millisecond)
		results := make(chan InterruptResult, 3)
		for i := 0; i < 3; i++ {
			go func() {
				res, _ := interruptNow(t, r.conv)
				results <- res
			}()
		}
		for i := 0; i < 3; i++ {
			if res := <-results; res != InterruptStopped {
				t.Fatalf("caller %d got %v, want stopped", i, res)
			}
		}
	})
}

func TestStream_APIErrors(t *testing.T) {
	r := openStreamRig(t, nil)
	id := r.send("ERR529")
	turn := r.ended(id)
	if turn.State != TurnStateErrored || turn.HTTPCode != 529 || turn.Code != "" ||
		!strings.Contains(turn.Reason, "harness tag: server_error") || turn.Text != "" {
		t.Fatalf("turn = %+v, want errored 529 tagged server_error", turn)
	}
	if st := r.conv.State(); st.Status != "" {
		t.Fatalf("State.Status = %q after the turn ended, want the retry cleared", st.Status)
	}
	id = r.send("LIMIT")
	turn = r.ended(id)
	if turn.State != TurnStateErrored || turn.Code != CodeUsageLimited || turn.ResumeAt.IsZero() || turn.HTTPCode != 429 {
		t.Fatalf("turn = %+v, want usage_limited with a reset time", turn)
	}
	id = r.send("REFUSE")
	if turn := r.ended(id); turn.State != TurnStateErrored || !strings.Contains(turn.Reason, "refused") {
		t.Fatalf("turn = %+v, want errored: refused", turn)
	}
}

func TestStream_PermissionPrompt(t *testing.T) {
	t.Run("surfaced-and-answered", func(t *testing.T) {
		r := openStreamRig(t, func(o *Options) { o.PermissionMode = "ask" })
		id := r.send("PERM")
		ev := r.await("input request", func(ev ConversationEvent) bool { return ev.Type == EventInputRequest })
		if ev.Input.Kind != streamPermissionKind || !strings.Contains(ev.Input.Prompt, "Bash") {
			t.Fatalf("request = %+v", ev.Input)
		}
		if p := r.conv.PendingInput(); p == nil || p.ID != ev.Input.ID {
			t.Fatalf("PendingInput = %+v", p)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		release, _ := r.conv.AcquireControl(ctx)
		err := r.conv.Answer(ctx, ev.Input.ID, InputAnswer{OptionID: "allow"})
		release()
		if err != nil {
			t.Fatalf("Answer: %v", err)
		}
		if turn := r.ended(id); turn.Text != "tool allow" {
			t.Fatalf("turn = %+v, want the tool allowed", turn)
		}
		if r.conv.PendingInput() != nil {
			t.Fatal("PendingInput after the answer")
		}
	})
	t.Run("policy-denies", func(t *testing.T) {
		r := openStreamRig(t, func(o *Options) {
			o.PermissionMode = "ask"
			o.InputPolicy = &InputPolicy{ByKind: map[string]Disposition{
				streamPermissionKind: {Kind: DispositionAnswer, OptionID: "deny"},
			}}
		})
		if turn := r.ended(r.send("PERM")); turn.Text != "tool deny" {
			t.Fatalf("turn = %+v, want the tool denied by policy", turn)
		}
	})
}

func TestStream_ExitMidTurn(t *testing.T) {
	r := openStreamRig(t, nil)
	id := r.send("EXIT")
	turn := r.ended(id)
	if turn.State != TurnStateErrored || !strings.Contains(turn.Reason, "harness exited") {
		t.Fatalf("turn = %+v, want errored: harness exited", turn)
	}
	ev := r.await("exited", func(ev ConversationEvent) bool { return ev.Type == EventExited })
	if ev.Exit.ExitCode != 3 || ev.Exit.Status != wrapper.StatusFailed || !strings.Contains(ev.Exit.Reason, "exiting on request") {
		t.Fatalf("exit = %+v, want code 3 with the stderr line", ev.Exit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, _ := r.conv.AcquireControl(ctx)
	defer release()
	if _, err := r.conv.Send(ctx, "PING late"); !errors.Is(err, ErrExited) {
		t.Fatalf("Send after exit = %v, want ErrExited", err)
	}
}

func TestStream_MissingCapability(t *testing.T) {
	r := openStreamRig(t, func(o *Options) { o.Env = append(o.Env, streamFakeCapsEnv+"=msg_lifecycle_v1") })
	id := r.send("PING x")
	turn := r.ended(id)
	if turn.State != TurnStateErrored || !strings.Contains(turn.Reason, "lacks interrupt_receipt_v1") {
		t.Fatalf("turn = %+v, want errored for the missing capability", turn)
	}
	r.await("exited", func(ev ConversationEvent) bool { return ev.Type == EventExited })
}

func TestStream_Quit(t *testing.T) {
	r := openStreamRig(t, nil)
	r.ended(r.send("PING x"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.conv.Quit(ctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	select {
	case <-r.conv.Done():
	case <-ctx.Done():
		t.Fatal("claude did not exit after Quit")
	}
	if st := r.conv.State(); st.Exit == nil || st.Exit.ExitCode != 0 || st.Exit.Status != wrapper.StatusIdle || st.Alive {
		t.Fatalf("State = %+v, want a clean exit", st)
	}
}

func TestStream_Reopen(t *testing.T) {
	store := newFakeStore()
	dir := t.TempDir()
	r := openStreamRig(t, func(o *Options) { o.Store = store; o.WorkingDir = dir })
	r.ended(r.send("PING x"))
	harnessID := r.conv.State().HarnessSessionID
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.conv.Close(ctx); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(t.TempDir(), "argv")
	base := fakeStreamOptions(streamFakeArgvEnv + "=" + argv)
	conv, err := Reopen(ctx, ReopenOptions{
		SessionID: r.conv.SessionID(), Transport: TransportStreamJSON, BinaryPath: base.BinaryPath,
		Env: base.Env, PermissionMode: "bypass", Store: store,
	})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = conv.Close(ctx) }()
	b, _ := os.ReadFile(argv)
	if !strings.Contains(string(b), "--resume\n"+harnessID) {
		t.Fatalf("argv lacks --resume %s:\n%s", harnessID, b)
	}
}

func TestStream_SetPermissionMode(t *testing.T) {
	r := openStreamRig(t, nil)
	r.ended(r.send("PING x")) // system/init reports the mode
	if mode, ok := r.conv.PermissionMode(); !ok || mode != "bypass" {
		t.Fatalf("PermissionMode = %q, %v; want bypass", mode, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, _ := r.conv.AcquireControl(ctx)
	defer release()
	if got, err := r.conv.SetPermissionMode(ctx, "plan"); err != nil || got != "plan" {
		t.Fatalf("SetPermissionMode = %q, %v; want plan", got, err)
	}
	if mode, _ := r.conv.PermissionMode(); mode != "plan" {
		t.Fatalf("PermissionMode after the switch = %q", mode)
	}
}

func TestStream_RefusedOptions(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*Options){
		"codex":             func(o *Options) { o.Harness = "codex" },
		"containment":       func(o *Options) { o.Containment = &wrapper.Containment{} },
		"reserved argument": func(o *Options) { o.Args = []string{"--output-format", "text"} },
		"unknown transport": func(o *Options) { o.Transport = "carrier-pigeon" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := fakeStreamOptions()
			opts.Store = newFakeStore()
			mutate(&opts)
			if _, err := Open(ctx, opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("Open = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestReadBoundedLine(t *testing.T) {
	in := "short\n" + strings.Repeat("x", 100) + "\nnext\ntail"
	r := bufio.NewReaderSize(strings.NewReader(in), 16)
	var got []string
	for {
		line, err := readBoundedLine(r, 50)
		if line != nil {
			got = append(got, string(line))
		}
		if err != nil {
			break
		}
	}
	if strings.Join(got, "|") != "short|next|tail" {
		t.Fatalf("lines = %q, want the 100-byte line dropped whole", got)
	}
}
