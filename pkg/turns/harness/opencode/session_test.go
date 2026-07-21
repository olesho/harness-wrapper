package opencode

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The opencode adapter must NOT implement turns.SessionResumer or
// turns.SessionControlFlags: its absence of SessionResumer is exactly what makes
// chat return ErrResumeUnsupported for opencode (mirroring
// meta-harness/test/chat/resume.test.ts). A future accidental implementation
// would silently make opencode resumable — catch it here.
func TestOpencodeNotSessionCapabilities(t *testing.T) {
	if _, ok := interface{}(New()).(turns.SessionResumer); ok {
		t.Error("opencode adapter must NOT implement turns.SessionResumer")
	}
	if _, ok := interface{}(New()).(turns.SessionControlFlags); ok {
		t.Error("opencode adapter must NOT implement turns.SessionControlFlags")
	}
}
