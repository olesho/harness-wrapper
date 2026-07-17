package daytona

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/env"
)

// --- fakes -------------------------------------------------------------------

// fakeSandbox is an injectable DaytonaSandbox for unit tests.
type fakeSandbox struct {
	id        string
	sandboxID string
	labels    map[string]string

	deleted   int
	deleteErr error

	uploads   map[string][]byte
	downloads map[string][]byte

	// execFn, when set, handles ExecuteCommand; otherwise a zero result is
	// returned (exit 0, empty output).
	execFn func(command, cwd string, envv map[string]string) (ExecCommandResult, error)
}

func (s *fakeSandbox) ID() string                { return s.id }
func (s *fakeSandbox) SandboxID() string         { return s.sandboxID }
func (s *fakeSandbox) Labels() map[string]string { return s.labels }
func (s *fakeSandbox) Delete(_ context.Context, _ int) error {
	s.deleted++
	return s.deleteErr
}

func (s *fakeSandbox) ExecuteCommand(_ context.Context, command, cwd string, envv map[string]string, _ *int) (ExecCommandResult, error) {
	if s.execFn != nil {
		return s.execFn(command, cwd, envv)
	}
	return ExecCommandResult{Result: "", ExitCode: 0}, nil
}

func (s *fakeSandbox) UploadFile(_ context.Context, buffer []byte, filePath string) error {
	if s.uploads == nil {
		s.uploads = map[string][]byte{}
	}
	s.uploads[filePath] = append([]byte(nil), buffer...)
	return nil
}

func (s *fakeSandbox) DownloadFile(_ context.Context, filePath string) ([]byte, error) {
	if b, ok := s.downloads[filePath]; ok {
		return b, nil
	}
	return nil, nil
}

// fakeClient is an injectable DaytonaSdkClient.
type fakeClient struct {
	sandboxes []DaytonaSandbox
	created   []CreateOpts
	toCreate  DaytonaSandbox
}

func (c *fakeClient) Create(_ context.Context, opts CreateOpts) (DaytonaSandbox, error) {
	c.created = append(c.created, opts)
	if c.toCreate != nil {
		return c.toCreate, nil
	}
	return &fakeSandbox{id: "created"}, nil
}

func (c *fakeClient) List(_ context.Context, _ ListQuery) ([]DaytonaSandbox, error) {
	return c.sandboxes, nil
}

// recordingWorkspace is an injectable env.Workspace that records Upload (with
// the host file's mode) and Exec calls — enough to verify the credential
// injector's chmod and cleanup behavior without a live sandbox.
type recordingWorkspace struct {
	uploads  []recordedUpload
	execArgs [][]string
}

type recordedUpload struct {
	guestPath string
	content   []byte
	mode      os.FileMode
}

func (w *recordingWorkspace) Exec(_ context.Context, argv []string, _ *env.ExecOpts) (env.ExecResult, error) {
	w.execArgs = append(w.execArgs, argv)
	return env.ExecResult{Code: 0}, nil
}

func (w *recordingWorkspace) Upload(_ context.Context, hostPath, guestPath string) error {
	info, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	w.uploads = append(w.uploads, recordedUpload{guestPath: guestPath, content: content, mode: info.Mode().Perm()})
	return nil
}

func (w *recordingWorkspace) Download(_ context.Context, _, _ string) error  { return nil }
func (w *recordingWorkspace) GuestPath(kind env.PathKind) string             { return string(kind) }
func (w *recordingWorkspace) HostAlias(hostURL string) string                { return hostURL }
func (w *recordingWorkspace) Destroy(_ context.Context, _ env.Outcome) error { return nil }

// --- FileCredentialInjector --------------------------------------------------

func TestFileCredentialInjector_WritesTokenWithDefaultMode(t *testing.T) {
	ws := &recordingWorkspace{}
	inj := FileCredentialInjector(FileCredentialInjectorConfig{
		Token:     "s3cr3t-token",
		GuestPath: "/home/daytona/.tokens/daytona",
	})
	if err := inj.Apply(context.Background(), ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(ws.uploads) != 1 {
		t.Fatalf("want 1 upload, got %d", len(ws.uploads))
	}
	up := ws.uploads[0]
	if got := string(up.content); got != "s3cr3t-token" {
		t.Errorf("content = %q, want token", got)
	}
	if up.guestPath != "/home/daytona/.tokens/daytona" {
		t.Errorf("guestPath = %q", up.guestPath)
	}
	if up.mode != 0o600 {
		t.Errorf("mode = %o, want 600", up.mode)
	}
}

func TestFileCredentialInjector_HonorsCustomMode(t *testing.T) {
	ws := &recordingWorkspace{}
	inj := FileCredentialInjector(FileCredentialInjectorConfig{
		Token:     "abc",
		GuestPath: "/g/token",
		FileMode:  0o640,
	})
	if err := inj.Apply(context.Background(), ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ws.uploads[0].mode != 0o640 {
		t.Errorf("mode = %o, want 640", ws.uploads[0].mode)
	}
}

func TestFileCredentialInjector_Redactions(t *testing.T) {
	inj := FileCredentialInjector(FileCredentialInjectorConfig{Token: "top-secret", GuestPath: "/g"})
	red := inj.Redactions()
	if len(red) != 1 || red[0] != "top-secret" {
		t.Errorf("Redactions = %v, want [top-secret]", red)
	}
}

func TestFileCredentialInjector_CleanupRemovesGuestFile(t *testing.T) {
	ws := &recordingWorkspace{}
	inj := FileCredentialInjector(FileCredentialInjectorConfig{Token: "x", GuestPath: "/home/daytona/.tokens/daytona"})
	if err := inj.Cleanup(context.Background(), ws); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(ws.execArgs) != 1 {
		t.Fatalf("want 1 exec, got %d", len(ws.execArgs))
	}
	want := []string{"rm", "-f", "/home/daytona/.tokens/daytona"}
	if strings.Join(ws.execArgs[0], " ") != strings.Join(want, " ") {
		t.Errorf("cleanup exec = %v, want %v", ws.execArgs[0], want)
	}
}

// --- Sweep -------------------------------------------------------------------

func withClient(config DaytonaConfig, client DaytonaSdkClient) DaytonaConfig {
	config.ClientCtor = func(_ map[string]interface{}) (DaytonaSdkClient, error) { return client, nil }
	return config
}

func TestSweep_EmptyLabelsRefused(t *testing.T) {
	_, err := Sweep(context.Background(), DaytonaConfig{}, SweepOpts{Labels: nil})
	if err == nil {
		t.Fatal("want error for empty labels")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error = %v", err)
	}
}

func TestSweep_DeletesMatchingKeepsRest(t *testing.T) {
	match1 := &fakeSandbox{id: "m1", labels: map[string]string{"run": "abc", "kind": "task"}}
	match2 := &fakeSandbox{id: "m2", labels: map[string]string{"run": "abc", "kind": "task", "extra": "y"}}
	nomatch := &fakeSandbox{id: "n1", labels: map[string]string{"run": "other"}}
	failing := &fakeSandbox{id: "f1", labels: map[string]string{"run": "abc", "kind": "task"}, deleteErr: errFake("boom")}

	client := &fakeClient{sandboxes: []DaytonaSandbox{match1, nomatch, match2, failing}}
	config := withClient(DaytonaConfig{}, client)

	res, err := Sweep(context.Background(), config, SweepOpts{Labels: map[string]string{"run": "abc", "kind": "task"}})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := strings.Join(res.Swept, ","); got != "m1,m2" {
		t.Errorf("Swept = %v, want [m1 m2]", res.Swept)
	}
	if len(res.Failed) != 1 || res.Failed[0].ID != "f1" {
		t.Errorf("Failed = %v, want [f1]", res.Failed)
	}
	if match1.deleted != 1 || match2.deleted != 1 {
		t.Errorf("matching sandboxes not deleted: %d %d", match1.deleted, match2.deleted)
	}
	if nomatch.deleted != 0 {
		t.Errorf("non-matching sandbox deleted %d times", nomatch.deleted)
	}
}

func TestSweep_DryRunDeletesNothing(t *testing.T) {
	match := &fakeSandbox{id: "m1", labels: map[string]string{"run": "abc"}}
	client := &fakeClient{sandboxes: []DaytonaSandbox{match}}
	config := withClient(DaytonaConfig{}, client)

	res, err := Sweep(context.Background(), config, SweepOpts{Labels: map[string]string{"run": "abc"}, DryRun: true})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0] != "m1" {
		t.Errorf("Kept = %v, want [m1]", res.Kept)
	}
	if len(res.Swept) != 0 {
		t.Errorf("Swept = %v, want empty", res.Swept)
	}
	if match.deleted != 0 {
		t.Errorf("dry run deleted sandbox %d times", match.deleted)
	}
}

func TestSweep_UnknownIDFallback(t *testing.T) {
	// No id, no sandboxId -> "<unknown>".
	match := &fakeSandbox{labels: map[string]string{"run": "abc"}}
	client := &fakeClient{sandboxes: []DaytonaSandbox{match}}
	config := withClient(DaytonaConfig{}, client)
	res, err := Sweep(context.Background(), config, SweepOpts{Labels: map[string]string{"run": "abc"}})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Swept) != 1 || res.Swept[0] != "<unknown>" {
		t.Errorf("Swept = %v, want [<unknown>]", res.Swept)
	}
}

// --- CredentialLeakProbe -----------------------------------------------------

func TestCredentialLeakProbe_CoversAllNames(t *testing.T) {
	probe := CredentialLeakProbe()
	for _, name := range CredentialSensitiveEnvNames {
		if !strings.Contains(probe, name) {
			t.Errorf("probe missing sensitive name %q", name)
		}
	}
	if !strings.HasPrefix(probe, "node -e ") {
		t.Errorf("probe should be a node -e command, got %q", probe)
	}
}

func TestCredentialLeakProbe_DetectsPlantedSecret(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping behavioral leak-probe test: %v", err)
	}
	probe := CredentialLeakProbe()
	cleanEnv := []string{"PATH=" + filepath.Dir(nodePath) + ":" + os.Getenv("PATH")}

	// No sensitive vars set -> count 0.
	out := runShell(t, probe, cleanEnv)
	if out != "0" {
		t.Errorf("clean env probe = %q, want 0", out)
	}

	// Plant a secret from the sensitive set -> count 1.
	planted := append(append([]string(nil), cleanEnv...), "DAYTONA_API_KEY=leaked")
	out = runShell(t, probe, planted)
	if out != "1" {
		t.Errorf("planted env probe = %q, want 1", out)
	}
}

func runShell(t *testing.T, command string, envv []string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = envv
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running probe %q: %v (out=%q)", command, err, out)
	}
	return strings.TrimSpace(string(out))
}

// --- exec envelope -----------------------------------------------------------

func TestParseExecEnvelope_RoundTrip(t *testing.T) {
	marker := "__MH_deadbeef__"
	raw := "line1\nline2\n" + marker + "\nerr-a\nerr-b"
	stdout, stderr := parseExecEnvelope(raw, marker)
	if stdout != "line1\nline2" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "err-a\nerr-b" {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestParseExecEnvelope_MissingMarker(t *testing.T) {
	stdout, stderr := parseExecEnvelope("only stdout", "__MH_x__")
	if stdout != "only stdout" || stderr != "" {
		t.Errorf("missing marker: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestBuildExecCommand_QuotesAndEnvelope(t *testing.T) {
	cmd := buildExecCommand([]string{"echo", "hi there"}, "MARK", nil)
	if !strings.Contains(cmd, "'hi there'") {
		t.Errorf("argument not quoted: %q", cmd)
	}
	if !strings.Contains(cmd, "'MARK'") {
		t.Errorf("marker not embedded: %q", cmd)
	}
	stdin := "payload"
	withStdin := buildExecCommand([]string{"cat"}, "MARK", &stdin)
	if !strings.Contains(withStdin, "printf %s 'payload' | ") {
		t.Errorf("stdin prefix missing: %q", withStdin)
	}
}

// --- Exec through the workspace ----------------------------------------------

func TestWorkspaceExec_SplitsEnvelope(t *testing.T) {
	sb := &fakeSandbox{execFn: func(command, _ string, _ map[string]string) (ExecCommandResult, error) {
		// Emulate the guest running buildExecCommand: echo the marker envelope.
		// Extract the marker embedded as '__MH_...__'.
		start := strings.Index(command, "'__MH_")
		end := strings.Index(command[start+1:], "'")
		marker := command[start+1 : start+1+end]
		return ExecCommandResult{Result: "hello\n" + marker + "\noops", ExitCode: 3}, nil
	}}
	ws := &daytonaWorkspace{sandbox: sb, spec: env.WorkspaceSpec{}}
	res, err := ws.Exec(context.Background(), []string{"true"}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Code != 3 || res.Stdout != "hello" || res.Stderr != "oops" {
		t.Errorf("res = %+v", res)
	}
}

// --- Destroy retention -------------------------------------------------------

func TestDestroy_RespectsRetention(t *testing.T) {
	// Empty retention + success -> destroyed.
	sb := &fakeSandbox{id: "a"}
	ws := &daytonaWorkspace{sandbox: sb, spec: env.WorkspaceSpec{}}
	if err := ws.Destroy(context.Background(), env.OutcomeSuccess); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if sb.deleted != 1 {
		t.Errorf("empty retention/success: deleted=%d, want 1", sb.deleted)
	}

	// RetentionAlways + success -> kept.
	sbKeep := &fakeSandbox{id: "b"}
	wsKeep := &daytonaWorkspace{sandbox: sbKeep, spec: env.WorkspaceSpec{Retention: env.RetentionAlways}}
	if err := wsKeep.Destroy(context.Background(), env.OutcomeSuccess); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if sbKeep.deleted != 0 {
		t.Errorf("RetentionAlways/success: deleted=%d, want 0", sbKeep.deleted)
	}
}

func TestDestroy_Idempotent(t *testing.T) {
	sb := &fakeSandbox{id: "a"}
	ws := &daytonaWorkspace{sandbox: sb, spec: env.WorkspaceSpec{}}
	_ = ws.Destroy(context.Background(), env.OutcomeSuccess)
	_ = ws.Destroy(context.Background(), env.OutcomeSuccess)
	if sb.deleted != 1 {
		t.Errorf("double destroy deleted=%d, want 1", sb.deleted)
	}
}

// --- provisioner SDK guard ---------------------------------------------------

func TestProvisioner_PreflightRequiresSDK(t *testing.T) {
	// No ClientCtor wired -> preflight fails (SDK-free unit tier).
	p := Daytona(DaytonaConfig{})
	if err := p.Preflight(context.Background()); err == nil {
		t.Fatal("want preflight error when no SDK is wired")
	}
	// With a fake ctor -> preflight succeeds and create returns a workspace.
	client := &fakeClient{toCreate: &fakeSandbox{id: "sb"}}
	p = Daytona(withClient(DaytonaConfig{}, client))
	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight with fake ctor: %v", err)
	}
	ws, err := p.Create(context.Background(), env.WorkspaceSpec{Image: "img"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ws.GuestPath(env.PathRepo) != "/home/daytona/repo" {
		t.Errorf("GuestPath repo = %q", ws.GuestPath(env.PathRepo))
	}
	if len(client.created) != 1 || client.created[0].AutoStopInterval != 15 {
		t.Errorf("create opts = %+v, want default autostop 15", client.created)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
