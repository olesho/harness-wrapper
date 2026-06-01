package codex

import (
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

func TestResumeArgs(t *testing.T) {
	if got := (resumer{}).ResumeArgs("sess-1"); !reflect.DeepEqual(got, []string{"resume", "sess-1"}) {
		t.Errorf("ResumeArgs = %v, want [resume sess-1] (codex exec resume <id>)", got)
	}
	if got := (resumer{}).ResumeArgs(""); got != nil {
		t.Errorf("ResumeArgs(\"\") = %v, want nil", got)
	}
}

func TestResolveResumeOnly(t *testing.T) {
	rp := Profile{}.Resolve(harness.ResolveContext{})
	if rp.Resume == nil {
		t.Fatal("codex must register Resume")
	}
	// Everything else is deferred (under-documented / unverifiable) — must be nil
	// so the orchestrator never acts on a guess (never installs codex hooks, etc.).
	if rp.Hooks != nil || rp.Stream != nil || rp.SessionID != nil {
		t.Errorf("codex must register ONLY Resume; got Hooks=%v Stream=%v SessionID=%v",
			rp.Hooks != nil, rp.Stream != nil, rp.SessionID != nil)
	}
}

func TestRegisteredViaInit(t *testing.T) {
	p, ok := harness.For("codex")
	if !ok || p.Name() != "codex" {
		t.Fatalf("codex not registered: ok=%v", ok)
	}
}
