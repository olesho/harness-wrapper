package codex

import (
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// Compile-time assertion that the adapter is a SessionResumer.
var _ turns.SessionResumer = (*Adapter)(nil)

func TestCodexResumeArgs(t *testing.T) {
	got := New().ResumeArgs("abc-123")
	want := []string{"resume", "abc-123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResumeArgs = %q, want %q", got, want)
	}
}

// Codex deliberately does NOT implement turns.SessionControlFlags — mirroring
// codex.ts, which declares no session-control flags so chat accepts a
// caller-supplied resume for codex. Guard against an accidental future impl.
func TestCodexNotSessionControlFlags(t *testing.T) {
	if _, ok := interface{}(New()).(turns.SessionControlFlags); ok {
		t.Error("codex adapter must NOT implement turns.SessionControlFlags")
	}
}
