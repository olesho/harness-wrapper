//go:build !linux

package wrapper

import (
	"io"
	"os"
)

// masterReader exists for the build only: containment is refused on every
// platform but Linux before a session starts.
type masterReader struct{ io.Reader }

func openWake() (int, error) { return -1, ErrContainmentUnsupported }

func newMasterReader(f *os.File, _ int) (*masterReader, error) { return &masterReader{f}, nil }

func (r *masterReader) stop() {}

func (r *masterReader) close() {}
