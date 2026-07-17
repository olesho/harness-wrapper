// Package daytona is the Daytona provisioner (design §3, §4): elastically
// provisioned remote sandboxes.
//
// Clean-room Go re-implementation of meta-harness/env-daytona. The real Daytona
// SDK is NOT a Go dependency of this package: the driver depends only on the
// DaytonaSdkClient / DaytonaSandbox / DaytonaClientCtor interfaces declared
// here, so a fake can be injected in unit tests and the real SDK is wired for
// the live tier only (via DaytonaConfig.ClientCtor). This keeps the package
// building and unit-testable without a live SDK.
//
// The child package imports the internal/env core for shared primitives
// (Provisioner/Workspace interfaces, ArgvToShell, ShQuote, ShouldKeep). The
// child -> parent import direction is cycle-free.
package daytona

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/internal/env"
)

// DaytonaConfig configures the Daytona provisioner.
type DaytonaConfig struct {
	// APIKey is the Daytona API key (from environment or credential store).
	APIKey string
	// APIURL overrides the Daytona API URL (default: public Daytona SaaS).
	APIURL string
	// Target overrides the Daytona region/target (default: auto-selected).
	Target string
	// ClientCtor injects the SDK client constructor. The real @daytonaio/sdk is
	// not a Go dependency; the live tier wires the real SDK here, and tests wire
	// a fake. When nil, loadDaytonaClass errors — the package stays SDK-free at
	// the unit tier. Mirrors upstream's `sdkImport` override seam.
	ClientCtor DaytonaClientCtor
}

// ExecCommandResult is the result of DaytonaSandbox.ExecuteCommand. The real
// SDK merges stdout+stderr into Result (no separate stderr stream), hence the
// marker envelope used by buildExecCommand/parseExecEnvelope.
type ExecCommandResult struct {
	Result   string
	ExitCode int
}

// CreateOpts are the inputs to DaytonaSdkClient.Create.
type CreateOpts struct {
	Image              string
	Labels             map[string]string
	AutoStopInterval   int
	AutoDeleteInterval int
}

// ListQuery filters DaytonaSdkClient.List.
type ListQuery struct {
	Labels map[string]string
}

// DaytonaSandbox is the subset of the real @daytonaio/sdk Sandbox surface this
// driver depends on. ID is always populated on a real SDK Sandbox; SandboxID is
// kept only as a defensive fallback for older shapes.
type DaytonaSandbox interface {
	ID() string
	SandboxID() string
	Labels() map[string]string
	// ExecuteCommand mirrors process.executeCommand; timeout nil means no
	// override. stdout+stderr are merged into ExecCommandResult.Result.
	ExecuteCommand(ctx context.Context, command, cwd string, envv map[string]string, timeout *int) (ExecCommandResult, error)
	// UploadFile mirrors fs.uploadFile.
	UploadFile(ctx context.Context, buffer []byte, filePath string) error
	// DownloadFile mirrors fs.downloadFile.
	DownloadFile(ctx context.Context, filePath string) ([]byte, error)
	// Delete is directly callable on a listed sandbox (verified against real SDK).
	Delete(ctx context.Context, timeoutSeconds int) error
}

// DaytonaSdkClient is the subset of the real @daytonaio/sdk Daytona client
// surface this driver depends on.
type DaytonaSdkClient interface {
	Create(ctx context.Context, opts CreateOpts) (DaytonaSandbox, error)
	// List returns every sandbox matching the query. The real SDK returns an
	// auto-paginating async iterator; returning the drained slice preserves the
	// "visits every sandbox, no manual page-looping" contract.
	List(ctx context.Context, query ListQuery) ([]DaytonaSandbox, error)
}

// DaytonaClientCtor constructs a DaytonaSdkClient from a client config map.
// Mirrors upstream's `new (config) => DaytonaSdkClient`.
type DaytonaClientCtor func(config map[string]interface{}) (DaytonaSdkClient, error)

// loadDaytonaClass returns the configured client constructor. Shared by
// preflight, create and sweep so there is exactly one SDK-loading path. Without
// a wired ClientCtor it errors — the real SDK is not a Go dependency.
func loadDaytonaClass(config DaytonaConfig) (DaytonaClientCtor, error) {
	if config.ClientCtor != nil {
		return config.ClientCtor, nil
	}
	return nil, fmt.Errorf("daytona: no Daytona SDK client constructor wired; set DaytonaConfig.ClientCtor (the real @daytonaio/sdk is not a Go dependency)")
}

func clientConfigFor(config DaytonaConfig) map[string]interface{} {
	cfg := map[string]interface{}{"apiKey": config.APIKey}
	if config.APIURL != "" {
		cfg["apiUrl"] = config.APIURL
	}
	if config.Target != "" {
		cfg["target"] = config.Target
	}
	return cfg
}

// Daytona constructs the Daytona provisioner.
func Daytona(config DaytonaConfig) env.Provisioner {
	return &daytonaProvisioner{config: config}
}

type daytonaProvisioner struct {
	config DaytonaConfig
}

func (p *daytonaProvisioner) Name() string { return "daytona" }

func (p *daytonaProvisioner) Preflight(ctx context.Context) error {
	// Load SDK; if it fails, the provisioner cannot be used.
	_, err := loadDaytonaClass(p.config)
	return err
}

func (p *daytonaProvisioner) Create(ctx context.Context, spec env.WorkspaceSpec) (env.Workspace, error) {
	ctor, err := loadDaytonaClass(p.config)
	if err != nil {
		return nil, err
	}
	client, err := ctor(clientConfigFor(p.config))
	if err != nil {
		return nil, err
	}
	autoStop := spec.AutoStopInterval
	if autoStop == 0 {
		autoStop = 15
	}
	sandbox, err := client.Create(ctx, CreateOpts{
		Image:              spec.Image,
		Labels:             orEmpty(spec.Labels),
		AutoStopInterval:   autoStop,
		AutoDeleteInterval: spec.AutoDeleteInterval,
	})
	if err != nil {
		return nil, err
	}
	return &daytonaWorkspace{sandbox: sandbox, spec: spec}, nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// buildExecCommand wraps argv so the merged stdout+stderr stream can be split
// back apart. Envelope layout: <stdout>\n<marker>\n<stderr>.
func buildExecCommand(argv []string, marker string, stdin *string) string {
	body := env.ArgvToShell(argv)
	stdinPrefix := ""
	if stdin != nil {
		stdinPrefix = "printf %s " + env.ShQuote(*stdin) + " | "
	}
	return "__e=$(mktemp); { " + stdinPrefix + body + "; } 2>\"$__e\"; " +
		"__c=$?; printf '\\n%s\\n' '" + marker + "'; cat \"$__e\"; rm -f \"$__e\"; exit $__c"
}

// parseExecEnvelope splits the merged stream back into stdout/stderr. A missing
// marker (truncated/odd SDK behavior) treats the whole payload as stdout.
func parseExecEnvelope(raw, marker string) (stdout, stderr string) {
	sep := "\n" + marker + "\n"
	idx := strings.Index(raw, sep)
	if idx == -1 {
		return raw, ""
	}
	return raw[:idx], raw[idx+len(sep):]
}

func newMarker() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "__MH_" + hex.EncodeToString(b) + "__", nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type daytonaWorkspace struct {
	sandbox   DaytonaSandbox
	spec      env.WorkspaceSpec
	destroyed bool
}

func (w *daytonaWorkspace) Exec(ctx context.Context, argv []string, opts *env.ExecOpts) (env.ExecResult, error) {
	marker, err := newMarker()
	if err != nil {
		return env.ExecResult{}, err
	}
	var cwd string
	var envv map[string]string
	var stdin *string
	if opts != nil {
		cwd = opts.Cwd
		envv = opts.Env
		stdin = opts.Stdin
	}
	command := buildExecCommand(argv, marker, stdin)
	res, err := w.sandbox.ExecuteCommand(ctx, command, cwd, envv, nil)
	if err != nil {
		return env.ExecResult{}, err
	}
	stdout, stderr := parseExecEnvelope(res.Result, marker)
	return env.ExecResult{Code: res.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

func (w *daytonaWorkspace) Upload(ctx context.Context, hostPath, guestPath string) error {
	info, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return w.uploadDir(ctx, hostPath, guestPath)
	}
	// Mirror local's mkParent: the guest parent dir is not guaranteed to exist.
	if err := w.execChecked(ctx, []string{"mkdir", "-p", path.Dir(guestPath)}); err != nil {
		return err
	}
	buf, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	return w.sandbox.UploadFile(ctx, buf, guestPath)
}

func (w *daytonaWorkspace) uploadDir(ctx context.Context, hostPath, guestPath string) error {
	hostTmp, err := os.MkdirTemp("", "mh-daytona-up-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(hostTmp)
	tarPath := filepath.Join(hostTmp, "up.tar")
	if err := exec.CommandContext(ctx, "tar", "-C", hostPath, "-cf", tarPath, ".").Run(); err != nil {
		return err
	}
	suffix, err := randHex(8)
	if err != nil {
		return err
	}
	guestTar := w.GuestPath(env.PathTmp) + "/mh-up-" + suffix + ".tar"
	buf, err := os.ReadFile(tarPath)
	if err != nil {
		return err
	}
	if err := w.execChecked(ctx, []string{"mkdir", "-p", w.GuestPath(env.PathTmp)}); err != nil {
		return err
	}
	if err := w.sandbox.UploadFile(ctx, buf, guestTar); err != nil {
		return err
	}
	if err := w.execChecked(ctx, []string{"mkdir", "-p", guestPath}); err != nil {
		return err
	}
	if err := w.execChecked(ctx, []string{"tar", "-xf", guestTar, "-C", guestPath}); err != nil {
		return err
	}
	return w.execChecked(ctx, []string{"rm", "-f", guestTar})
}

func (w *daytonaWorkspace) Download(ctx context.Context, guestPath, hostPath string) error {
	isDir, err := w.execIsDir(ctx, guestPath)
	if err != nil {
		return err
	}
	if isDir {
		return w.downloadDir(ctx, guestPath, hostPath)
	}
	// Mirror local's mkParent(hostPath).
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}
	buf, err := w.sandbox.DownloadFile(ctx, guestPath)
	if err != nil {
		return err
	}
	return os.WriteFile(hostPath, buf, 0o644)
}

func (w *daytonaWorkspace) execIsDir(ctx context.Context, guestPath string) (bool, error) {
	r, err := w.Exec(ctx, []string{"test", "-d", guestPath}, nil)
	if err != nil {
		return false, err
	}
	return r.Code == 0, nil
}

func (w *daytonaWorkspace) downloadDir(ctx context.Context, guestPath, hostPath string) error {
	suffix, err := randHex(8)
	if err != nil {
		return err
	}
	guestTar := w.GuestPath(env.PathTmp) + "/mh-down-" + suffix + ".tar"
	if err := w.execChecked(ctx, []string{"tar", "-cf", guestTar, "-C", guestPath, "."}); err != nil {
		return err
	}
	buf, err := w.sandbox.DownloadFile(ctx, guestTar)
	if err != nil {
		return err
	}
	if err := w.execChecked(ctx, []string{"rm", "-f", guestTar}); err != nil {
		return err
	}
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		return err
	}
	hostTmp, err := os.MkdirTemp("", "mh-daytona-down-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(hostTmp)
	tarPath := filepath.Join(hostTmp, "down.tar")
	if err := os.WriteFile(tarPath, buf, 0o644); err != nil {
		return err
	}
	return exec.CommandContext(ctx, "tar", "-xf", tarPath, "-C", hostPath).Run()
}

func (w *daytonaWorkspace) execChecked(ctx context.Context, argv []string) error {
	r, err := w.Exec(ctx, argv, nil)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		detail := r.Stderr
		if detail == "" {
			detail = r.Stdout
		}
		return fmt.Errorf("daytona: %s failed (code %d): %s", strings.Join(argv, " "), r.Code, detail)
	}
	return nil
}

func (w *daytonaWorkspace) GuestPath(kind env.PathKind) string {
	switch kind {
	case env.PathRepo:
		return "/home/daytona/repo"
	case env.PathHome:
		return "/home/daytona/.home"
	case env.PathTmp:
		return "/tmp"
	default:
		return "/tmp"
	}
}

// HostAlias is identity: Daytona sandboxes reach the host via localhost.
func (w *daytonaWorkspace) HostAlias(hostURL string) string { return hostURL }

func (w *daytonaWorkspace) Destroy(ctx context.Context, outcome env.Outcome) error {
	if w.destroyed {
		return nil // idempotent
	}
	w.destroyed = true // set BEFORE the ShouldKeep check — see design note.
	if env.ShouldKeep(w.spec.Retention, outcome) {
		return nil // kept for debugging.
	}
	if err := w.sandbox.Delete(ctx, 60); err != nil {
		// Best-effort cleanup; surfaced to the caller.
		return fmt.Errorf("daytona: failed to delete sandbox: %w", err)
	}
	return nil
}
