package env

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newLocalWorkspace(t *testing.T, spec WorkspaceSpec) (Provisioner, Workspace, string) {
	t.Helper()
	root := t.TempDir()
	p := Local(root)
	if p.Name() != "local" {
		t.Fatalf("name = %q", p.Name())
	}
	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	ws, err := p.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return p, ws, root
}

func TestLocalCreateMakesSubdirs(t *testing.T) {
	_, ws, root := newLocalWorkspace(t, WorkspaceSpec{Name: "run1"})
	base := filepath.Join(root, "run1")
	for kind, sub := range map[PathKind]string{PathRepo: "repo", PathHome: ".home", PathTmp: "tmp"} {
		if got := ws.GuestPath(kind); got != filepath.Join(base, sub) {
			t.Fatalf("GuestPath(%s) = %q", kind, got)
		}
		if fi, err := os.Stat(filepath.Join(base, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("subdir %s not created: %v", sub, err)
		}
	}
}

func TestLocalExec(t *testing.T) {
	_, ws, _ := newLocalWorkspace(t, WorkspaceSpec{Name: "run-exec"})
	ctx := context.Background()

	t.Run("stdout and exit code", func(t *testing.T) { execStdoutAndExitCode(t, ws, ctx) })
	t.Run("non-zero exit resolves with code", func(t *testing.T) { execNonZeroExit(t, ws, ctx) })
	t.Run("empty argv errors", func(t *testing.T) { execEmptyArgv(t, ws, ctx) })
	t.Run("env overlay crosses", func(t *testing.T) { execEnvOverlay(t, ws, ctx) })
	t.Run("stdin is delivered", func(t *testing.T) { execStdinDelivered(t, ws, ctx) })
	t.Run("cancelled context errors", func(t *testing.T) { execCancelledContext(t, ws, ctx) })
}

func execStdoutAndExitCode(t *testing.T, ws Workspace, ctx context.Context) {
	res, err := ws.Exec(ctx, []string{"sh", "-c", "echo hello; exit 0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 0 || res.Stdout != "hello\n" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func execNonZeroExit(t *testing.T, ws Workspace, ctx context.Context) {
	res, err := ws.Exec(ctx, []string{"sh", "-c", "exit 3"}, nil)
	if err != nil {
		t.Fatalf("non-zero exit should not error: %v", err)
	}
	if res.Code != 3 {
		t.Fatalf("code = %d, want 3", res.Code)
	}
}

func execEmptyArgv(t *testing.T, ws Workspace, ctx context.Context) {
	if _, err := ws.Exec(ctx, nil, nil); err == nil {
		t.Fatal("expected error on empty argv")
	}
}

func execEnvOverlay(t *testing.T, ws Workspace, ctx context.Context) {
	res, err := ws.Exec(ctx, []string{"sh", "-c", "echo $FOO"}, &ExecOpts{Env: map[string]string{"FOO": "bar"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "bar\n" {
		t.Fatalf("env not crossed: %q", res.Stdout)
	}
}

func execStdinDelivered(t *testing.T, ws Workspace, ctx context.Context) {
	in := "piped-input"
	res, err := ws.Exec(ctx, []string{"cat"}, &ExecOpts{Stdin: &in})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != in {
		t.Fatalf("stdin not delivered: %q", res.Stdout)
	}
}

func execCancelledContext(t *testing.T, ws Workspace, ctx context.Context) {
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ws.Exec(cctx, []string{"sh", "-c", "sleep 5"}, nil); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestLocalUploadDownload(t *testing.T) {
	_, ws, _ := newLocalWorkspace(t, WorkspaceSpec{Name: "run-io"})
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "src.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	guest := filepath.Join(ws.GuestPath(PathRepo), "nested", "dst.txt")
	if err := ws.Upload(ctx, src, guest); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// Parent dir was created and mode bits preserved.
	b, err := os.ReadFile(guest)
	if err != nil || string(b) != "payload" {
		t.Fatalf("uploaded content wrong: %q err=%v", b, err)
	}
	if fi, _ := os.Stat(guest); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode not preserved: %v", fi.Mode().Perm())
	}

	back := filepath.Join(t.TempDir(), "back", "out.txt")
	if err := ws.Download(ctx, guest, back); err != nil {
		t.Fatalf("download: %v", err)
	}
	if b, _ := os.ReadFile(back); string(b) != "payload" {
		t.Fatalf("downloaded content wrong: %q", b)
	}
}

func TestLocalDestroyRetention(t *testing.T) {
	cases := []struct {
		name      string
		retention Retention
		outcome   Outcome
		wantKept  bool
	}{
		{"absent-success destroys", "", OutcomeSuccess, false},
		{"always-success keeps", RetentionAlways, OutcomeSuccess, true},
		{"keep-on-failure success destroys", RetentionKeepOnFailure, OutcomeSuccess, false},
		{"keep-on-failure failure keeps", RetentionKeepOnFailure, OutcomeFailure, true},
		{"always but setup-failure destroys", RetentionAlways, OutcomeSetupFailure, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ws, root := newLocalWorkspace(t, WorkspaceSpec{Name: "run-ret", Retention: c.retention})
			base := filepath.Join(root, "run-ret")
			if err := ws.Destroy(context.Background(), c.outcome); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(base)
			kept := statErr == nil
			if kept != c.wantKept {
				t.Fatalf("kept=%v, want %v", kept, c.wantKept)
			}
		})
	}
}

func TestLocalDestroyIdempotent(t *testing.T) {
	_, ws, _ := newLocalWorkspace(t, WorkspaceSpec{Name: "run-idem"})
	ctx := context.Background()
	if err := ws.Destroy(ctx, OutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	// Second destroy is a no-op, not an error.
	if err := ws.Destroy(ctx, OutcomeSuccess); err != nil {
		t.Fatalf("double destroy should be a no-op: %v", err)
	}
}

func TestLocalCreateCrashRecovery(t *testing.T) {
	root := t.TempDir()
	p := Local(root)
	ctx := context.Background()
	if err := p.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	ws1, err := p.Create(ctx, WorkspaceSpec{Name: "same"})
	if err != nil {
		t.Fatal(err)
	}
	// Drop a leftover file in the old workspace.
	leftover := filepath.Join(ws1.GuestPath(PathRepo), "leftover")
	if err := os.WriteFile(leftover, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Recreating under the SAME deterministic name wipes the leftover.
	if _, err := p.Create(ctx, WorkspaceSpec{Name: "same"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("leftover from crashed run not removed: %v", err)
	}
}

func TestLocalHostAliasIdentity(t *testing.T) {
	_, ws, _ := newLocalWorkspace(t, WorkspaceSpec{Name: "run-alias"})
	if got := ws.HostAlias("http://localhost:8080"); got != "http://localhost:8080" {
		t.Fatalf("local host alias should be identity: %q", got)
	}
}
