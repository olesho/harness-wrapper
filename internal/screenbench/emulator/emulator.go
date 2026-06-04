//go:build screenbench

// Package emulator defines a thin adapter interface around candidate
// vt100 emulator libraries so the bake-off harness can compare them on
// equal footing. Each adapter wraps one upstream library and exposes
// only what the bench needs: feed bytes, then read a text snapshot of
// the resulting screen state.
package emulator

import "io"

// Emulator is the minimal surface the bake-off cares about.
//
// Write feeds raw PTY bytes (ANSI escapes intact) into the emulator.
// Snapshot returns the current visible-screen contents as a plain-text
// rendering, with trailing whitespace per line preserved (callers
// normalize for comparisons). Cursor returns the 0-indexed cursor
// position (col, row). Size returns the terminal dimensions.
type Emulator interface {
	io.Writer
	Snapshot() string
	Cursor() (col, row int)
	Size() (cols, rows int)
	Name() string
}

// Factory constructs an Emulator at a given terminal size.
type Factory func(cols, rows int) Emulator

// Registry of available emulator factories. Adapters register
// themselves via init().
var Registry = map[string]Factory{}

func Register(name string, f Factory) { Registry[name] = f }

// Names returns the registered emulator names in a deterministic order.
func Names() []string {
	out := make([]string, 0, len(Registry))
	for n := range Registry {
		out = append(out, n)
	}
	// Simple sort to keep report output stable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
