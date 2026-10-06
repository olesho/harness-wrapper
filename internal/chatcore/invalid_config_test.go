package chatcore

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// A wrapper.Config that fails wrapper.validateConfig is a caller-supplied
// option problem, not an internal fault: openWithSession classifies it as
// ErrInvalidOptions while keeping wrapper.ErrInvalidConfig matchable through
// the multi-%w wrap, so transports map it to a 4xx and consumers that
// discriminate on the wrapper sentinel keep working.
//
// Effort and Model are the validateConfig arms reachable from here:
// BinaryPath is rejected earlier by openWithSession itself, Stdout is always
// set, and the IdleClassify/StaleThreshold knobs are never populated from
// Options. Both effort arms are covered — the bad enum and the harness that
// has no effort axis — and the harness that has no model flag, which used to
// launch on its default model with the caller's Model silently dropped.
//
// No launchable binary is needed: validateConfig runs inside wrapper.Start
// ahead of startSession, so nothing is exec'd.
func TestOpenClassifiesInvalidWrapperConfigAsInvalidOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		harness string
		effort  string
		model   string
	}{
		{name: "unknown-effort", harness: "codex", effort: "hgih"},
		{name: "harness-without-effort-axis", harness: "opencode", effort: "high"},
		{name: "harness-without-model-flag", harness: "opencode", model: "gpt-5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			conv, err := Open(context.Background(), Options{
				Harness:    tt.harness,
				BinaryPath: os.Args[0],
				Effort:     tt.effort,
				Model:      tt.model,
				Store:      store,
			})
			if conv != nil {
				_ = conv.Close(context.Background())
				t.Fatal("Open returned a conversation for an invalid Effort or Model")
			}
			if !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("Open error = %v, want errors.Is(err, ErrInvalidOptions)", err)
			}
			if !errors.Is(err, wrapper.ErrInvalidConfig) {
				t.Errorf("Open error = %v, want errors.Is(err, wrapper.ErrInvalidConfig)", err)
			}

			// Store.CreateSession runs after wrapper.Start, so the rejected
			// launch must leave no orphan session record behind.
			store.mu.Lock()
			n := len(store.sessions)
			store.mu.Unlock()
			if n != 0 {
				t.Errorf("store holds %d session(s) after a rejected Open, want 0", n)
			}
		})
	}
}
