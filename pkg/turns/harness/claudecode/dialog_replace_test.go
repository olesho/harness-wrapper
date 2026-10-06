package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// One dialog giving way to another with no dialog-free frame between must
// resolve the first before requesting the second, so the chat layer's pending
// request is never overwritten with the replacement's identity.
func TestOnScreen_DialogReplacedResolvesPrevious(t *testing.T) {
	trust := strings.Join([]string{
		" Quick safety check: " + trustAnchorAlt,
		" ❯ 1. Yes, I trust this folder",
		"   2. No, exit",
	}, "\n")
	bypass := strings.Join([]string{
		" " + bypassAnchor,
		" ❯ 1. No, exit",
		"   2. Yes, I accept",
	}, "\n")

	a := New()
	evs := a.OnScreen(screen.Snapshot{Text: trust})
	if len(evs) != 1 || evs[0].Kind != turns.InputRequested || evs[0].Input.Kind != KindTrustPrompt {
		t.Fatalf("trust frame: %+v", evs)
	}
	evs = a.OnScreen(screen.Snapshot{Text: bypass})
	if len(evs) != 2 || evs[0].Kind != turns.InputResolved || evs[1].Kind != turns.InputRequested {
		t.Fatalf("replacement: %+v, want [InputResolved InputRequested]", evs)
	}
	if evs[0].Input.Kind != KindTrustPrompt || evs[0].Reason != "claude-code: input resolved" {
		t.Errorf("resolved %+v (%q), want the trust dialog", evs[0].Input, evs[0].Reason)
	}
	if evs[1].Input.Kind != KindBypassAcceptance {
		t.Errorf("requested %+v, want the bypass dialog", evs[1].Input)
	}
}
