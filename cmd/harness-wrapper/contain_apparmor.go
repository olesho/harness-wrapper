package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/olesho/harness-wrapper/internal/apparmor"
)

// runContainAppArmorProfile prints the AppArmor socket layer's profile:
//
//	harness-wrapper contain-apparmor-profile --root DIR [--root DIR ...]
//
// A contained launch stacks it when the kernel's Landlock ABI predates
// RESOLVE_UNIX and the request accepts that (--contain-min-abi below 9). It
// denies writes, and so connects to pathname UNIX sockets, outside the roots,
// so every directory a contained harness writes — working directories,
// --contain-rw paths, --contain-state-dir and the managed-state directory
// (XDG_STATE_HOME/harness-wrapper) — must lie beneath one. Each root must
// exist; it is recorded with symlinks resolved. Root installs the output as
// /etc/apparmor.d/harness-wrapper-contain and loads it with apparmor_parser -r.
// Exit 0 on success, 2 on a usage error.
func runContainAppArmorProfile(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("contain-apparmor-profile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var roots stringList
	fs.Var(&roots, "root", "a directory beneath which a contained harness may write and connect to pathname UNIX sockets (repeatable, at least one)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: harness-wrapper contain-apparmor-profile --root DIR [--root DIR ...]")
		_, _ = fmt.Fprintf(stderr, "Prints the AppArmor socket layer. Install it as %s and load it with\n", apparmor.PolicyPath)
		_, _ = fmt.Fprintf(stderr, "  apparmor_parser -r %s\n", apparmor.PolicyPath)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || len(roots) == 0 {
		fs.Usage()
		return 2
	}
	canonical := make([]string, 0, len(roots))
	for _, r := range roots {
		c, err := canonicalDir(r)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-apparmor-profile:", err)
			return 2
		}
		canonical = append(canonical, c)
	}
	src, err := apparmor.Profile(canonical)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-apparmor-profile:", err)
		return 2
	}
	_, _ = io.WriteString(stdout, src)
	return 0
}

// canonicalDir resolves an existing directory to its absolute, symlink-free
// path: the form AppArmor matches and a launch compares grants against.
func canonicalDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	c, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("root %s: %w", path, err)
	}
	fi, err := os.Stat(c)
	if err != nil {
		return "", fmt.Errorf("root %s: %w", path, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("root %s is not a directory", path)
	}
	return c, nil
}
