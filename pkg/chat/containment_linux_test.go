//go:build linux

package chat_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// orderStore records whether the harness had started when CreateSession ran.
type orderStore struct {
	*countingStore
	argv          string
	startedBefore bool
}

func (s *orderStore) CreateSession(ctx context.Context, sess *chat.Session) error {
	if _, err := os.Stat(s.argv); err == nil {
		s.startedBefore = true
	}
	return s.countingStore.CreateSession(ctx, sess)
}

// containedFake sets up the fake harness under a test profile and returns the
// binary, its launch environment and a containment request that can read the
// script and write the argv dump.
func containedFake(t *testing.T, argvOut string) (string, []string, *wrapper.Containment) {
	t.Helper()
	if _, err := landlock.Probe(0); err != nil {
		if v := os.Getenv("HW_LANDLOCK_REQUIRE_ABI"); v != "" && v != "0" {
			t.Fatalf("Landlock ABI 9 required: %v", err)
		}
		t.Skipf("Landlock ABI 9 unavailable: %v", err)
	}
	bin := buildFake(t)
	realBin, _ := filepath.EvalSymlinks(bin)
	t.Cleanup(contain.RegisterTestProfile(contain.TestProfile{
		Harness: "claude-code", ExecDirs: []string{filepath.Dir(realBin)}, ConfigEnv: "CLAUDE_CONFIG_DIR",
	}))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := fakeLaunchEnv(t, fakeharness.New("claude-code").Idle().StayAliveUntilStopped().Build(), argvOut)
	var scriptDir string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, fakeharness.EnvVar+"="); ok {
			scriptDir = filepath.Dir(v)
		}
	}
	return bin, env, &wrapper.Containment{
		Kind:      wrapper.ContainmentLandlock,
		ReadOnly:  []string{scriptDir},
		ReadWrite: []string{filepath.Dir(argvOut)},
		PassEnv:   []string{fakeharness.EnvVar, fakeharness.ArgvOutVar},
	}
}

// openStored opens a stored contained conversation, skipping where the host
// delegates no cgroup (unless the cell requires supervision).
func openStored(t *testing.T, opts chat.Options) *chat.Conversation {
	t.Helper()
	conv, err := chat.Open(context.Background(), opts)
	if err != nil {
		if strings.Contains(err.Error(), "supervision") {
			if os.Getenv("HW_LANDLOCK_REQUIRE_SUPERVISION") != "" {
				t.Fatalf("stored contained conversation refused: %v", err)
			}
			t.Skipf("no cgroup supervision here: %v", err)
		}
		t.Fatalf("Open: %v", err)
	}
	return conv
}

// markDiscovered records a harness session id in the stored containment
// record, as the line tap does once the harness reports one.
func markDiscovered(t *testing.T, store chat.Store, id string) {
	t.Helper()
	rec, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	discovered := *rec
	discovered.Containment = rec.Containment.Clone()
	discovered.Containment.HarnessSessionID = uuid
	if err := store.UpdateSession(context.Background(), &discovered); err != nil {
		t.Fatal(err)
	}
}

// TestContainedConversationLifecycle walks a stored contained conversation:
// its record is persisted before the harness starts, a resume inherits it, a
// different policy is refused before launch, and deleting its state makes it
// unresumable.
func TestContainedConversationLifecycle(t *testing.T) {
	argvOut := filepath.Join(t.TempDir(), "argv.json")
	bin, env, req := containedFake(t, argvOut)
	store := &orderStore{countingStore: newCountingStore(), argv: argvOut}
	wd := t.TempDir()

	conv := openStored(t, chat.Options{
		Harness: "claude-code", BinaryPath: bin, WorkingDir: wd, Env: env, Store: store, Containment: req,
	})
	if store.startedBefore {
		t.Fatal("the harness started before the containment record was persisted")
	}
	readArgv(t, argvOut) // the fake is up
	rec, err := store.GetSession(context.Background(), conv.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	c := rec.Containment
	if c == nil || !c.Resumable || c.StateID == "" || c.LastLaunch == nil || len(c.Targets) == 0 || rec.HarnessSessionID != "" {
		t.Fatalf("stored record = %+v / %+v", rec, c)
	}
	first := conv.Containment()
	if first == nil || first.Supervision.Mode != "cgroup" {
		t.Fatalf("applied = %+v", first)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = conv.Close(ctx)

	// The harness's session id is discovered into the record (as the line tap
	// would), never into the legacy field.
	markDiscovered(t, store, conv.SessionID())

	// Resume inherits the record: no containment in the options, contained
	// launch, same policy fingerprint, resume args carry the recorded id.
	_ = os.Remove(argvOut)
	conv2, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID: conv.SessionID(), BinaryPath: bin, Env: env, Store: store,
	})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	argv := readArgv(t, argvOut)
	if len(argv) < 2 || argv[0] != "--resume" || argv[1] != uuid {
		t.Fatalf("resume argv = %v", argv)
	}
	second := conv2.Containment()
	if second == nil || second.Fingerprint != first.Fingerprint || second.State.ID != first.State.ID {
		t.Fatalf("resumed launch policy %+v differs from %+v", second, first)
	}
	_ = conv2.Close(ctx)

	// A different policy is refused before anything launches.
	_ = os.Remove(argvOut)
	other := req.Clone()
	other.RestrictTCP = true
	if _, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID: conv.SessionID(), BinaryPath: bin, Env: env, Store: store, Containment: other,
	}); !errors.Is(err, chat.ErrInvalidOptions) {
		t.Fatalf("reopen with another policy: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(argvOut); err == nil {
		t.Fatal("a refused reopen launched the harness")
	}

	// Deleting the state ends the conversation's resumability.
	if err := chat.DeleteContainmentState(context.Background(), store, conv.SessionID()); err != nil {
		t.Fatalf("DeleteContainmentState: %v", err)
	}
	if _, err := os.Stat(first.State.Home); !os.IsNotExist(err) {
		t.Fatalf("private state still present: %v", err)
	}
	if _, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID: conv.SessionID(), BinaryPath: bin, Env: env, Store: store,
	}); !errors.Is(err, chat.ErrContainmentUnavailable) {
		t.Fatalf("reopen after deletion: %v", err)
	}
}

// TestResumeRefusesRetargetedGrant: a stored contained conversation records
// what each granted path resolved to. Once one resolves elsewhere, a resume is
// refused before anything launches, and restoring the path makes the
// conversation resumable again.
func TestResumeRefusesRetargetedGrant(t *testing.T) {
	argvOut := filepath.Join(t.TempDir(), "argv.json")
	bin, env, req := containedFake(t, argvOut)
	dir := t.TempDir()
	real, decoy := filepath.Join(dir, "real"), filepath.Join(dir, "decoy")
	for _, d := range []string{real, decoy} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link")
	point := func(target string) {
		t.Helper()
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	point(real)
	req.ReadOnly = append(req.ReadOnly, link)
	store := newCountingStore()
	conv := openStored(t, chat.Options{
		Harness: "claude-code", BinaryPath: bin, WorkingDir: t.TempDir(), Env: env, Store: store, Containment: req,
	})
	readArgv(t, argvOut)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = conv.Close(ctx)
	defer func() {
		if err := chat.DeleteContainmentState(context.Background(), store, conv.SessionID()); err != nil {
			t.Errorf("DeleteContainmentState: %v", err)
		}
	}()
	markDiscovered(t, store, conv.SessionID())
	reopen := func() (*chat.Conversation, error) {
		return chat.Reopen(context.Background(), chat.ReopenOptions{
			SessionID: conv.SessionID(), BinaryPath: bin, Env: env, Store: store,
		})
	}

	point(decoy)
	_ = os.Remove(argvOut)
	if _, err := reopen(); !errors.Is(err, wrapper.ErrContainmentRefused) || !strings.Contains(err.Error(), "now resolves to") {
		t.Fatalf("reopen with the grant retargeted: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(argvOut); err == nil {
		t.Fatal("a refused reopen launched the harness")
	}

	point(real)
	conv2, err := reopen()
	if err != nil {
		t.Fatalf("reopen with the grant restored: %v", err)
	}
	readArgv(t, argvOut)
	_ = conv2.Close(ctx)
}

// unwritableStore persists nothing: CreateSession always fails.
type unwritableStore struct{ *countingStore }

func (unwritableStore) CreateSession(context.Context, *chat.Session) error {
	return errors.New("store unavailable")
}

// TestContainedOpenNeedsItsRecordPersisted: a stored contained conversation
// whose containment record cannot be persisted never launches, and the
// private state allocated for it is given back.
func TestContainedOpenNeedsItsRecordPersisted(t *testing.T) {
	argvOut := filepath.Join(t.TempDir(), "argv.json")
	bin, env, req := containedFake(t, argvOut)
	_, err := chat.Open(context.Background(), chat.Options{
		Harness: "claude-code", BinaryPath: bin, WorkingDir: t.TempDir(), Env: env,
		Store: unwritableStore{newCountingStore()}, Containment: req,
	})
	if err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(argvOut); err == nil {
		t.Fatal("the harness started without a persisted containment record")
	}
	parent, _ := contain.StateParent()
	if ents, _ := os.ReadDir(parent); len(ents) != 0 {
		t.Fatalf("a failed open left %d managed-state entries", len(ents))
	}
}

// TestStoredContainedConversationNeedsSupervision: without cgroup supervision
// a stored contained conversation is refused at creation, and the private
// state allocated for it is given back.
func TestStoredContainedConversationNeedsSupervision(t *testing.T) {
	argvOut := filepath.Join(t.TempDir(), "argv.json")
	bin, env, req := containedFake(t, argvOut)
	t.Cleanup(contain.DisableSupervisionForTest())
	_, err := chat.Open(context.Background(), chat.Options{
		Harness: "claude-code", BinaryPath: bin, WorkingDir: t.TempDir(), Env: env, Store: newCountingStore(), Containment: req,
	})
	if !errors.Is(err, wrapper.ErrContainmentRefused) || !strings.Contains(err.Error(), "supervision") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(argvOut); err == nil {
		t.Fatal("the harness started")
	}
	parent, _ := contain.StateParent()
	if ents, _ := os.ReadDir(parent); len(ents) != 0 {
		t.Fatalf("refused open left %d managed-state entries", len(ents))
	}
}
