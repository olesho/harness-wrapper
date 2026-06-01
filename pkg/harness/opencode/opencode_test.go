package opencode

import (
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

func TestResumeArgs(t *testing.T) {
	if got := (resumer{}).ResumeArgs("s9"); !reflect.DeepEqual(got, []string{"--session", "s9"}) {
		t.Errorf("ResumeArgs = %v, want [--session s9]", got)
	}
	if got := (resumer{}).ResumeArgs(""); got != nil {
		t.Errorf("ResumeArgs(\"\") = %v, want nil", got)
	}
}

func TestExtractSessionID(t *testing.T) {
	ext := sessionIDExtractor{}
	cases := []struct {
		name, line, want string
		ok               bool
	}{
		{"camelCase sessionID", `{"type":"text","sessionID":"abc","text":"hi"}`, "abc", true},
		{"snake_case fallback", `{"type":"step_start","session_id":"xyz"}`, "xyz", true},
		{"no session field", `{"type":"text","text":"hi"}`, "", false},
		{"non-JSON / ANSI noise", "\x1b[2mthinking\x1b[0m", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ext.ExtractSessionID(c.line)
			if got != c.want || ok != c.ok {
				t.Errorf("ExtractSessionID(%q) = (%q,%v), want (%q,%v)", c.line, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestResolveCapabilities(t *testing.T) {
	rp := Profile{}.Resolve(harness.ResolveContext{})
	if rp.Resume == nil || rp.SessionID == nil {
		t.Fatal("opencode must register Resume + SessionID")
	}
	// OpenCode has no shell hooks (correct, not a gap); Stream/export deferred.
	if rp.Hooks != nil || rp.Stream != nil {
		t.Errorf("opencode must NOT register Hooks/Stream; got Hooks=%v Stream=%v", rp.Hooks != nil, rp.Stream != nil)
	}
}

func TestRegisteredViaInit(t *testing.T) {
	p, ok := harness.For("opencode")
	if !ok || p.Name() != "opencode" {
		t.Fatalf("opencode not registered: ok=%v", ok)
	}
}
