package emulator

import (
	"github.com/hinshun/vt10x"
)

func init() {
	Register("vt10x", newVT10x)
}

type vt10xAdapter struct {
	term vt10x.Terminal
}

func newVT10x(cols, rows int) Emulator {
	return &vt10xAdapter{term: vt10x.New(vt10x.WithSize(cols, rows))}
}

func (a *vt10xAdapter) Write(p []byte) (int, error) { return a.term.Write(p) }

func (a *vt10xAdapter) Snapshot() string { return a.term.String() }

func (a *vt10xAdapter) Cursor() (int, int) {
	c := a.term.Cursor()
	return c.X, c.Y
}

func (a *vt10xAdapter) Size() (int, int) { return a.term.Size() }

func (a *vt10xAdapter) Name() string { return "vt10x" }
