package harnesscore

import "testing"

type plainProfile struct{ name string }

func (p plainProfile) Name() string                         { return p.name }
func (plainProfile) Resolve(ResolveContext) ResolvedProfile { return ResolvedProfile{} }

type namedBinaryProfile struct {
	plainProfile
	bin string
}

func (p namedBinaryProfile) BinaryName() string { return p.bin }

func TestBinaryName(t *testing.T) {
	if got := BinaryName(plainProfile{"opencode"}); got != "opencode" {
		t.Errorf("no BinaryNamer: got %q, want the profile name", got)
	}
	if got := BinaryName(namedBinaryProfile{plainProfile{"x"}, "x-cli"}); got != "x-cli" {
		t.Errorf("BinaryNamer: got %q, want %q", got, "x-cli")
	}
	if got := BinaryName(namedBinaryProfile{plainProfile{"y"}, ""}); got != "y" {
		t.Errorf("empty BinaryName: got %q, want the profile name", got)
	}
}
