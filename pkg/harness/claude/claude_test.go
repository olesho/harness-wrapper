package claude

import (
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

func TestExtractSessionID(t *testing.T) {
	ext := sessionIDExtractor{}
	cases := []struct {
		name   string
		line   string
		want   string
		wantOK bool
	}{
		{
			name:   "system init carries session_id",
			line:   `{"type":"system","subtype":"init","session_id":"019e2824-db19-72b2-bd4a-d5a5d80f74f0","cwd":"/x"}`,
			want:   "019e2824-db19-72b2-bd4a-d5a5d80f74f0",
			wantOK: true,
		},
		{
			name:   "assistant line is not init",
			line:   `{"type":"assistant","message":{"id":"m1"}}`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "system but wrong subtype",
			line:   `{"type":"system","subtype":"status","session_id":"x"}`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "init with empty session_id",
			line:   `{"type":"system","subtype":"init","session_id":""}`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "non-JSON / ANSI-polluted line is skipped",
			line:   "\x1b[2m> some TUI noise\x1b[0m",
			want:   "",
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			want:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ext.ExtractSessionID(tc.line)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("ExtractSessionID(%q) = (%q,%v), want (%q,%v)", tc.line, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestResumeArgs(t *testing.T) {
	r := resumer{}
	got := r.ResumeArgs("abc-123")
	// Positional `--resume <id>` — NOT `--resume --session-id <id>`, which claude
	// rejects unless --fork-session is also set (regression guard).
	want := []string{"--resume", "abc-123"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResumeArgs = %v, want %v", got, want)
	}
	if got := r.ResumeArgs(""); got != nil {
		t.Fatalf("ResumeArgs(\"\") = %v, want nil (cold start)", got)
	}
}

func TestResolvePopulatesResumeCaps(t *testing.T) {
	rp := Profile{}.Resolve(harness.ResolveContext{})
	if rp.SessionID == nil {
		t.Fatal("Resolve: SessionID capability must be non-nil for claude")
	}
	if rp.Resume == nil {
		t.Fatal("Resolve: Resume capability must be non-nil for claude")
	}
	if id, ok := rp.SessionID.ExtractSessionID(`{"type":"system","subtype":"init","session_id":"s"}`); !ok || id != "s" {
		t.Fatalf("resolved SessionID.ExtractSessionID = (%q,%v)", id, ok)
	}
}

func TestRegisteredViaInit(t *testing.T) {
	p, ok := harness.For("claude")
	if !ok {
		t.Fatal("harness.For(\"claude\") not registered (init did not run?)")
	}
	if p.Name() != "claude" {
		t.Fatalf("profile name = %q, want claude", p.Name())
	}
}
