package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// testAgent is an agent provisioned for the fake profile.
type testAgent struct {
	a      contract.Adapter
	p      *fakeProfile
	layout contract.Layout
	oc     []byte
	bin    string // the harness binary
}

// fakeAgent provisions an agent for the fake profile under a temporary
// directory.
func fakeAgent(t *testing.T) testAgent {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(fakeBinary(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeBinary(root), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"),
		Workspace: filepath.Join(base, "workspace"), Secrets: filepath.Join(base, "secrets"),
		Scratch: filepath.Join(base, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := newFakeProfile()
	a := New(p)
	res, err := a.Provision(contract.ProvisionRequest{
		Contract: contract.Version, HarnessRoot: root, Layout: l,
		Spec: contract.AgentSpec{PermissionPosture: contract.PostureBypass},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conformance.Apply(l, res); err != nil {
		t.Fatal(err)
	}
	return testAgent{a: a, p: p, layout: l, oc: res.OpenConfig, bin: fakeBinary(root)}
}

func openFailure(err error) contract.OpenFailure {
	var e *contract.Error
	if errors.As(err, &e) && e.Code == contract.CodeOpenFailed {
		return e.Reason
	}
	return ""
}

// A Session whose harness runs in one Host is refused to another, before a
// second harness starts, and opens there once the first has exited; an Open
// that failed holds nothing.
func TestOneHostPerSession(t *testing.T) {
	ag := fakeAgent(t)
	a, p := ag.a, ag.p
	ctx := context.Background()
	first, err := a.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, OpenConfig: ag.oc, Layout: ag.layout})
	if err != nil {
		t.Fatal(err)
	}
	res, err := first.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reopen := func() (contract.Session, error) {
		s, err := a.NewSession(contract.OpenRequest{Mode: contract.OpenReopen, SessionID: res.SessionID, OpenConfig: ag.oc, Layout: ag.layout})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Open(ctx)
		return s, err
	}
	p.mu.Lock()
	harness := p.procs[res.SessionID]
	p.mu.Unlock()
	if _, err := reopen(); openFailure(err) != contract.OpenSessionInUse {
		t.Fatalf("reopening a Session another Host runs: %v, want open_failed session_in_use", err)
	}
	p.mu.Lock()
	again := p.procs[res.SessionID]
	p.mu.Unlock()
	if again != harness {
		t.Error("the refused open started a harness")
	}
	if _, err := first.Close(ctx, contract.ClosePark, time.Second); err != nil {
		t.Fatal(err)
	}
	// A harness gone missing fails the open, which leaves the Session free.
	if err := os.Rename(ag.bin, ag.bin+".hidden"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopen(); openFailure(err) != contract.OpenBinaryNotFound {
		t.Fatalf("reopening with no harness binary: %v, want open_failed binary_not_found", err)
	}
	if err := os.Rename(ag.bin+".hidden", ag.bin); err != nil {
		t.Fatal(err)
	}
	second, err := reopen()
	if err != nil {
		t.Fatalf("reopening once the first harness exited: %v", err)
	}
	if _, err := second.Close(ctx, contract.ClosePark, time.Second); err != nil {
		t.Fatal(err)
	}
}
