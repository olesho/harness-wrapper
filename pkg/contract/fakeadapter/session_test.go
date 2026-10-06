package fakeadapter

import (
	"context"
	"errors"
	"os"
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

// A Close that lands after Open has taken the Session's directory, but before
// the Session turns idle, still ends it: Open returns closed and gives the
// directory up, so the Session can be opened again.
func TestCloseJustBeforeIdle(t *testing.T) {
	base := t.TempDir()
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"), Workspace: filepath.Join(base, "workspace"),
		Secrets: filepath.Join(base, "secrets"), Scratch: filepath.Join(base, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(base, "fake")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(l.Secrets, CredentialKind)
	if err := os.WriteFile(cred, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := func(mode contract.OpenMode, id string) contract.OpenRequest {
		return contract.OpenRequest{
			Mode: mode, SessionID: id, Layout: l, OpenConfig: []byte(`{"binary":"` + bin + `"}`),
			Credential: &contract.CredentialFile{Kind: CredentialKind, File: cred},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := New(Options{})
	cs, err := a.NewSession(req(contract.OpenFresh, "fake-race"))
	if err != nil {
		t.Fatal(err)
	}
	s := cs.(*session)
	s.beforeIdle = func() {
		if _, err := s.Close(ctx, contract.ClosePark, 0); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	var ce *contract.Error
	if _, err := s.Open(ctx); !errors.As(err, &ce) || ce.Code != contract.CodeClosed {
		t.Fatalf("Open: %v, want closed", err)
	}
	if p := s.State().Phase; p != contract.PhaseExited {
		t.Fatalf("the Session is %s, want exited", p)
	}

	again, err := a.NewSession(req(contract.OpenReopen, "fake-race"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.Open(ctx); err != nil {
		t.Fatalf("reopening after the closed Open: %v", err)
	}
	if _, err := again.Close(ctx, contract.ClosePark, 0); err != nil {
		t.Fatal(err)
	}
}
