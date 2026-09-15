package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// portList is a repeatable TCP port flag.
type portList []uint16

func (l *portList) String() string {
	parts := make([]string, len(*l))
	for i, p := range *l {
		parts[i] = strconv.Itoa(int(p))
	}
	return strings.Join(parts, ",")
}

func (l *portList) Set(v string) error {
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("invalid TCP port %q", v)
	}
	*l = append(*l, uint16(n))
	return nil
}

// containFlags is the --contain* surface: an optional Landlock boundary around
// the harness (Linux). --contain selects it; every other --contain-* flag
// refines it and is invalid without it. Value validation (kind, paths,
// minimum ABI, environment names) belongs to pkg/wrapper, in one place for
// every entry point, exactly as for --permission-mode.
type containFlags struct {
	Kind        string
	ReadWrite   stringList
	ReadOnly    stringList
	RestrictTCP bool
	AllowTCP    portList
	MinABI      int
	StateDir    string
	PassEnv     stringList
}

// any reports whether any --contain* flag was given.
func (c containFlags) any() bool {
	return c.Kind != "" || len(c.ReadWrite) > 0 || len(c.ReadOnly) > 0 || c.RestrictTCP ||
		len(c.AllowTCP) > 0 || c.MinABI != 0 || c.StateDir != "" || len(c.PassEnv) > 0
}

// validate rejects refinements without --contain: a --contain-rw with no
// containment would read as a boundary that is not there.
func (c containFlags) validate() error {
	if c.Kind == "" && c.any() {
		return fmt.Errorf("harness-wrapper: --contain-* flags require --contain %s", wrapper.ContainmentLandlock)
	}
	return nil
}

// request builds the wrapper request, nil when --contain was not given.
func (c containFlags) request() *wrapper.Containment {
	if c.Kind == "" {
		return nil
	}
	return &wrapper.Containment{
		Kind:        c.Kind,
		ReadOnly:    append([]string(nil), c.ReadOnly...),
		ReadWrite:   append([]string(nil), c.ReadWrite...),
		RestrictTCP: c.RestrictTCP,
		ConnectTCP:  append([]uint16(nil), c.AllowTCP...),
		MinABI:      c.MinABI,
		StateDir:    c.StateDir,
		PassEnv:     append([]string(nil), c.PassEnv...),
	}
}
