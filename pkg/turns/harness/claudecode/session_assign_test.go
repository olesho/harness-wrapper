package claudecode

import (
	"slices"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

var _ turns.SessionAssigner = (*Adapter)(nil)

// TestSessionAssigner pins the id form and argv claude accepts for a fresh
// session: a UUID, as --session-id <uuid>.
func TestSessionAssigner(t *testing.T) {
	a := New()
	id := a.NewSessionID()
	if err := a.ValidSessionID(id); err != nil {
		t.Fatalf("minted id %q is not valid: %v", id, err)
	}
	if other := a.NewSessionID(); other == id {
		t.Fatalf("NewSessionID repeated %q", id)
	}
	for _, bad := range []string{"", "not-a-uuid", "123e4567e89b12d3a456426614174000", "123e4567-e89b-12d3-a456-42661417400"} {
		if a.ValidSessionID(bad) == nil {
			t.Errorf("ValidSessionID(%q) = nil, want an error", bad)
		}
	}
	if got, want := a.SessionIDArgs(id), []string{"--session-id", id}; !slices.Equal(got, want) {
		t.Fatalf("SessionIDArgs = %q, want %q", got, want)
	}
	if !slices.Contains(a.SessionControlFlags(), "--session-id") {
		t.Fatal("--session-id must stay a chat-managed flag")
	}
}
