package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// runContainCheck previews a contained launch without starting anything:
//
//	harness-wrapper contain-check [--json] [wrapper flags] <harness> -- [harness args]
//
// It prints the resolved planned policy (placeholders for private directories
// not yet allocated), the kernel's Landlock ABI, whether cgroup supervision is
// available, optional paths absent on this host, and every requirement that
// would refuse the launch. --contain defaults to landlock. Exit 0 when the
// launch would proceed, 1 when it would be refused, 2 on a usage error. The
// output is a preview: it certifies nothing about enforcement.
func runContainCheck(args []string, stdout, stderr io.Writer) int {
	jsonOut := false
	var rest []string
	for i, a := range args {
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	parsed, err := parseHarnessWrapperArgs(rest)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if parsed.Contain.Kind == "" {
		parsed.Contain.Kind = wrapper.ContainmentLandlock
	}
	binPath, err := resolveHarness(parsed.HarnessName)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	harnessArgs := parsed.HarnessArgs
	env := cleanedEnv()
	if parsed.SandboxDefaults {
		harnessArgs, env = applySandboxDefaults(parsed.HarnessName, parsed.PermissionMode, harnessArgs, env)
	}
	wd, _ := os.Getwd()
	p, err := contain.PreviewLaunch(contain.Input{
		Request:    parsed.Contain.request(),
		Harness:    parsed.HarnessName,
		BinaryPath: binPath,
		Args:       harnessArgs,
		LaunchRung: wrapper.EffectiveLaunchRung(parsed.HarnessName, harnessArgs, parsed.PermissionMode),
		WorkingDir: wd,
		Env:        env,
	})
	switch {
	case errors.Is(err, contain.ErrUnsupported):
		_, _ = fmt.Fprintf(stderr, "harness-wrapper contain-check: containment is not supported on %s (Landlock is Linux-only)\n", runtime.GOOS)
		return 1
	case err != nil:
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-check:", err)
		return 2
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(p)
	} else {
		printPreview(stdout, p)
	}
	if !p.WouldLaunch {
		return 1
	}
	return 0
}

func printPreview(w io.Writer, p *contain.Preview) {
	pl := p.Planned
	_, _ = fmt.Fprintln(w, "PREVIEW: nothing was started, and enforcement is not certified.")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { _, _ = fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	if pl != nil {
		row("profile", fmt.Sprintf("%s (manifest %d)", pl.Profile, pl.ProfileVersion))
	}
	kernel := fmt.Sprintf("Landlock ABI %d", p.KernelABI)
	if pl != nil {
		kernel += fmt.Sprintf(" (required %d)", pl.RequiredABI)
	}
	if p.Kernel != "" {
		kernel += ": " + p.Kernel
	}
	row("kernel", kernel)
	sup := p.Supervision
	if p.SupervisionReason != "" {
		sup += ": " + p.SupervisionReason + " (the launch proceeds; private state is kept, not deleted, when it ends)"
	}
	row("supervision", sup)
	if pl != nil {
		tcp := pl.TCP.Mode
		if pl.TCP.Mode == "restricted" {
			tcp = fmt.Sprintf("restricted: connect %v, bind %s", pl.TCP.Connect, pl.TCP.Bind)
		}
		row("tcp", tcp)
		row("unix sockets", "pathname: "+pl.PathnameSockets+"; scopes: "+strings.Join(pl.Scopes, ", "))
		state := fmt.Sprintf("%s: HOME=%s TMPDIR=%s", pl.State.Mode, pl.State.Home, pl.State.Tmp)
		if pl.State.HarnessStateEnv != "" {
			state += fmt.Sprintf(" %s=%s", pl.State.HarnessStateEnv, pl.State.HarnessState)
		}
		row("state", state)
		row("env", strings.Join(pl.Env, " "))
		row("fingerprint", pl.Fingerprint)
	}
	_ = tw.Flush()
	if pl != nil {
		_, _ = fmt.Fprintln(w, "grants:")
		gw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, g := range pl.Grants {
			path := g.Path
			if g.Requested != "" {
				path += " (from " + g.Requested + ")"
			}
			_, _ = fmt.Fprintf(gw, "  %s\t%s\t%s\n", g.Access, path, g.Source)
		}
		_ = gw.Flush()
	}
	for _, n := range p.Notes {
		_, _ = fmt.Fprintln(w, "note:", n)
	}
	if len(p.Omitted) > 0 {
		_, _ = fmt.Fprintln(w, "omitted (optional, absent here):", strings.Join(p.Omitted, ", "))
	}
	if len(p.Missing) > 0 {
		_, _ = fmt.Fprintln(w, "missing requirements:")
		for _, m := range p.Missing {
			_, _ = fmt.Fprintln(w, "  -", m)
		}
		_, _ = fmt.Fprintln(w, "result: the launch would be REFUSED")
		return
	}
	_, _ = fmt.Fprintln(w, "result: the launch would proceed")
}
