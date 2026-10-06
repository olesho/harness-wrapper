package fakeadapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A Close that lands while Open waits out its start delay ends the Session
// once: Open returns closed, the Session is exited, and nothing is closed
// twice.
func TestCloseWhileStarting(t *testing.T) {
	base := t.TempDir()
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"), Workspace: filepath.Join(base, "workspace"),
		Secrets: filepath.Join(base, "secrets"), Scratch: filepath.Join(base, "scratch"),
	}
	a := New(Options{StartDelay: time.Second})
	for range 200 {
		s, err := a.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, Layout: l, OpenConfig: []byte(`{"binary":"/nonexistent"}`)})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		opened := make(chan error, 1)
		go func() {
			_, err := s.Open(ctx)
			opened <- err
		}()
		for s.State().Phase != contract.PhaseStarting {
			time.Sleep(50 * time.Microsecond)
		}
		if _, err := s.Close(ctx, contract.ClosePark, 0); err != nil {
			t.Fatalf("Close: %v", err)
		}
		var ce *contract.Error
		if err := <-opened; !errors.As(err, &ce) || ce.Code != contract.CodeClosed {
			t.Fatalf("Open: %v, want closed", err)
		}
		if p := s.State().Phase; p != contract.PhaseExited {
			t.Fatalf("the Session is %s, want exited", p)
		}
		cancel()
	}
}
