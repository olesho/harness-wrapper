package claudecode

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// What the Claude Code profiles share. A profile that runs claude another way
// — the TUI hybrid (pkg/adapter/claudecodetui) — renders claude's
// configuration with ProvisionWith, launches claude from the same
// open_config, reads the same record with NewRecord, and classifies claude's
// failures with TurnError; each of them is this profile's own, so the two
// cannot drift apart.

// OpenConfig is the open_config Provision renders: claude's binary, arguments,
// environment, working directory and the agent's hook spool root.
type OpenConfig = openConfig

// ParseOpenConfig reads an open_config Provision rendered.
func ParseOpenConfig(raw []byte) (OpenConfig, error) { return parseOpenConfig(raw) }

// SessionSpool is the hook spool of Session id under the spool root: the one
// the Session's claude process writes, and its record reads.
func SessionSpool(root, id string) string { return sessionSpool(root, id) }

// SessionArgs are the arguments that name the session claude runs: a fresh
// one under its id, or the one it resumes.
func SessionArgs(mode contract.OpenMode, id string, cfg OpenConfig) []string {
	return sessionArgs(mode, id, cfg)
}

// versionWait bounds `claude --version`.
const versionWait = 30 * time.Second

// versionOf runs `claude --version`; a var for tests.
var versionOf = func(ctx context.Context, bin string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", errors.New("no version")
	}
	return f[0], nil
}

// Version is the version of the claude at bin, as `claude --version` run in
// env says, within versionWait.
func Version(ctx context.Context, bin string, env []string) (string, error) {
	vctx, cancel := context.WithTimeout(ctx, versionWait)
	defer cancel()
	return versionOf(vctx, bin, env)
}

// Pinned is the claude version the profiles are verified against.
func Pinned() string {
	pin, _ := versions.Pinned(Name)
	return pin
}

// TurnError is the contract's account of a model call claude reported failed:
// its error tag (an API-error entry's, or StopFailure's error), the HTTP
// status when known, and claude's text, which tells a usage wall and when it
// resets. rejected says the account's usage limit refused the call.
func TurnError(tag string, status int, text string, rejected bool, now time.Time) *contract.TurnError {
	return failure{tag: tag, status: status, text: text, rejected: rejected}.turnError(now)
}

// maxTaskDescription bounds a background task's description, in bytes.
const maxTaskDescription = 200

// BackgroundTask is a task claude runs in the background, in the contract's
// terms: a shell is a command (stream-json's local_bash, a hook's shell), an
// agent a subagent (stream-json's …_agent, a hook's subagent), anything else
// other; a long description is cut on a rune.
func BackgroundTask(id, typ, description string) contract.BackgroundTask {
	kind := contract.BackgroundOther
	switch {
	case typ == "local_bash" || typ == "shell":
		kind = contract.BackgroundCommand
	case strings.HasSuffix(typ, "_agent") || typ == "subagent":
		kind = contract.BackgroundSubagent
	}
	if len(description) > maxTaskDescription {
		cut := maxTaskDescription
		for cut > 0 && !utf8.RuneStart(description[cut]) {
			cut--
		}
		description = description[:cut]
	}
	return contract.BackgroundTask{ID: id, Kind: kind, Description: description}
}

// OwnNative is the native id of the turn claude starts itself to take up the
// background work task that ended: the one the record names it by, from the
// task notification's entry.
func OwnNative(task string) string { return ownNative(task) }
