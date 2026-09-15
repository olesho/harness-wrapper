//go:build linux

package contain

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Seeds are the only configuration a contained harness starts with: never a
// copy of the user's harness home, caches, hooks or MCP definitions. Each seed
// is written once, when its file does not exist yet, so a stored conversation
// keeps the harness's own later edits and a caller-managed StateDir keeps the
// login it holds.

// claudeSeed returns the .claude.json claude needs to reach its composer
// unattended: onboarding done (without it the TUI stops at the theme and
// login-method screens even with a token set), the working directory trusted,
// and — when the session authenticates with ANTHROPIC_API_KEY — the key's
// custom-key screen pre-approved by its last 20 characters, because
// harness-wrapper has no matcher for that screen.
func claudeSeed(workingDir, apiKey string) ([]byte, error) {
	cfg := map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			workingDir: map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	if apiKey != "" {
		tail := apiKey
		if len(tail) > 20 {
			tail = tail[len(tail)-20:]
		}
		cfg["customApiKeyResponses"] = map[string]any{
			"approved": []string{tail},
			"rejected": []string{},
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// codexConfig returns codex's seeded config.toml:
//   - check_for_update_on_startup = false: no update check or menu;
//   - [features] plugins = false: avoids an 88 MB plugin fetch;
//   - [features] unified_exec = false (0.144.5 replaces the PTY-backed
//     exec_command with the pipe-only shell_command) and unified_exec_tty =
//     false (0.154.0 removes exec_command's tty parameter). Each version
//     ignores the other's key, so every tool command runs over pipes and the
//     profile needs neither /dev/ptmx nor /dev/pts;
//   - the working directory trusted, so an unattended TUI reaches its composer.
func codexConfig(workingDir string) ([]byte, error) {
	quoted, err := tomlQuote(workingDir)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# Seeded by harness-wrapper for a contained session.\n")
	b.WriteString("check_for_update_on_startup = false\n\n")
	b.WriteString("[features]\nplugins = false\nunified_exec = false\nunified_exec_tty = false\n\n")
	fmt.Fprintf(&b, "[projects.%s]\ntrust_level = \"trusted\"\n", quoted)
	return []byte(b.String()), nil
}

// codexAuth returns an API-key auth.json. Interactive codex authenticates only
// from CODEX_HOME/auth.json, so a caller-supplied key is seeded here; the
// rotating ChatGPT auth.json is never copied (its refresh token is single-use,
// so two copies invalidate each other) — that login lives in a caller StateDir.
func codexAuth(apiKey string) ([]byte, error) {
	b, err := json.MarshalIndent(map[string]string{
		"auth_mode":      "apikey",
		"OPENAI_API_KEY": apiKey,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// tomlQuote renders s as a TOML basic string.
func tomlQuote(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("path %q is not valid UTF-8 and cannot be written to codex's config", s)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}
