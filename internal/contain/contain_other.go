//go:build !linux

package contain

import (
	"context"
	"time"

	"github.com/olesho/harness-wrapper/pkg/containment"
)

// State is never created on this platform.
type State struct {
	ID         string
	Persistent bool
}

// StateParent reports ErrUnsupported.
func StateParent() (string, error) { return "", ErrUnsupported }

// NewState reports ErrUnsupported.
func NewState(bool) (*State, error) { return nil, ErrUnsupported }

// OpenState reports ErrUnsupported.
func OpenState(string) (*State, error) { return nil, ErrUnsupported }

// Root returns "".
func (*State) Root() string { return "" }

// Remove reports ErrUnsupported.
func (*State) Remove(context.Context) error { return ErrUnsupported }

// Close does nothing.
func (*State) Close() {}

// Launch is never prepared on this platform.
type Launch struct{}

// Prepare reports ErrUnsupported: Landlock containment is Linux-only.
func Prepare(Input) (*Launch, error) { return nil, ErrUnsupported }

// AddTerminal reports ErrUnsupported.
func (*Launch) AddTerminal(int) error { return ErrUnsupported }

// Applied returns nil.
func (*Launch) Applied() *containment.Applied { return nil }

// Supervised reports false.
func (*Launch) Supervised() bool { return false }

// Start reports ErrUnsupported.
func (*Launch) Start(int) (int, error) { return 0, ErrUnsupported }

// Release does nothing.
func (*Launch) Release() {}

// Finish does nothing.
func (*Launch) Finish(int, bool, time.Time) string { return "" }

// OpenPTYPair reports ErrUnsupported.
func OpenPTYPair() (int, int, error) { return -1, -1, ErrUnsupported }

// PreviewLaunch reports ErrUnsupported.
func PreviewLaunch(Input) (*Preview, error) { return nil, ErrUnsupported }

// DisableSupervisionForTest does nothing here.
func DisableSupervisionForTest() func() { return func() {} }
