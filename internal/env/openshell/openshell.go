// Package openshell is the OpenShell containment driver (design §3, §5).
//
// It is a clean-room Go re-implementation of meta-harness's env-openshell layer.
// It wraps the `openshell` CLI transport as an injectable CliRunner over the
// core env.Containment interface. The CLI transport manages sandbox
// create/exec/upload/download/delete operations with:
//
//   - injectable CliRunner for testability (scripted unit tests, no live gateway)
//   - policy generation (per-tier filesystem sets, landlock, per-binary egress)
//   - host-alias rewrite (docker/podman containers reaching the host)
//   - env crossing as in-guest `env K=V` argv PREFIX (openshell exec has no --env)
//   - deterministic sandbox naming for crash recovery
//
// It imports the core internal/env package for the shared primitives
// (Containment/Workspace interfaces, ArgvToShell, ShQuote, EnvPrefixedShell).
// The core imports no driver, so this child->parent import is cycle-free.
package openshell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/olesho/harness-wrapper/internal/env"
)

// CliResult is a CLI runner result shape.
type CliResult struct {
	Code   int
	Stdout string
	Stderr string
}

// CliRunner is an injectable host runner for `openshell …` invocations. Tests
// script the daemon without a live gateway; the default spawns a real process.
type CliRunner func(argv []string) CliResult

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// flagNoTTY disables openshell's pseudo-terminal for non-interactive exec.
const flagNoTTY = "--no-tty"

// stripAnsi strips ANSI SGR color escapes from CLI output.
func stripAnsi(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// SandboxName normalizes an agentId into an OpenShell sandbox name: `openshell-`
// + lowercased, charset-bounded ([a-z0-9-]), length-bounded slug with a hash
// suffix on truncation. Collision-resistant and deterministic for crash
// recovery.
func SandboxName(agentID string) string {
	slug := nonSlug.ReplaceAllString(strings.ToLower(agentID), "-")
	slug = strings.Trim(slug, "-")
	const max = 40
	const prefix = "openshell-"
	if len(slug)+len(prefix) <= max {
		return prefix + slug
	}
	sum := sha256.Sum256([]byte(agentID))
	hash := hex.EncodeToString(sum[:])[:8]
	keep := max - len(prefix) - 1 - len(hash)
	if keep < 1 {
		keep = 1
	}
	if keep > len(slug) {
		keep = len(slug)
	}
	return fmt.Sprintf("%s%s-%s", prefix, slug[:keep], hash)
}

// loopback hosts that cannot reach a host gateway.
var loopback = map[string]bool{
	"127.0.0.1": true,
	"localhost": true,
	"::1":       true,
	"0.0.0.0":   true,
}

// hostGatewayAlias returns the host-gateway alias for a container driver, or ""
// when the driver has none.
func hostGatewayAlias(driver string) string {
	switch driver {
	case "container", "docker":
		return "host.docker.internal"
	case "podman":
		return "host.containers.internal"
	default:
		return ""
	}
}

// ResolveGuestURL rewrites a loopback URL to a guest-reachable address for the
// driver. It returns an error when loopback can't be routed and no override is
// configured. A non-empty guestOverride short-circuits everything.
func ResolveGuestURL(hostURL, driver, guestOverride string) (string, error) {
	if s := strings.TrimSpace(guestOverride); s != "" {
		return s, nil
	}
	u, err := url.Parse(hostURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", hostURL)
	}
	if !loopback[u.Hostname()] {
		return hostURL, nil
	}
	alias := hostGatewayAlias(driver)
	if alias == "" {
		return "", fmt.Errorf("URL is loopback (%s) and driver %q cannot route it", u.Hostname(), driver)
	}
	if port := u.Port(); port != "" {
		u.Host = alias + ":" + port
	} else {
		u.Host = alias
	}
	result := u.String()
	// Drop a trailing slash if the path is empty or just "/".
	if strings.HasSuffix(result, "/") && u.Path == "/" {
		result = result[:len(result)-1]
	}
	return result, nil
}

// ScrapeEndpoint is an extra egress target the guest may reach (e.g. a scrape
// site), bound to the processes allowed to reach it. It is emitted as a BARE
// {host, port} endpoint — a plain CONNECT tunnel — which is the shape that
// actually works: a `tls: terminate` lane makes the in-guest proxy 403 the
// browser/curl CONNECT (field-tested, openshell 0.0.53). NB egress ALSO requires
// the guest image to ship a statable /init.krun (the proxy's ancestor-integrity
// check stats the libkrun PID-1 init); without it every lane is denied
// regardless of this policy.
type ScrapeEndpoint struct {
	Host string
	// Port defaults to 443 when zero.
	Port int
	// Binaries are absolute guest paths of the processes allowed to reach Host
	// (e.g. a browser binary). openshell matches the connecting process's exe +
	// ancestor chain.
	Binaries []string
}

// PolicyScopes are the inputs to GeneratePolicy: per-tier filesystem sets,
// landlock, and per-binary egress. GeneratePolicy is a pure function with no I/O.
type PolicyScopes struct {
	Tier      string
	ModelHost string
	// ModelPort defaults to 443 when zero.
	ModelPort   int
	FleetHost   string
	FleetPort   int
	HarnessPath string
	// ScrapeEndpoints are OPTIONAL extra egress targets. Absent/empty ⇒ NO scrape
	// lane is emitted and the generated policy is byte-for-byte unchanged
	// (additive; existing consumers unaffected).
	ScrapeEndpoints []ScrapeEndpoint
}

type tierKnobs struct {
	readOnly    []string
	enforcement string
}

func knobsForTier(tier string) (tierKnobs, error) {
	switch tier {
	case "untrusted":
		return tierKnobs{
			readOnly:    []string{"/usr", "/lib", "/lib64", "/etc", "/bin", "/sbin", "/opt"},
			enforcement: "enforce",
		}, nil
	case "semi-trusted":
		return tierKnobs{
			readOnly:    []string{"/usr", "/lib", "/etc", "/bin"},
			enforcement: "enforce",
		}, nil
	case "trusted-internal":
		return tierKnobs{
			readOnly:    []string{"/usr", "/lib"},
			enforcement: "observe",
		}, nil
	default:
		return tierKnobs{}, fmt.Errorf("unknown tier %q", tier)
	}
}

// GeneratePolicy renders the sandbox policy YAML for the given scopes. It is a
// pure function with no I/O and returns an error only for an unknown tier.
func GeneratePolicy(scopes PolicyScopes) (string, error) {
	knobs, err := knobsForTier(scopes.Tier)
	if err != nil {
		return "", err
	}
	modelPort := scopes.ModelPort
	if modelPort == 0 {
		modelPort = 443
	}

	ro := make([]string, len(knobs.readOnly))
	for i, p := range knobs.readOnly {
		ro[i] = "'" + p + "'"
	}

	var lines []string
	lines = append(
		lines,
		"version: 1",
		"filesystem_policy:",
		"  include_workdir: false",
		fmt.Sprintf("  read_only: [%s]", strings.Join(ro, ", ")),
		"  read_write: [/sandbox, /tmp]",
		"process: { run_as_user: sandbox, run_as_group: sandbox }",
		"landlock: { compatibility: best_effort }",
		"network_policies:",
		"  model:",
		fmt.Sprintf("    endpoints: [{ host: %s, port: %d, protocol: rest, access: full, enforcement: %s }]",
			scopes.ModelHost, modelPort, knobs.enforcement),
		"    binaries: [{ path: /usr/local/bin/claude }]",
		"  fleet:",
		fmt.Sprintf("    endpoints: [{ host: %s, port: %d, protocol: rest, access: full, enforcement: %s }]",
			scopes.FleetHost, scopes.FleetPort, knobs.enforcement),
		fmt.Sprintf("    binaries: [{ path: %s }, { path: /usr/local/bin/orche }]", scopes.HarnessPath),
	)
	// Optional scrape lanes — one per endpoint so each host is bound to exactly
	// its own binaries (a shared lane would let any listed binary reach any listed
	// host). Bare {host, port} endpoints (plain CONNECT tunnel), no name field —
	// same shape as the model/fleet lanes above, which rely on the key as the lane
	// name.
	for i, e := range scopes.ScrapeEndpoints {
		port := e.Port
		if port == 0 {
			port = 443
		}
		bins := make([]string, len(e.Binaries))
		for j, b := range e.Binaries {
			bins[j] = fmt.Sprintf("{ path: %s }", b)
		}
		lines = append(
			lines,
			fmt.Sprintf("  scrape_%d:", i),
			fmt.Sprintf("    endpoints: [{ host: %s, port: %d }]", e.Host, port),
			fmt.Sprintf("    binaries: [%s]", strings.Join(bins, ", ")),
		)
	}
	lines = append(lines, "  # git hub: bundle-out ⇒ NO network endpoint")
	return strings.Join(lines, "\n") + "\n", nil
}

// Options configure an OpenShellContainment.
type Options struct {
	Driver   string
	Provider string
	// GuestPath is the in-guest repo path (default /sandbox/repo).
	GuestPath string
	// AgentID is the default agentId for sandbox naming when policy.agentId is
	// absent.
	AgentID string
	// From is an image/Dockerfile ref for `sandbox create --from` (e.g. a
	// node-bearing image when the gateway's default image lacks node).
	From string
	// CLI is the injectable runner; nil defaults to a real process spawner.
	CLI CliRunner
}

// OpenShellContainment is the OpenShell containment implementation.
type OpenShellContainment struct {
	opts      Options
	cli       CliRunner
	driver    string
	provider  string
	guestPath string
}

// OpenShell constructs an OpenShell containment.
func OpenShell(opts Options) *OpenShellContainment {
	c := &OpenShellContainment{
		opts:      opts,
		cli:       opts.CLI,
		driver:    opts.Driver,
		provider:  opts.Provider,
		guestPath: opts.GuestPath,
	}
	if c.cli == nil {
		c.cli = spawnOpenShellCli
	}
	if c.driver == "" {
		c.driver = "container"
	}
	if c.provider == "" {
		c.provider = "anthropic"
	}
	if c.guestPath == "" {
		c.guestPath = "/sandbox/repo"
	}
	return c
}

// Name identifies the containment.
func (c *OpenShellContainment) Name() string { return "openshell" }

var connectedRE = regexp.MustCompile(`(?i)\bconnected\b`)

// Preflight checks gateway connectivity and provider registration. It runs
// host-side via the injected CLI runner.
func (c *OpenShellContainment) Preflight(_ context.Context, _ env.Workspace) error {
	st := c.cli([]string{"openshell", "status"})
	if st.Code != 0 {
		msg := strings.TrimSpace(st.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(st.Stdout)
		}
		return fmt.Errorf("openshell gateway not available: %s", clip(msg, 300))
	}
	statusText := stripAnsi(st.Stdout)
	if !connectedRE.MatchString(statusText) {
		return fmt.Errorf("openshell gateway not Connected: %s", clip(strings.TrimSpace(statusText), 300))
	}
	pr := c.cli([]string{"openshell", "provider", "get", c.provider})
	if pr.Code != 0 {
		return fmt.Errorf("openshell provider %q not registered", c.provider)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Layer returns the primitives consumed by the core Compose combinator.
//
// This is the unit-test / caller-owned-sandbox seam: the sandbox name is read
// from policy.Extra["sandboxName"]. Production goes through Acquire, which
// creates the sandbox and returns a layer closed over the real name.
func (c *OpenShellContainment) Layer(policy env.PolicySpec) env.ContainmentLayer {
	name, _ := policy.Extra["sandboxName"].(string)
	if name == "" {
		// Mirrors the TS throw: without a name there is nothing to close over. The
		// interface has no error return, so surface a layer that fails every call
		// loudly rather than silently addressing a nameless sandbox.
		return errorLayer{err: fmt.Errorf(
			"openshell.Layer: no sandbox name — use Acquire (production path) or pass policy.Extra[\"sandboxName\"] (unit-test seam)",
		)}
	}
	return buildLayer(name, c.guestPath, c.driver)
}

// Acquire creates the sandbox (lifecycle step 4) and returns a layer closed over
// its name. All commands run via the INNER workspace's exec (containment runs
// where inner runs, §5.1). A failure best-effort deletes any half-created
// sandbox before returning.
func (c *OpenShellContainment) Acquire(ctx context.Context, ws env.Workspace, policy env.PolicySpec) (env.ContainmentLayer, error) {
	agentID := stringFrom(policy.Extra, "agentId")
	if agentID == "" {
		agentID = c.opts.AgentID
	}
	if agentID == "" {
		return nil, fmt.Errorf("openshell.Acquire: no agentId — set policy.Extra[\"agentId\"] or OpenShell(Options{AgentID})")
	}
	name := SandboxName(agentID)

	// Crash recovery: best-effort delete of a leftover sandbox under the same
	// deterministic name from a crashed prior run.
	_, _ = ws.Exec(ctx, []string{"openshell", "sandbox", "delete", name}, nil)

	// Explicit tier ⇒ stage a generated policy file; absent ⇒ gateway default.
	var policyPath string
	if policy.Tier != "" {
		yaml, err := GeneratePolicy(PolicyScopes{
			Tier:            policy.Tier,
			ModelHost:       stringOr(policy.Extra, "modelHost", "api.anthropic.com"),
			ModelPort:       intOr(policy.Extra, "modelPort", 443),
			FleetHost:       stringOr(policy.Extra, "fleetHost", "localhost"),
			FleetPort:       intOr(policy.Extra, "fleetPort", 53343),
			HarnessPath:     stringOr(policy.Extra, "harnessPath", "/usr/local/bin/harness-wrapper"),
			ScrapeEndpoints: scrapeEndpointsFrom(policy.Extra),
		})
		if err != nil {
			return nil, err
		}
		policyPath = ws.GuestPath(env.PathTmp) + "/openshell-policy-" + name + ".yaml"
		staged, err := ws.Exec(ctx, []string{"sh", "-c", "cat > " + env.ShQuote(policyPath)}, &env.ExecOpts{Stdin: &yaml})
		if err != nil {
			return nil, err
		}
		if staged.Code != 0 {
			return nil, fmt.Errorf("openshell.Acquire: staging policy file failed (exit %d): %s", staged.Code, staged.Stderr+staged.Stdout)
		}
	}

	// `create` with no trailing command attaches an interactive shell and never
	// exits under piped stdio (field-tested, 0.0.53) — run `true` as the initial
	// command instead: create exits once it returns, and the sandbox is kept alive
	// (deleting on command exit is opt-in via --no-keep).
	createArgv := []string{"openshell", "sandbox", "create", "--name", name}
	if c.opts.From != "" {
		createArgv = append(createArgv, "--from", c.opts.From)
	}
	if policyPath != "" {
		createArgv = append(createArgv, "--policy", policyPath)
	}
	createArgv = append(createArgv, flagNoTTY, "--", "true")
	created, err := ws.Exec(ctx, createArgv, nil)
	if err != nil {
		return nil, err
	}
	if created.Code != 0 {
		return nil, fmt.Errorf("openshell.Acquire: sandbox create failed (exit %d): %s", created.Code, created.Stderr+created.Stdout)
	}

	// Anything fallible after create must not leak the sandbox: best-effort delete
	// before returning an error. Guest layout prep — default image layout is not
	// guaranteed. One multi-arg mkdir: fully succeeds or fully fails, no partial
	// case.
	prep, err := ws.Exec(ctx, []string{
		"openshell", "sandbox", "exec", "-n", name, flagNoTTY, "--",
		"mkdir", "-p", c.guestPath, "/sandbox/.home",
	}, nil)
	if err != nil {
		_, _ = ws.Exec(ctx, []string{"openshell", "sandbox", "delete", name}, nil)
		return nil, err
	}
	if prep.Code != 0 {
		_, _ = ws.Exec(ctx, []string{"openshell", "sandbox", "delete", name}, nil)
		return nil, fmt.Errorf("openshell.Acquire: guest layout prep failed (exit %d): %s", prep.Code, prep.Stderr+prep.Stdout)
	}

	return buildLayer(name, c.guestPath, c.driver), nil
}

// errorLayer is a degenerate layer that surfaces a construction error on every
// primitive call — used when Layer is invoked without a sandbox name.
type errorLayer struct{ err error }

func (l errorLayer) ExecWrap(argv []string, opts env.ExecOpts) ([]string, env.ExecOpts) {
	return []string{"sh", "-c", "echo " + env.ShQuote(l.err.Error()) + " >&2; exit 1"}, env.ExecOpts{}
}
func (l errorLayer) CrossUpload(_, _ string) []string   { return nil }
func (l errorLayer) CrossDownload(_, _ string) []string { return nil }
func (l errorLayer) PathMap(_ env.PathKind) string      { return "" }
func (l errorLayer) Teardown() []string                 { return nil }

// posixBasename returns the last /-separated element of p.
func posixBasename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// posixDirname returns the directory portion of p (POSIX-only, /-separated).
func posixDirname(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

// openShellLayer is the set of layer primitives closed over a REAL sandbox name.
type openShellLayer struct {
	name      string
	guestRepo string
	driver    string
	// torndown makes Teardown idempotent: it emits the delete argv once, then nil.
	torndown bool
}

func buildLayer(name, guestRepo, driver string) *openShellLayer {
	return &openShellLayer{name: name, guestRepo: guestRepo, driver: driver}
}

func (l *openShellLayer) ExecWrap(argv []string, opts env.ExecOpts) ([]string, env.ExecOpts) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = l.guestRepo
	}
	wrapped := []string{
		"openshell", "sandbox", "exec", "-n", l.name, flagNoTTY, "--workdir", cwd, "--",
	}
	// openshell exec has no --env: cross env as an in-guest `env K=V` prefix,
	// omitted entirely when empty (a bare `env` would swallow argv[0]).
	if len(opts.Env) > 0 {
		wrapped = append(wrapped, envPrefix(opts.Env)...)
	}
	wrapped = append(wrapped, argv...)
	// cwd/env are CONSUMED into the wrapper (guest-side): passing them through
	// would set a guest path as the HOST cwd and leak guest env (possibly secrets)
	// into the host openshell process.
	rest := opts
	rest.Cwd = ""
	rest.Env = nil
	return wrapped, rest
}

// envPrefix builds the `env K=V …` argv prefix with deterministic key order,
// mirroring the core EnvPrefixedShell discipline but as a raw argv (no shell
// re-quoting, since these tokens ride as distinct argv elements).
func envPrefix(m map[string]string) []string {
	// Reuse the core helper for the sorted, shell-safe form, then re-split is
	// wrong here — instead we assemble argv tokens directly with sorted keys.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, 1+len(keys))
	out = append(out, "env")
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func (l *openShellLayer) CrossUpload(stagingPath, guestPath string) []string {
	// `openshell sandbox upload NAME SRC DEST` always NESTS: the tree lands at
	// DEST/<basename(SRC)> regardless of trailing slash/dot or whether DEST exists
	// (field-tested, 0.0.53) — but Compose requires guestPath to BECOME the copy
	// (mirroring local's cp semantics). So: upload into guest /tmp (nesting to the
	// collision-free staging basename), then move into place in-guest. Chained via
	// host `sh -c`; every embedded path is ShQuote'd, and the in-guest script rides
	// as ONE argv token.
	nested := "/tmp/" + posixBasename(stagingPath)
	move := "mkdir -p " + env.ShQuote(posixDirname(guestPath)) + " && " +
		"rm -rf " + env.ShQuote(guestPath) + " && " +
		"mv " + env.ShQuote(nested) + " " + env.ShQuote(guestPath)
	return []string{
		"sh", "-c",
		env.ArgvToShell([]string{"openshell", "sandbox", "upload", "--no-git-ignore", l.name, stagingPath, "/tmp"}) +
			" && " +
			env.ArgvToShell([]string{"openshell", "sandbox", "exec", "-n", l.name, flagNoTTY, "--", "sh", "-c", move}),
	}
}

func (l *openShellLayer) CrossDownload(guestPath, stagingPath string) []string {
	return []string{"openshell", "sandbox", "download", l.name, guestPath, stagingPath}
}

func (l *openShellLayer) PathMap(kind env.PathKind) string {
	switch kind {
	case env.PathRepo:
		return l.guestRepo
	case env.PathHome:
		return "/sandbox/.home"
	default:
		return "/tmp"
	}
}

func (l *openShellLayer) Teardown() []string {
	// The real openshell CLI errors on deleting an already-gone sandbox, and
	// Compose calls Teardown unconditionally on every destroy — idempotent
	// double-destroy therefore lives here: emit the delete argv once, then nil.
	if l.torndown {
		return nil
	}
	l.torndown = true
	return []string{"openshell", "sandbox", "delete", l.name}
}

// AliasMap folds the loopback rewrite on top of the inner's for a two-hop alias.
func (l *openShellLayer) AliasMap(hostURL string) string {
	out, err := ResolveGuestURL(hostURL, l.driver, "")
	if err != nil {
		// Best-effort alias: an unroutable URL is left untouched for the inner
		// workspace to handle (or fail on) downstream.
		return hostURL
	}
	return out
}
