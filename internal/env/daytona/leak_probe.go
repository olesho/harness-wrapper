// Credential leak detection for sandboxed environments.
//
// CredentialSensitiveEnvNames is the canonical list of sensitive environment
// variable names that should never cross into a sandbox boundary — the source
// of truth for both the in-guest probe and host-side redaction.

package daytona

import (
	"strings"

	"github.com/olesho/harness-wrapper/internal/env"
)

// CredentialSensitiveEnvNames is the canonical set of sensitive env var names.
var CredentialSensitiveEnvNames = []string{
	"DAYTONA_API_KEY",
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"CODEX_HOME",
	"LOOM_TASK_RUN_LEASE_TOKEN",
	"LOOM_DRIVER_TASK_RUNNER_CMD_JSON",
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"CODEX_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"CURSOR_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// CredentialLeakProbe returns a shell command that probes for credential leaks
// by counting how many of CredentialSensitiveEnvNames are set in the current
// environment. The command prints a single decimal number; a nonzero count
// means a secret reached the sandbox and the run should fail.
//
// Designed to run inside a sandbox via Workspace.Exec.
func CredentialLeakProbe() string {
	var b strings.Builder
	b.WriteString("const names=[")
	for _, name := range CredentialSensitiveEnvNames {
		b.WriteString("['")
		b.WriteString(name)
		b.WriteString("'],")
	}
	b.WriteString("].map((parts)=>parts.join('_'));")
	b.WriteString("let count=0;")
	b.WriteString("for (const name of names) if (process.env[name]) count++;")
	b.WriteString("console.log(count);")

	// Shell-quote the entire node -e argument.
	return "node -e " + env.ShQuote(b.String())
}
