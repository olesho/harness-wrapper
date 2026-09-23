package pi

import (
	"slices"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

var _ turns.SessionAssigner = (*Adapter)(nil)

// TestSessionAssigner pins the id form and argv pi is launched with for a fresh
// session: a UUID, as --session-id <uuid>.
func TestSessionAssigner(t *testing.T) {
	a := New()
	id := a.NewSessionID()
	if err := a.ValidSessionID(id); err != nil {
		t.Fatalf("minted id %q is not valid: %v", id, err)
	}
	if a.ValidSessionID("session-1") == nil {
		t.Error("ValidSessionID accepted a non-UUID")
	}
	if got, want := a.SessionIDArgs(id), []string{"--session-id", id}; !slices.Equal(got, want) {
		t.Fatalf("SessionIDArgs = %q, want %q", got, want)
	}
}
