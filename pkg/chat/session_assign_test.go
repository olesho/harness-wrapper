package chat_test

// Session ids assigned at launch (turns.SessionAssigner), driven over a REAL
// fake-harness process. A fresh claude-code or pi conversation is started under
// an id chat assigns — minted, or the caller's Options.HarnessSessionID — and
// the stored session carries it before any turn runs, so everything that reads
// the harness's own transcript works from turn 1. The Go mirror of
// meta-harness's test/chat/claudecode_session.test.ts, plus the typed id and
// the in-use check that meta-harness does not have.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// assignedID is an id a caller supplies; distinct from the fake's default hint.
const assignedID = "0c1a55e5-5e55-4e55-8e55-000000000001"

// openAssigned opens a fresh conversation on the fake with the given script,
// dumping its argv, and returns it with the argv and the store.
func openAssigned(t *testing.T, script fakeharness.Script, mutate func(*chat.Options)) (*chat.Conversation, []string, *memstore.Store) {
	t.Helper()
	argvOut := argvOutPath(t)
	store := memstore.New()
	opts := chat.Options{
		Harness:    script.Harness,
		BinaryPath: buildFake(t),
		Env:        fakeLaunchEnv(t, script, argvOut),
		Store:      store,
	}
	if mutate != nil {
		mutate(&opts)
	}
	conv, err := chat.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	trackClose(t, conv)
	return conv, readArgv(t, argvOut), store
}

func storedHarnessID(t *testing.T, store *memstore.Store, conv *chat.Conversation) string {
	t.Helper()
	rec, err := store.GetSession(context.Background(), conv.SessionID())
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return rec.HarnessID()
}

func TestOpen_AssignsMintedSessionID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script fakeharness.Script
	}{
		{"claude-code", fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build()},
		{"pi", fakeharness.New("pi").PiIdle().StayAliveUntilStopped().Build()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conv, argv, store := openAssigned(t, tc.script, nil)
			if len(argv) < 2 || argv[0] != "--session-id" || !uuidRE.MatchString(argv[1]) {
				t.Fatalf("argv = %q, want a leading --session-id <uuid>", argv)
			}
			if got := storedHarnessID(t, store, conv); got != argv[1] {
				t.Fatalf("stored harness session id = %q, want the launch's %q", got, argv[1])
			}
		})
	}
}

func TestOpen_AssignsCallerSessionID(t *testing.T) {
	conv, argv, store := openAssigned(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build(),
		func(o *chat.Options) { o.HarnessSessionID = assignedID })
	if len(argv) < 2 || argv[0] != "--session-id" || argv[1] != assignedID {
		t.Fatalf("argv = %q, want --session-id %s first", argv, assignedID)
	}
	if got := storedHarnessID(t, store, conv); got != assignedID {
		t.Fatalf("stored harness session id = %q, want %q", got, assignedID)
	}
}

// TestOpen_ExitHintDoesNotReplaceAssignedID: the fake paints a resume hint
// naming a different id and then exits — the id the launch assigned stands.
func TestOpen_ExitHintDoesNotReplaceAssignedID(t *testing.T) {
	const hintID = "abcd1234-0000-4000-8000-00000000cafe"
	script := fakeharness.New("claude-code").Session(hintID).Idle().Raw(50, "claude --resume "+hintID).Exit(0).Build()
	conv, argv, store := openAssigned(t, script, nil)
	if _, err := conv.Wrapper().Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := storedHarnessID(t, store, conv); got != argv[1] || got == hintID {
		t.Fatalf("stored harness session id = %q, want the assigned %q, not the hint's", got, argv[1])
	}
}

// TestOpen_RefusesBeforeLaunch covers every refusal: each is decided before the
// harness starts, so the fake never dumps its argv.
func TestOpen_RefusesBeforeLaunch(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		mutate  func(*chat.Options)
	}{
		{"id not a UUID", "claude-code", func(o *chat.Options) { o.HarnessSessionID = "session-1" }},
		{"harness takes no id", "codex", func(o *chat.Options) { o.HarnessSessionID = assignedID }},
		{"id with Resume", "claude-code", func(o *chat.Options) { o.HarnessSessionID = assignedID; o.Resume = uuid }},
		{"--session-id in Args", "claude-code", func(o *chat.Options) { o.Args = []string{"--session-id", assignedID} }},
		{"attached --session-id= in Args", "claude-code", func(o *chat.Options) { o.Args = []string{"--session-id=" + assignedID} }},
		{"--resume in Args", "claude-code", func(o *chat.Options) { o.Args = []string{"--resume", uuid} }},
		{"short -r in Args", "claude-code", func(o *chat.Options) { o.Args = []string{"-r"} }},
		{"--continue in Args", "claude-code", func(o *chat.Options) { o.Args = []string{"--continue"} }},
		{"--fork-session in Args", "claude-code", func(o *chat.Options) { o.Args = []string{"--fork-session"} }},
		{"--no-session-persistence in Args", "claude-code", func(o *chat.Options) { o.Args = []string{"--no-session-persistence"} }},
		{"pi --session in Args", "pi", func(o *chat.Options) { o.Args = []string{"--session", uuid} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argvOut := argvOutPath(t)
			opts := chat.Options{
				Harness:    tc.harness,
				BinaryPath: buildFake(t),
				Env:        fakeLaunchEnv(t, fakeharness.New(tc.harness).Idle().StayAliveUntilStopped().Build(), argvOut),
				Store:      memstore.New(),
			}
			tc.mutate(&opts)
			conv, err := chat.Open(context.Background(), opts)
			if err == nil {
				trackClose(t, conv)
				t.Fatal("Open succeeded, want ErrInvalidOptions")
			}
			if !errors.Is(err, chat.ErrInvalidOptions) {
				t.Fatalf("Open error = %v, want ErrInvalidOptions", err)
			}
			time.Sleep(50 * time.Millisecond)
			if _, statErr := os.Stat(argvOut); statErr == nil {
				t.Fatal("the harness was launched; a refusal must come first")
			}
		})
	}
}

// TestOpen_PositionalAfterTerminatorIsNotAFlag: "--" ends the flags, so a
// session-control word after it is a positional and the assigned id still
// leads the argv.
func TestOpen_PositionalAfterTerminatorIsNotAFlag(t *testing.T) {
	_, argv, _ := openAssigned(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build(),
		func(o *chat.Options) { o.Args = []string{"--", "--resume"} })
	if argv[0] != "--session-id" || !contains(argv, "--") || !contains(argv, "--resume") {
		t.Fatalf("argv = %q, want the assigned id first and the positional kept", argv)
	}
}

// TestOpen_RefusesSessionInUse: the caller's id already has a transcript under
// the launch's config root. claude would refuse to start ("Session ID … is
// already in use"); chat refuses first, with a matchable sentinel.
func TestOpen_RefusesSessionInUse(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "claude")
	wd := t.TempDir()
	stageTranscript(t, cfgDir, wd, assignedID, `{"type":"user","message":{"role":"user","content":"earlier"}}`)

	argvOut := argvOutPath(t)
	_, err := chat.Open(context.Background(), chat.Options{
		Harness:          "claude-code",
		BinaryPath:       buildFake(t),
		WorkingDir:       wd,
		Env:              append(fakeLaunchEnv(t, fakeharness.New("claude-code").Idle().Build(), argvOut), "CLAUDE_CONFIG_DIR="+cfgDir),
		Store:            memstore.New(),
		HarnessSessionID: assignedID,
	})
	if !errors.Is(err, chat.ErrHarnessSessionInUse) || !errors.Is(err, chat.ErrInvalidOptions) {
		t.Fatalf("Open error = %v, want ErrHarnessSessionInUse and ErrInvalidOptions", err)
	}
	if _, statErr := os.Stat(argvOut); statErr == nil {
		t.Fatal("the harness was launched under an id already in use")
	}
}

// TestHistory_BeforeFirstFlushComesFromStore: the id is known from launch but
// the harness has written no transcript yet. History is the store's, not an
// error (chatd's /history used to answer 500 here).
// An empty WorkingDir runs the harness in this process's directory, and the
// transcript is looked for there.
func TestHistory_BeforeFirstFlushComesFromStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(t *testing.T) string
	}{
		{"working dir", func(t *testing.T) string { return t.TempDir() }},
		{"inherited working dir", func(*testing.T) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conv, _, _ := openAssigned(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build(),
				func(o *chat.Options) {
					o.WorkingDir = tc.dir(t)
					o.Env = append(o.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(t.TempDir(), "claude"))
				})
			_, src, err := conv.HistoryWithSource(context.Background())
			if err != nil {
				t.Fatalf("HistoryWithSource: %v", err)
			}
			if src != chat.HistorySourceStore {
				t.Fatalf("source = %q, want %q before the first flush", src, chat.HistorySourceStore)
			}
		})
	}
}

// TestAPIErrorRelabel_DecidesFirstTurnOfFreshConversation: the harness records
// the turn's failure in its transcript and paints the error as its reply. The
// transcript verdict needs the session id, which a fresh conversation used to
// learn only when claude exited — so turn 1 completed as a SUCCESS whose reply
// was "API Error: 529 …". With the id assigned at launch the tag decides it.
func TestAPIErrorRelabel_DecidesFirstTurnOfFreshConversation(t *testing.T) {
	const errText = "API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment."
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		TranscriptUser(0).
		TranscriptAPIError(0, "server_error", errText).
		Reply(40, errText, "Baked", "1s").
		StayAliveUntilStopped().
		Build()
	conv, _, _ := openAssigned(t, script, func(o *chat.Options) {
		o.WorkingDir = t.TempDir()
		o.Env = append(o.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(t.TempDir(), "claude"))
	})

	turn := sendAndAwait(t, conv, "do the thing")
	if turn.State != chat.TurnStateErrored {
		t.Fatalf("turn = %+v, want errored by the harness's server_error tag", turn)
	}
	if !strings.Contains(turn.Reason, "harness tag: server_error") {
		t.Fatalf("reason = %q, want the harness's tag as evidence", turn.Reason)
	}
	if turn.Text != "" {
		t.Fatalf("text = %q, want no reply: the error is not the turn's answer", turn.Text)
	}
}

// stageTranscript writes a claude transcript for id where claude keeps it for
// a launch in wd under configRoot.
func stageTranscript(t *testing.T, configRoot, wd, id, body string) {
	t.Helper()
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	dir := filepath.Join(configRoot, "projects", transcriptcc.EncodedCWD(wd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sendAndAwait sends text and returns the assistant turn once it is terminal.
func sendAndAwait(t *testing.T, conv *chat.Conversation, text string) chat.Turn {
	t.Helper()
	return sendAndAwaitWithin(t, conv, text, 15*time.Second)
}

// sendAndAwaitWithin is sendAndAwait with the whole exchange bounded by limit.
func sendAndAwaitWithin(t *testing.T, conv *chat.Conversation, text string, limit time.Duration) chat.Turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()
	id, err := conv.Send(ctx, text)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	for {
		select {
		case ev := <-conv.Events():
			if ev.Type == chat.EventTurn && ev.Turn.ID == id &&
				(ev.Turn.State == chat.TurnStateComplete || ev.Turn.State == chat.TurnStateErrored) {
				return ev.Turn
			}
		case <-ctx.Done():
			t.Fatalf("turn %s never ended", id)
		}
	}
}
