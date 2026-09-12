//go:build !unix

package wrapper

import (
	"errors"
	"os"
	"syscall"
)

// signalProcessGroup signals p alone on platforms with no POSIX process
// groups. Behaviour here is exactly what the wrapper did on every platform
// before group-scoped termination; the unix build gets the group semantics.
// Keeping the same signature on both sides is what keeps session.go free of
// build tags.
func signalProcessGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	if err := p.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
