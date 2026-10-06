package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/fakeadapter"
)

// fakeFixture is the kit's fixture for the fake adapter, with the rule named
// by breaks broken ("" for none).
func fakeFixture(t *testing.T, breaks string) Fixture {
	root := t.TempDir()
	bin := fakeadapter.BinaryPath(root)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Fixture{
		Adapter:     fakeadapter.New(fakeadapter.Options{Break: breaks, Tick: 10 * time.Millisecond, StartDelay: 50 * time.Millisecond}),
		HarnessRoot: root,
		Spec: contract.AgentSpec{
			Instructions:      contract.Instructions{Persona: "Be brief.", Workspace: "# Workspace\n"},
			Skills:            []contract.Skill{{Name: "review", Files: []contract.SkillFile{{Path: "SKILL.md", Content: "# Review\n"}}}},
			Memory:            &contract.Memory{Files: []contract.MemoryFile{{Path: "notes.md", Content: "remember\n"}}},
			Connectors:        []contract.Connector{{Name: "fleet", Stdio: &contract.StdioConnector{Command: "/bin/true"}}},
			PermissionPosture: contract.PostureBypass,
		},
		Credential: func(t T, l contract.Layout) *contract.CredentialFile {
			f := filepath.Join(l.Secrets, fakeadapter.CredentialKind)
			if err := os.WriteFile(f, []byte("secret\n"), 0o600); err != nil {
				t.Errorf("staging the credential: %v", err)
			}
			return &contract.CredentialFile{Kind: fakeadapter.CredentialKind, File: f}
		},
		Kill: func(_ T, s contract.Session) { fakeadapter.Kill(s) },
		HideBinary: func(t T) func() {
			if err := os.Rename(bin, bin+".hidden"); err != nil {
				t.Errorf("hiding the binary: %v", err)
			}
			return func() { _ = os.Rename(bin+".hidden", bin) }
		},
		Heard: func(_ T, s contract.Session) string { return strings.Join(fakeadapter.Heard(s), "\n") },
		Approve: func(t T, k contract.Keeper, dc contract.DeviceCode) {
			if err := fakeadapter.Approve(k, dc.Code); err != nil {
				t.Errorf("approving the sign-in: %v", err)
			}
		},
		Timeout: 10 * time.Second,
		Quiet:   300 * time.Millisecond,
	}
}

// The fake adapter passes every scenario.
func TestFakeAdapterConforms(t *testing.T) {
	Run(Testing(t), fakeFixture(t, ""))
}

// recorder is a T that records failures instead of failing, so a broken
// adapter's failures can be checked.
type recorder struct {
	t        *testing.T
	mu       sync.Mutex
	failures []string
}

func (r *recorder) Helper()                         {}
func (r *recorder) Logf(format string, args ...any) {}
func (r *recorder) TempDir() string                 { return r.t.TempDir() }
func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *recorder) Run(name string, f func(T)) bool {
	f(r)
	return true
}

// broken maps each rule the fake adapter can break to the kit's rule that
// catches it, and the scenario that holds that rule.
var broken = map[string]struct{ rule, scenario string }{
	"busy-maybe-submitted":        {"send.busy", "send-refused"},
	"unstable-ids":                {"record.redeliver", "record-after-crash"},
	"replay-new-batch":            {"observe.replay", "observe"},
	"interrupt-other-turn":        {"interrupt.other-turn", "interrupt"},
	"two-outcomes":                {"turn.one-outcome", "turn"},
	"recover-not-found":           {"record.crash-mid-turn", "record-crash-mid-turn"},
	"drained-without-acks":        {"close.drained", "close-drain"},
	"no-rescan":                   {"rescan", "rescan"},
	"open-gate":                   {"gate.blocked", "usage-limit"},
	"empty-poll-batch":            {"observe.empty-poll", "observe"},
	"impure-provision":            {"provision.pure", "provision"},
	"fresh-checkpoint":            {"open.fresh-checkpoint", "open"},
	"ack-unknown":                 {"observe.ack-unknown", "observe"},
	"no-turn-started":             {"turn.started-ended", "turn"},
	"drop-oversize":               {"observe.too-large", "observe-size"},
	"interrupt-twice-differs":     {"interrupt.repeat", "interrupt"},
	"answer-after-taken":          {"prompt.gone", "prompts"},
	"no-truncate":                 {"observe.truncated", "observe-size"},
	"no-record-turn-end":          {"record.turn-end", "record-after-crash"},
	"unsent-unknown":              {"record.not-found", "record-after-crash"},
	"no-session-exited":           {"close.drained", "close-drain"},
	"close-differs":               {"close.repeat", "close-drain"},
	"block-on-overloaded":         {"api-error.exhausted", "api-error"},
	"ignore-checkpoint":           {"reopen.no-redelivery", "reopen"},
	"no-retrying":                 {"api-error.recovers", "api-error"},
	"prompt-pending-maybe":        {"prompt.pending", "prompts"},
	"idle-too-late":               {"interrupt.no-turn", "interrupt"},
	"send-after-close":            {"close.send-after", "close-drain"},
	"load-starts-fresh":           {"load.strict", "load-missing"},
	"load-keeps-path":             {"load.record", "load"},
	"load-any-source":             {"load.unsupported", "load-refused"},
	"load-forgets":                {"load.history", "load"},
	"load-drops-goal":             {"load.own-work", "load-autonomous"},
	"auto-unreported":             {"auto.reported", "autonomous"},
	"auto-send-busy":              {"auto.send", "autonomous"},
	"auto-input-id":               {"auto.ids", "autonomous"},
	"auto-interrupt-ignored":      {"auto.interrupt", "autonomous"},
	"auto-restarts":               {"auto.rests", "autonomous"},
	"auto-no-record-end":          {"auto.record", "autonomous"},
	"egress-without-capability":   {"describe.egress", "describe"},
	"impure-placeholder":          {"placeholder.pure", "placeholder"},
	"placeholder-ignores-nonce":   {"placeholder.nonce", "placeholder"},
	"placeholder-holds-secret":    {"placeholder.valid", "placeholder"},
	"placeholder-off-route":       {"placeholder.valid", "placeholder"},
	"keeper-forgets":              {"keeper.persists", "keeper"},
	"keeper-lends-unbrokerable":   {"keeper.lend", "keeper"},
	"keeper-signout-keeps":        {"keeper.sign-out", "keeper"},
	"sessions-without-capability": {"describe.sessions", "describe"},
}

// Every rule the fake adapter can break is caught: the kit fails that rule
// against the broken adapter.
func TestBrokenAdaptersFail(t *testing.T) {
	for _, b := range fakeadapter.Breaks {
		if _, ok := broken[b]; !ok {
			t.Errorf("break %q has no expected rule", b)
		}
	}
	for b, want := range broken {
		t.Run(b, func(t *testing.T) {
			t.Parallel()
			f := fakeFixture(t, b)
			r := &recorder{t: t}
			ran := false
			for _, s := range scenarios {
				if s.name != want.scenario {
					continue
				}
				ran = true
				Run(r, Fixture{
					Adapter: f.Adapter, HarnessRoot: f.HarnessRoot, Spec: f.Spec, Credential: f.Credential, Kill: f.Kill,
					HideBinary: f.HideBinary, Heard: f.Heard, Timeout: 3 * time.Second, Quiet: f.Quiet, Skip: skipAllBut(want.scenario),
					Approve: f.Approve,
				})
			}
			if !ran {
				t.Fatalf("no scenario %q", want.scenario)
			}
			for _, msg := range r.failures {
				if strings.HasPrefix(msg, "["+want.rule+"]") {
					return
				}
			}
			t.Errorf("breaking %q: no [%s] failure; got %q", b, want.rule, r.failures)
		})
	}
}

func skipAllBut(name string) map[string]string {
	skip := map[string]string{}
	for _, s := range Scenarios() {
		if s != name {
			skip[s] = "not under test"
		}
	}
	return skip
}

// A saved Session kept as files loads as one saved a moment ago does: what
// RecordSavedSession makes, written and read back, passes LoadSaved — and
// fails it against an adapter that forgets.
func TestSavedSessionLoads(t *testing.T) {
	f := fakeFixture(t, "")
	r := &recorder{t: t}
	saved, ok := RecordSavedSession(r, f, filepath.Join(t.TempDir(), "source"), nil)
	if !ok || len(r.failures) > 0 {
		t.Fatalf("RecordSavedSession: %v", r.failures)
	}
	dir := t.TempDir()
	if err := WriteSavedSession(dir, saved); err != nil {
		t.Fatal(err)
	}
	read, err := ReadSavedSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if read.SessionID != saved.SessionID || len(read.Files) != len(saved.Files) || len(read.Files) == 0 || read.Source != saved.Source {
		t.Fatalf("read back %+v with %d files, wrote %+v with %d", read.Source, len(read.Files), saved.Source, len(saved.Files))
	}
	for i, file := range read.Files {
		if w := saved.Files[i]; file.At != w.At || file.Mode != w.Mode || string(file.Content) != string(w.Content) {
			t.Errorf("file %d read back as %s %v, wrote %s %v", i, file.At, file.Mode, w.At, w.Mode)
		}
	}
	LoadSaved(Testing(t), f, read)

	forgets := fakeFixture(t, "load-forgets")
	forgets.Timeout = 3 * time.Second
	LoadSaved(r, forgets, read)
	for _, msg := range r.failures {
		if strings.HasPrefix(msg, "[load.history]") {
			return
		}
	}
	t.Errorf("an adapter that forgets the saved conversation: no [load.history] failure; got %q", r.failures)
}
