package discovery

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// semverRe matches the first X.Y.Z (optionally followed by -pre.release
// or +build metadata) substring. Used by semverDashVProbe.
var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][\w.]+)?`)

// semverDashVProbe runs `<binary> --version` and extracts the first
// semver-shaped substring from the combined output. Suitable for
// harnesses whose --version line contains a clean X.Y.Z[-suffix] token
// (codex, claude-code, gemini, opencode, and pi at the time of writing).
type semverDashVProbe struct{}

func (semverDashVProbe) Detect(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", path, err)
	}
	m := semverRe.FindString(string(out))
	if m == "" {
		return "", fmt.Errorf("%s --version: no semver in %q",
			path, strings.TrimSpace(string(out)))
	}
	return m, nil
}

func init() {
	p := semverDashVProbe{}
	RegisterProbe("codex", p)
	RegisterProbe("claude-code", p)
	RegisterProbe("gemini", p)
	RegisterProbe("opencode", p)
	RegisterProbe("pi", p)
}
