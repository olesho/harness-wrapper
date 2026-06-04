//go:build screenbench

package emulator

import (
	"io"

	cvt "github.com/charmbracelet/x/vt"
)

func init() {
	Register("charm-x-vt", newCharm)
}

type charmAdapter struct {
	emu *cvt.Emulator
}

func newCharm(cols, rows int) Emulator {
	emu := cvt.NewEmulator(cols, rows)
	// Drain the emulator's host-side reader. The emulator writes
	// responses to host queries (CSI 5n device status, CSI 6n cursor
	// report, DA1, etc.) into this pipe; if nobody reads it, the next
	// Write blocks once the pipe fills.
	go io.Copy(io.Discard, emu)
	return &charmAdapter{emu: emu}
}

func (a *charmAdapter) Write(p []byte) (int, error) { return a.emu.Write(p) }
func (a *charmAdapter) Snapshot() string            { return a.emu.Render() }
func (a *charmAdapter) Cursor() (int, int) {
	p := a.emu.CursorPosition()
	return p.X, p.Y
}
func (a *charmAdapter) Size() (int, int) { return a.emu.Width(), a.emu.Height() }
func (a *charmAdapter) Name() string     { return "charm-x-vt" }
