package claudecode

import (
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// Compile-time assertions that the adapter advertises the session capabilities.
var (
	_ turns.SessionResumer      = (*Adapter)(nil)
	_ turns.SessionControlFlags = (*Adapter)(nil)
)

func TestClaudeCodeResumeArgs(t *testing.T) {
	got := New().ResumeArgs("abc-123")
	want := []string{"--resume", "abc-123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResumeArgs = %q, want %q", got, want)
	}
}

func TestClaudeCodeSessionControlFlags(t *testing.T) {
	got := New().SessionControlFlags()
	want := []string{
		"--session-id",
		"-r",
		"--resume",
		"-c",
		"--continue",
		"--fork-session",
		"--from-pr",
		"--no-session-persistence",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionControlFlags = %q, want %q", got, want)
	}
}
