//go:build !linux

package landlock

// ABI reports ErrUnsupported: Landlock is Linux-only.
func ABI() (int, error) { return 0, ErrUnsupported }

// Errata reports 0.
func Errata() int { return 0 }

// Probe reports ErrUnsupported.
func Probe() (int, error) { return 0, ErrUnsupported }

// Ruleset is never created on this platform.
type Ruleset struct{}

// New reports ErrUnsupported.
func New(Config) (*Ruleset, error) { return nil, ErrUnsupported }

// ABI returns 0.
func (*Ruleset) ABI() int { return 0 }

// AddPath reports ErrUnsupported.
func (*Ruleset) AddPath(Rule) error { return ErrUnsupported }

// AddConnectTCP reports ErrUnsupported.
func (*Ruleset) AddConnectTCP(uint16) error { return ErrUnsupported }

// RestrictCurrentThread reports ErrUnsupported.
func (*Ruleset) RestrictCurrentThread() error { return ErrUnsupported }

// FD returns -1.
func (*Ruleset) FD() int { return -1 }

// Close does nothing.
func (*Ruleset) Close() error { return nil }
