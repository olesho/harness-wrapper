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
		Timeout: 10 * time.Second,
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
	"busy-maybe-submitted":    {"send.busy", "send-refused"},
	"unstable-ids":            {"record.redeliver", "record-after-crash"},
	"replay-new-batch":        {"observe.replay", "observe"},
	"interrupt-other-turn":    {"interrupt.other-turn", "interrupt"},
	"two-outcomes":            {"turn.one-outcome", "turn"},
	"recover-not-found":       {"record.crash-mid-turn", "record-crash-mid-turn"},
	"drained-without-acks":    {"close.drained", "close-drain"},
	"no-rescan":               {"rescan", "rescan"},
	"open-gate":               {"gate.blocked", "usage-limit"},
	"empty-poll-batch":        {"observe.empty-poll", "observe"},
	"impure-provision":        {"provision.pure", "provision"},
	"fresh-checkpoint":        {"open.fresh-checkpoint", "open"},
	"ack-unknown":             {"observe.ack-unknown", "observe"},
	"no-turn-started":         {"turn.started-ended", "turn"},
	"drop-oversize":           {"observe.too-large", "observe-size"},
	"interrupt-twice-differs": {"interrupt.repeat", "interrupt"},
	"answer-after-taken":      {"prompt.gone", "prompts"},
	"no-truncate":             {"observe.truncated", "observe-size"},
	"no-record-turn-end":      {"record.turn-end", "record-after-crash"},
	"unsent-unknown":          {"record.not-found", "record-after-crash"},
	"no-session-exited":       {"close.drained", "close-drain"},
	"close-differs":           {"close.repeat", "close-drain"},
	"block-on-overloaded":     {"api-error.exhausted", "api-error"},
	"ignore-checkpoint":       {"reopen.no-redelivery", "reopen"},
	"no-retrying":             {"api-error.recovers", "api-error"},
	"prompt-pending-maybe":    {"prompt.pending", "prompts"},
	"idle-too-late":           {"interrupt.no-turn", "interrupt"},
	"send-after-close":        {"close.send-after", "close-drain"},
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
					HideBinary: f.HideBinary, Timeout: 3 * time.Second, Skip: skipAllBut(want.scenario),
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
