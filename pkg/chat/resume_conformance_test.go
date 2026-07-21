package chat_test

// Resume plumbing (Options.Resume) and the Reopen convenience helper, driven
// over a REAL fake-harness process. Phase 1 asserts the adapter's resume args
// are prepended to argv and the harness session id is seeded; Phase 2 asserts
// Reopen reuses the SAME chat session id, relaunches in resume mode, and reads
// back the stored session's history — and that both surface the right sentinels.
// Phase 3 exercises the SessionControlFlags conflict check.
//
// This is the Go mirror of meta-harness test/chat/resume.test.ts. It lives in
// the EXTERNAL test package so it can seed pkg/chat/memstore directly (memstore
// imports chat, so an internal `package chat` test importing it would cycle) and
// touch only the PUBLIC chat surface — no conversation.go / chat.go edits.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
)

// uuid is the harness session id every case resumes. It matches the default the
// fake builder stamps into its resume hint, so screen-scrape session-id
// extraction stays consistent with the injected resume id.
const uuid = "11111111-2222-3333-4444-555555555555"

// buildFake compiles cmd/fakeharness once per test binary and returns its path.
// Skips (rather than fails) when the Go toolchain is unavailable, matching the
// internal fakeharness_test.go helper.
func buildFake(t *testing.T) string {
	t.Helper()
	path, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	return path
}

// argvOutPath returns a fresh path for the fake's argv dump.
func argvOutPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "argv.json")
}

// fakeLaunchEnv writes the script to a temp file and returns the full env a
// fake-harness launch needs (script path + optional argv-dump path). wrapper.Start
// REPLACES the environment, so os.Environ() is folded in to keep PATH/TERM.
// Mirrors the TS fakeLaunchEnv used by the resume tests.
func fakeLaunchEnv(t *testing.T, script fakeharness.Script, argvOut string) []string {
	t.Helper()
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(scriptPath, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	env := append(os.Environ(), fakeharness.EnvVar+"="+scriptPath)
	if argvOut != "" {
		env = append(env, fakeharness.ArgvOutVar+"="+argvOut)
	}
	return env
}

// readArgv polls the argv-dump file the fake writes at startup; the write races
// the Open return, so retry briefly before reading. Mirrors the TS readArgv.
func readArgv(t *testing.T, path string) []string {
	t.Helper()
	for i := 0; i < 100; i++ {
		raw, err := os.ReadFile(path)
		if err == nil {
			var got []string
			if err := json.Unmarshal(raw, &got); err == nil {
				return got
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("argv dump never appeared at %s", path)
	return nil
}

// trackClose registers a best-effort Close so a launched Conversation's process
// is torn down at test end.
func trackClose(t *testing.T, conv *chat.Conversation) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conv.Close(ctx)
	})
}

// countingStore wraps memstore.Store to count CreateSession calls, so Reopen can
// be shown to add NO second store record.
type countingStore struct {
	*memstore.Store
	creates int
}

func newCountingStore() *countingStore { return &countingStore{Store: memstore.New()} }

func (s *countingStore) CreateSession(ctx context.Context, sess *chat.Session) error {
	s.creates++
	return s.Store.CreateSession(ctx, sess)
}

func indexOf(argv []string, want string) int {
	for i, tok := range argv {
		if tok == want {
			return i
		}
	}
	return -1
}

func contains(argv []string, want string) bool { return indexOf(argv, want) >= 0 }

// --- Phase 1: Open with Options.Resume ----------------------------------

// TestOpenResume_PrependsClaudeCodeArgs is the core Phase-1 assertion: the
// claude-code resume fragment (`--resume <id>`) leads argv, AHEAD of the
// caller's own args; Session.HarnessSessionID is seeded with the resume id; and
// a fresh chat session record is persisted carrying it.
func TestOpenResume_PrependsClaudeCodeArgs(t *testing.T) {
	bin := buildFake(t)
	store := memstore.New()
	argvPath := argvOutPath(t)
	env := fakeLaunchEnv(t, fakeharness.New("claude-code").Idle().Build(), argvPath)

	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "claude-code",
		BinaryPath: bin,
		Env:        env,
		Store:      store,
		Resume:     uuid,
		Args:       []string{"--foo"},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	trackClose(t, conv)

	argv := readArgv(t, argvPath)
	if len(argv) < 2 || argv[0] != "--resume" || argv[1] != uuid {
		t.Fatalf("argv[:2] = %#v, want [--resume %s]", argv, uuid)
	}
	if !contains(argv, "--foo") {
		t.Fatalf("argv %#v missing caller arg --foo", argv)
	}
	// resumeArgs must precede the caller's own args.
	if ri, fi := indexOf(argv, "--resume"), indexOf(argv, "--foo"); ri >= fi {
		t.Fatalf("resume prefix not ahead of --foo: --resume@%d, --foo@%d (argv %#v)", ri, fi, argv)
	}

	stored, err := store.GetSession(context.Background(), conv.SessionID())
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if stored.HarnessSessionID != uuid {
		t.Fatalf("stored HarnessSessionID = %q, want %q", stored.HarnessSessionID, uuid)
	}
}

// TestOpenResume_PrependsCodexArgs asserts the codex resume fragment
// (`resume <id>`, no leading dash) leads argv.
func TestOpenResume_PrependsCodexArgs(t *testing.T) {
	bin := buildFake(t)
	argvPath := argvOutPath(t)
	env := fakeLaunchEnv(t, fakeharness.New("codex").Idle().Build(), argvPath)

	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "codex",
		BinaryPath: bin,
		Env:        env,
		Store:      memstore.New(),
		Resume:     uuid,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	trackClose(t, conv)

	argv := readArgv(t, argvPath)
	if len(argv) < 2 || argv[0] != "resume" || argv[1] != uuid {
		t.Fatalf("argv[:2] = %#v, want [resume %s]", argv, uuid)
	}
}

// TestOpenResume_PrependsPiArgs asserts the pi resume fragment
// (`--session <id>`) leads argv.
func TestOpenResume_PrependsPiArgs(t *testing.T) {
	bin := buildFake(t)
	argvPath := argvOutPath(t)
	env := fakeLaunchEnv(t, fakeharness.New("pi").PiIdle().Build(), argvPath)

	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "pi",
		BinaryPath: bin,
		Env:        env,
		Store:      memstore.New(),
		Resume:     uuid,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	trackClose(t, conv)

	argv := readArgv(t, argvPath)
	if len(argv) < 2 || argv[0] != "--session" || argv[1] != uuid {
		t.Fatalf("argv[:2] = %#v, want [--session %s]", argv, uuid)
	}
}

// TestOpenResume_OpencodeUnsupported: opencode has no SessionResumer, so Open
// rejects with ErrResumeUnsupported BEFORE spawning any process.
func TestOpenResume_OpencodeUnsupported(t *testing.T) {
	bin := buildFake(t)
	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "opencode",
		BinaryPath: bin,
		Store:      memstore.New(),
		Resume:     uuid,
	})
	if conv != nil {
		trackClose(t, conv)
		t.Fatalf("Open returned a live conversation; want nil on ErrResumeUnsupported")
	}
	if !errors.Is(err, chat.ErrResumeUnsupported) {
		t.Fatalf("err = %v, want errors.Is ErrResumeUnsupported", err)
	}
}

// --- Phase 2: Reopen ----------------------------------------------------

// seedSession inserts a hand-built Session record directly into the store, the
// way resume.test.ts:192-209 does — needed because some records (e.g. an
// opencode session carrying a harness id) cannot be produced via Open.
func seedSession(t *testing.T, store chat.Store, sess chat.Session) {
	t.Helper()
	if err := store.CreateSession(context.Background(), &sess); err != nil {
		t.Fatalf("seed CreateSession: %v", err)
	}
}

// TestReopen_ReusesStoredRecord: Reopen reuses the stored chat session id, adds
// NO second store record, relaunches in resume mode with the stored harness
// session id, and the prior turn stays reachable under the reused id.
func TestReopen_ReusesStoredRecord(t *testing.T) {
	bin := buildFake(t)
	store := newCountingStore()
	const storedID = "chat-sess-reopen"
	workingDir := t.TempDir()
	seedSession(t, store, chat.Session{
		ID:               storedID,
		Harness:          "claude-code",
		WorkingDir:       workingDir,
		CreatedAt:        time.Now(),
		HarnessSessionID: uuid,
	})
	prior := chat.Turn{
		ID:        "t1",
		SessionID: storedID,
		Role:      chat.RoleAssistant,
		State:     chat.TurnStateComplete,
		Text:      "earlier reply",
	}
	if err := store.AppendTurn(context.Background(), &prior); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	argvPath := argvOutPath(t)
	env := fakeLaunchEnv(t, fakeharness.New("claude-code").Idle().Build(), argvPath)

	conv, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID:  storedID,
		BinaryPath: bin,
		Env:        env,
		Store:      store,
	})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	trackClose(t, conv)

	// Same chat session id — not a freshly-minted one.
	if conv.SessionID() != storedID {
		t.Fatalf("conv.SessionID() = %q, want %q", conv.SessionID(), storedID)
	}
	// The seed was the ONLY CreateSession; Reopen must not add a second.
	if store.creates != 1 {
		t.Fatalf("CreateSession call count = %d, want 1 (Reopen must not create)", store.creates)
	}

	argv := readArgv(t, argvPath)
	if len(argv) < 2 || argv[0] != "--resume" || argv[1] != uuid {
		t.Fatalf("argv[:2] = %#v, want [--resume %s] (resume fragment carries stored id)", argv, uuid)
	}

	// The prior turn is still reachable under the REUSED chat session id — the
	// proof Reopen attached the stored record rather than minting a new one.
	turns, err := store.ListTurns(context.Background(), conv.SessionID())
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	var found bool
	for _, tn := range turns {
		if tn.Text == "earlier reply" {
			found = true
		}
	}
	if !found {
		t.Fatalf("prior turn %q not reachable under reused session id (turns %#v)", "earlier reply", turns)
	}
}

// TestReopen_NoHarnessSession: Reopen returns ErrNoHarnessSession when the
// stored record captured no harness session id.
func TestReopen_NoHarnessSession(t *testing.T) {
	bin := buildFake(t)
	store := memstore.New()
	const storedID = "chat-sess-empty"
	seedSession(t, store, chat.Session{
		ID:               storedID,
		Harness:          "claude-code",
		WorkingDir:       "",
		CreatedAt:        time.Now(),
		HarnessSessionID: "",
	})
	conv, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID:  storedID,
		BinaryPath: bin,
		Store:      store,
	})
	if conv != nil {
		trackClose(t, conv)
		t.Fatalf("Reopen returned a live conversation; want nil on ErrNoHarnessSession")
	}
	if !errors.Is(err, chat.ErrNoHarnessSession) {
		t.Fatalf("err = %v, want errors.Is ErrNoHarnessSession", err)
	}
}

// TestReopen_OpencodeUnsupported: Reopen propagates ErrResumeUnsupported before
// launch. The check order is Store → GetSession → empty-id → capability, so the
// stored record MUST carry a non-empty HarnessSessionID; Go's opencode adapter
// has no session-id extractor, so such a record cannot be produced via Open —
// it is seeded directly.
func TestReopen_OpencodeUnsupported(t *testing.T) {
	bin := buildFake(t)
	store := memstore.New()
	const storedID = "chat-sess-opencode"
	seedSession(t, store, chat.Session{
		ID:               storedID,
		Harness:          "opencode",
		WorkingDir:       "",
		CreatedAt:        time.Now(),
		HarnessSessionID: uuid,
	})
	conv, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID:  storedID,
		BinaryPath: bin,
		Store:      store,
	})
	if conv != nil {
		trackClose(t, conv)
		t.Fatalf("Reopen returned a live conversation; want nil on ErrResumeUnsupported")
	}
	if !errors.Is(err, chat.ErrResumeUnsupported) {
		t.Fatalf("err = %v, want errors.Is ErrResumeUnsupported", err)
	}
}

// TestReopen_UnknownSession: an unknown session id surfaces the store's own
// GetSession error (not a resume/session sentinel).
func TestReopen_UnknownSession(t *testing.T) {
	bin := buildFake(t)
	store := memstore.New()
	conv, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID:  "does-not-exist",
		BinaryPath: bin,
		Store:      store,
	})
	if conv != nil {
		trackClose(t, conv)
		t.Fatalf("Reopen returned a live conversation; want nil for unknown session id")
	}
	if err == nil {
		t.Fatal("Reopen err = nil, want the store's GetSession error")
	}
	if errors.Is(err, chat.ErrNoHarnessSession) || errors.Is(err, chat.ErrResumeUnsupported) {
		t.Fatalf("err = %v, want the store's own GetSession error, not a resume/session sentinel", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want it to surface the store's not-found error", err)
	}
}

// --- Phase 3: SessionControlFlags conflict check ------------------------

// TestOpenResume_ConflictBareFlag: Options.Resume combined with a bare banned
// flag (`--resume X`) in Options.Args is rejected via ErrInvalidOptions before
// launch.
func TestOpenResume_ConflictBareFlag(t *testing.T) {
	bin := buildFake(t)
	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "claude-code",
		BinaryPath: bin,
		Store:      memstore.New(),
		Resume:     uuid,
		Args:       []string{"--resume", "X"},
	})
	if conv != nil {
		trackClose(t, conv)
		t.Fatalf("Open returned a live conversation; want nil on ErrInvalidOptions")
	}
	if !errors.Is(err, chat.ErrInvalidOptions) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidOptions", err)
	}
}

// TestOpenResume_ConflictAttachedFlag: the attached long-flag form (`--resume=X`)
// is likewise rejected.
func TestOpenResume_ConflictAttachedFlag(t *testing.T) {
	bin := buildFake(t)
	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "claude-code",
		BinaryPath: bin,
		Store:      memstore.New(),
		Resume:     uuid,
		Args:       []string{"--resume=X"},
	})
	if conv != nil {
		trackClose(t, conv)
		t.Fatalf("Open returned a live conversation; want nil on ErrInvalidOptions")
	}
	if !errors.Is(err, chat.ErrInvalidOptions) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidOptions", err)
	}
}

// TestOpenResume_PositionalAfterDashDashAccepted: a banned token AFTER a bare
// `--` terminator is a positional, not a flag, so it is NOT rejected — Open
// launches normally.
func TestOpenResume_PositionalAfterDashDashAccepted(t *testing.T) {
	bin := buildFake(t)
	argvPath := argvOutPath(t)
	env := fakeLaunchEnv(t, fakeharness.New("claude-code").Idle().Build(), argvPath)

	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "claude-code",
		BinaryPath: bin,
		Env:        env,
		Store:      memstore.New(),
		Resume:     uuid,
		Args:       []string{"--", "--resume"},
	})
	if err != nil {
		t.Fatalf("Open with positional-after-`--` rejected: %v", err)
	}
	trackClose(t, conv)

	argv := readArgv(t, argvPath)
	if len(argv) < 2 || argv[0] != "--resume" || argv[1] != uuid {
		t.Fatalf("argv[:2] = %#v, want [--resume %s]", argv, uuid)
	}
}

// TestOpenResume_CodexConflictAccepted: the SAME conflicting combination for
// codex (`resume X` in Options.Args) is ACCEPTED — codex declares no
// session-control flags. Matches the TS conformance behavior.
func TestOpenResume_CodexConflictAccepted(t *testing.T) {
	bin := buildFake(t)
	argvPath := argvOutPath(t)
	env := fakeLaunchEnv(t, fakeharness.New("codex").Idle().Build(), argvPath)

	conv, err := chat.Open(context.Background(), chat.Options{
		Harness:    "codex",
		BinaryPath: bin,
		Env:        env,
		Store:      memstore.New(),
		Resume:     uuid,
		Args:       []string{"resume", "X"},
	})
	if err != nil {
		t.Fatalf("Open for codex with `resume X` rejected; codex declares no session-control flags: %v", err)
	}
	trackClose(t, conv)

	argv := readArgv(t, argvPath)
	if len(argv) < 2 || argv[0] != "resume" || argv[1] != uuid {
		t.Fatalf("argv[:2] = %#v, want [resume %s] (chat's resume fragment leads)", argv, uuid)
	}
}
