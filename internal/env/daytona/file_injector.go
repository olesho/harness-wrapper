// File-based credential injector for sandboxed environments (design §6).
//
// Delivers a short-lived scoped token to the sandbox as a file, registered for
// redaction, and removed on cleanup.

package daytona

import (
	"context"
	"os"
	"path/filepath"

	"github.com/olesho/harness-wrapper/internal/env"
)

// FileCredentialInjectorConfig configures a FileCredentialInjector.
type FileCredentialInjectorConfig struct {
	// Token is the secret to write. Registered for redaction before apply.
	Token string
	// GuestPath is where the token is written in the guest (e.g. ~/.tokens/daytona).
	GuestPath string
	// FileMode is the chmod applied to the host staging file (and, via upload,
	// the delivered credential). Zero defaults to 0o600 — owner read/write only.
	FileMode os.FileMode
}

// FileCredentialInjector writes a file-based token into the sandbox, registered
// for redaction and removed idempotently on cleanup.
func FileCredentialInjector(config FileCredentialInjectorConfig) env.CredentialInjector {
	return &fileInjector{config: config}
}

type fileInjector struct {
	config FileCredentialInjectorConfig
}

func (f *fileInjector) Requires() []env.Capability { return nil }

func (f *fileInjector) Redactions() []string { return []string{f.config.Token} }

func (f *fileInjector) mode() os.FileMode {
	if f.config.FileMode == 0 {
		return 0o600
	}
	return f.config.FileMode
}

func (f *fileInjector) Apply(ctx context.Context, ws env.Workspace) error {
	// Write the token to a temporary host file with restrictive permissions,
	// then upload it to the guest. A per-apply temp dir avoids collisions and
	// lets us set the mode before any content is written.
	hostTmp, err := os.MkdirTemp("", "daytona-token-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(hostTmp) }()

	hostFile := filepath.Join(hostTmp, "token")
	if err := os.WriteFile(hostFile, []byte(f.config.Token), f.mode()); err != nil {
		return err
	}
	// WriteFile honors umask; force the exact mode so the credential is never
	// group/world readable regardless of the process umask.
	if err := os.Chmod(hostFile, f.mode()); err != nil {
		return err
	}
	return ws.Upload(ctx, hostFile, f.config.GuestPath)
}

func (f *fileInjector) Cleanup(ctx context.Context, ws env.Workspace) error {
	// Idempotently remove the credential file from the guest. rm -f suppresses
	// errors if the file doesn't exist; exec errors are swallowed (cleanup runs
	// on failure paths too).
	_, _ = ws.Exec(ctx, []string{"rm", "-f", f.config.GuestPath}, nil)
	return nil
}
