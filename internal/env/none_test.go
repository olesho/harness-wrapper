package env

import (
	"context"
	"testing"
)

func TestNoneIsIdentity(t *testing.T) {
	n := None()
	if n.Name() != "none" {
		t.Fatalf("name = %q", n.Name())
	}
	if err := n.Preflight(context.Background(), &fakeWorkspace{}); err != nil {
		t.Fatalf("none preflight should never fail: %v", err)
	}

	layer := n.Layer(PolicySpec{})
	// Degenerate primitives.
	if got := layer.CrossUpload("a", "b"); len(got) != 0 {
		t.Fatalf("CrossUpload should be empty: %v", got)
	}
	if got := layer.CrossDownload("a", "b"); len(got) != 0 {
		t.Fatalf("CrossDownload should be empty: %v", got)
	}
	if got := layer.PathMap(PathRepo); got != "" {
		t.Fatalf("PathMap should defer (empty): %q", got)
	}
	if got := layer.Teardown(); len(got) != 0 {
		t.Fatalf("Teardown should be empty: %v", got)
	}
	argv := []string{"echo", "hi"}
	opts := ExecOpts{Cwd: "/x"}
	gotArgv, gotOpts := layer.ExecWrap(argv, opts)
	if len(gotArgv) != 2 || gotArgv[0] != "echo" || gotArgv[1] != "hi" || gotOpts.Cwd != "/x" {
		t.Fatalf("ExecWrap should be identity: %v %+v", gotArgv, gotOpts)
	}

	// None advertises no AliasMapper, so a composed workspace defers to inner.
	if _, ok := layer.(AliasMapper); ok {
		t.Fatal("none layer must not implement AliasMapper")
	}
}

func TestComposeWithNoneEqualsInner(t *testing.T) {
	inner := &fakeWorkspace{execResult: ExecResult{Code: 0, Stdout: "ok"}}
	ws := Compose(inner, None().Layer(PolicySpec{}))

	// exec passes through unchanged.
	if _, err := ws.Exec(context.Background(), []string{"cmd", "a"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(inner.execCalls) != 1 || inner.execCalls[0][0] != "cmd" || inner.execCalls[0][1] != "a" {
		t.Fatalf("none should not wrap exec: %v", inner.execCalls)
	}
	// upload goes straight to the final path (no staging, no cross exec).
	if err := ws.Upload(context.Background(), "/h", "/g"); err != nil {
		t.Fatal(err)
	}
	if len(inner.uploads) != 1 || inner.uploads[0] != [2]string{"/h", "/g"} {
		t.Fatalf("none upload should be direct: %v", inner.uploads)
	}
	if len(inner.execCalls) != 1 {
		t.Fatalf("none upload must not exec a cross command: %v", inner.execCalls)
	}
	// guestPath defers to inner.
	if got := ws.GuestPath(PathRepo); got != "/inner/repo" {
		t.Fatalf("none guestPath should defer to inner: %q", got)
	}
	// hostAlias defers to inner.
	if got := ws.HostAlias("http://h"); got != "inner(http://h)" {
		t.Fatalf("none hostAlias should defer to inner: %q", got)
	}
}
