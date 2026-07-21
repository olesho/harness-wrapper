package pi

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

func TestPiResumeArgs(t *testing.T) {
	got := New().ResumeArgs("abc-123")
	want := []string{"--session", "abc-123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResumeArgs = %q, want %q", got, want)
	}
}

func TestPiSessionControlFlags(t *testing.T) {
	got := New().SessionControlFlags()
	want := []string{
		"--session",
		"--session-id",
		"--fork",
		"-c",
		"--continue",
		"-r",
		"--resume",
		"--no-session",
		"--session-dir",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionControlFlags = %q, want %q", got, want)
	}
}
