package openshell

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/env"
)

// ---- GeneratePolicy golden ----

func TestGeneratePolicy_Golden(t *testing.T) {
	got, err := GeneratePolicy(PolicyScopes{
		Tier:        "untrusted",
		ModelHost:   "api.anthropic.com",
		FleetHost:   "localhost",
		FleetPort:   53343,
		HarnessPath: "/usr/local/bin/harness-wrapper",
		ScrapeEndpoints: []ScrapeEndpoint{
			{Host: "example.com", Binaries: []string{"/usr/bin/curl", "/opt/br/chrome"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "version: 1\n" +
		"filesystem_policy:\n" +
		"  include_workdir: false\n" +
		"  read_only: ['/usr', '/lib', '/lib64', '/etc', '/bin', '/sbin', '/opt']\n" +
		"  read_write: [/sandbox, /tmp]\n" +
		"process: { run_as_user: sandbox, run_as_group: sandbox }\n" +
		"landlock: { compatibility: best_effort }\n" +
		"network_policies:\n" +
		"  model:\n" +
		"    endpoints: [{ host: api.anthropic.com, port: 443, protocol: rest, access: full, enforcement: enforce }]\n" +
		"    binaries: [{ path: /usr/local/bin/claude }]\n" +
		"  fleet:\n" +
		"    endpoints: [{ host: localhost, port: 53343, protocol: rest, access: full, enforcement: enforce }]\n" +
		"    binaries: [{ path: /usr/local/bin/harness-wrapper }, { path: /usr/local/bin/orche }]\n" +
		"  scrape_0:\n" +
		"    endpoints: [{ host: example.com, port: 443 }]\n" +
		"    binaries: [{ path: /usr/bin/curl }, { path: /opt/br/chrome }]\n" +
		"  # git hub: bundle-out ⇒ NO network endpoint\n"
	if got != want {
		t.Errorf("policy mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGeneratePolicy_NoScrapeLaneWhenEmpty(t *testing.T) {
	// Absent scrape endpoints ⇒ NO scrape lane emitted (additive-unchanged
	// property from index.ts:205).
	got, err := GeneratePolicy(PolicyScopes{
		Tier: "semi-trusted", ModelHost: "m", FleetHost: "f", FleetPort: 1, HarnessPath: "/h",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "scrape_") {
		t.Errorf("expected no scrape lane, got:\n%s", got)
	}
	// A single empty slice must be byte-for-byte equal to nil.
	got2, _ := GeneratePolicy(PolicyScopes{
		Tier: "semi-trusted", ModelHost: "m", FleetHost: "f", FleetPort: 1, HarnessPath: "/h",
		ScrapeEndpoints: []ScrapeEndpoint{},
	})
	if got != got2 {
		t.Errorf("empty slice changed output:\n%q\nvs\n%q", got, got2)
	}
}

func TestGeneratePolicy_TierKnobs(t *testing.T) {
	cases := []struct {
		tier         string
		wantReadOnly string
		wantEnforce  string
	}{
		{"untrusted", "['/usr', '/lib', '/lib64', '/etc', '/bin', '/sbin', '/opt']", "enforce"},
		{"semi-trusted", "['/usr', '/lib', '/etc', '/bin']", "enforce"},
		{"trusted-internal", "['/usr', '/lib']", "observe"},
	}
	for _, c := range cases {
		got, err := GeneratePolicy(PolicyScopes{
			Tier: c.tier, ModelHost: "m", FleetHost: "f", FleetPort: 1, HarnessPath: "/h",
		})
		if err != nil {
			t.Fatalf("tier %q: unexpected error: %v", c.tier, err)
		}
		if !strings.Contains(got, "read_only: "+c.wantReadOnly) {
			t.Errorf("tier %q: missing read_only %q in:\n%s", c.tier, c.wantReadOnly, got)
		}
		if !strings.Contains(got, "enforcement: "+c.wantEnforce+" }") {
			t.Errorf("tier %q: missing enforcement %q in:\n%s", c.tier, c.wantEnforce, got)
		}
	}
}

func TestGeneratePolicy_UnknownTier(t *testing.T) {
	if _, err := GeneratePolicy(PolicyScopes{Tier: "wat"}); err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

func TestGeneratePolicy_ModelPortDefault(t *testing.T) {
	got, _ := GeneratePolicy(PolicyScopes{
		Tier: "untrusted", ModelHost: "m", FleetHost: "f", FleetPort: 1, HarnessPath: "/h",
	})
	if !strings.Contains(got, "host: m, port: 443,") {
		t.Errorf("expected default model port 443, got:\n%s", got)
	}
}

func TestGeneratePolicy_MultipleScrapeLanes(t *testing.T) {
	got, _ := GeneratePolicy(PolicyScopes{
		Tier: "untrusted", ModelHost: "m", FleetHost: "f", FleetPort: 1, HarnessPath: "/h",
		ScrapeEndpoints: []ScrapeEndpoint{
			{Host: "a.com", Port: 8443, Binaries: []string{"/a"}},
			{Host: "b.com", Binaries: []string{"/b1", "/b2"}},
		},
	})
	if !strings.Contains(got, "scrape_0:") || !strings.Contains(got, "scrape_1:") {
		t.Errorf("expected two scrape lanes, got:\n%s", got)
	}
	if !strings.Contains(got, "host: a.com, port: 8443") {
		t.Errorf("expected explicit port 8443, got:\n%s", got)
	}
	if !strings.Contains(got, "host: b.com, port: 443") {
		t.Errorf("expected default port 443 for second lane, got:\n%s", got)
	}
}

// ---- SandboxName determinism & collision ----

func TestSandboxName_Basic(t *testing.T) {
	cases := map[string]string{
		"agent-1":            "openshell-agent-1",
		"Agent_ONE":          "openshell-agent-one",
		"  weird!!name  ":    "openshell-weird-name",
		"---leading-dash---": "openshell-leading-dash",
	}
	for in, want := range cases {
		if got := SandboxName(in); got != want {
			t.Errorf("SandboxName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSandboxName_Deterministic(t *testing.T) {
	id := "some-agent-id-42"
	a := SandboxName(id)
	b := SandboxName(id)
	if a != b {
		t.Fatal("SandboxName is not deterministic")
	}
}

func TestSandboxName_LengthBoundedWithHash(t *testing.T) {
	long := strings.Repeat("abcde-", 20) // way over 40
	got := SandboxName(long)
	if len(got) > 40 {
		t.Errorf("SandboxName too long: %d (%q)", len(got), got)
	}
	if !strings.HasPrefix(got, "openshell-") {
		t.Errorf("missing prefix: %q", got)
	}
	// Hash suffix present (8 hex chars after the last dash).
	last := got[strings.LastIndex(got, "-")+1:]
	if len(last) != 8 {
		t.Errorf("expected 8-char hash suffix, got %q in %q", last, got)
	}
	// Deterministic under truncation.
	if SandboxName(long) != got {
		t.Error("truncated name is not deterministic")
	}
}

func TestSandboxName_CollisionResistant(t *testing.T) {
	// Two long ids sharing a slug prefix must NOT collide (distinct hash suffix).
	a := strings.Repeat("x", 100) + "-alpha"
	b := strings.Repeat("x", 100) + "-beta"
	if SandboxName(a) == SandboxName(b) {
		t.Errorf("distinct ids collided: %q", SandboxName(a))
	}
}

func TestSandboxName_ExactBoundary(t *testing.T) {
	// slug length that lands exactly at MAX (40) keeps the plain form (no hash).
	// prefix "openshell-" is 10, so a 30-char slug hits exactly 40.
	slug := strings.Repeat("a", 30)
	got := SandboxName(slug)
	if got != "openshell-"+slug {
		t.Errorf("boundary name = %q, want plain form", got)
	}
	if len(got) != 40 {
		t.Errorf("boundary length = %d, want 40", len(got))
	}
}

// ---- ResolveGuestURL parsing ----

func TestResolveGuestURL_Override(t *testing.T) {
	got, err := ResolveGuestURL("http://localhost:1234", "none", "  http://override:9  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://override:9" {
		t.Errorf("override not honored/trimmed: %q", got)
	}
}

func TestResolveGuestURL_NonLoopbackUnchanged(t *testing.T) {
	in := "https://api.anthropic.com/v1"
	got, err := ResolveGuestURL(in, "container", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != in {
		t.Errorf("non-loopback URL changed: %q", got)
	}
}

func TestResolveGuestURL_LoopbackRewrite(t *testing.T) {
	cases := []struct {
		host, driver, want string
	}{
		{"http://localhost:53343", "container", "http://host.docker.internal:53343"},
		{"http://127.0.0.1:8080/x", "docker", "http://host.docker.internal:8080/x"},
		{"http://0.0.0.0:9/p", "podman", "http://host.containers.internal:9/p"},
	}
	for _, c := range cases {
		got, err := ResolveGuestURL(c.host, c.driver, "")
		if err != nil {
			t.Fatalf("%q/%q: unexpected error: %v", c.host, c.driver, err)
		}
		if got != c.want {
			t.Errorf("ResolveGuestURL(%q, %q) = %q, want %q", c.host, c.driver, got, c.want)
		}
	}
}

func TestResolveGuestURL_TrailingSlashStripped(t *testing.T) {
	got, err := ResolveGuestURL("http://localhost/", "container", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://host.docker.internal" {
		t.Errorf("trailing slash not stripped: %q", got)
	}
}

func TestResolveGuestURL_LoopbackNoAlias(t *testing.T) {
	if _, err := ResolveGuestURL("http://localhost:1", "none", ""); err == nil {
		t.Fatal("expected error: loopback with no routable driver")
	}
}

func TestResolveGuestURL_InvalidURL(t *testing.T) {
	if _, err := ResolveGuestURL("::not a url::", "container", ""); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

// ---- Layer primitives (pure, no live gateway) ----

func TestLayer_ExecWrapEnvPrefix(t *testing.T) {
	l := buildLayer("openshell-test", "/sandbox/repo", "container")
	argv, rest := l.ExecWrap([]string{"echo", "hi"}, env.ExecOpts{
		Env: map[string]string{"B": "2", "A": "1"}, Cwd: "/work",
	})
	want := []string{
		"openshell", "sandbox", "exec", "-n", "openshell-test", "--no-tty", "--workdir", "/work", "--",
		"env", "A=1", "B=2", "echo", "hi",
	}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("ExecWrap argv = %v, want %v", argv, want)
	}
	// cwd/env are consumed into the wrapper.
	if rest.Cwd != "" || rest.Env != nil {
		t.Errorf("cwd/env not consumed: %+v", rest)
	}
}

func TestLayer_ExecWrapNoEnv(t *testing.T) {
	l := buildLayer("s", "/sandbox/repo", "container")
	argv, _ := l.ExecWrap([]string{"ls"}, env.ExecOpts{})
	// No env ⇒ no `env` prefix (a bare env would swallow argv[0]).
	for _, tok := range argv {
		if tok == "env" {
			t.Fatalf("unexpected env prefix: %v", argv)
		}
	}
	// defaults workdir to the repo path.
	if !strings.Contains(strings.Join(argv, " "), "--workdir /sandbox/repo --") {
		t.Errorf("expected default workdir, got %v", argv)
	}
}

func TestLayer_TeardownIdempotent(t *testing.T) {
	l := buildLayer("openshell-x", "/sandbox/repo", "container")
	first := l.Teardown()
	if len(first) == 0 || first[len(first)-1] != "openshell-x" {
		t.Errorf("first teardown wrong: %v", first)
	}
	if second := l.Teardown(); second != nil {
		t.Errorf("second teardown must be nil, got %v", second)
	}
}

func TestLayer_PathMap(t *testing.T) {
	l := buildLayer("s", "/sandbox/repo", "container")
	if l.PathMap(env.PathRepo) != "/sandbox/repo" {
		t.Error("repo path wrong")
	}
	if l.PathMap(env.PathHome) != "/sandbox/.home" {
		t.Error("home path wrong")
	}
	if l.PathMap(env.PathTmp) != "/tmp" {
		t.Error("tmp path wrong")
	}
}

func TestLayer_CrossUploadNestThenMove(t *testing.T) {
	l := buildLayer("s", "/sandbox/repo", "container")
	argv := l.CrossUpload("/host/staging/tree", "/sandbox/repo/sub")
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("expected sh -c form, got %v", argv)
	}
	script := argv[2]
	// The inner move script is ShQuote-nested as one argv token, so its own single
	// quotes are escaped. Check the durable pieces survive: the upload step, the
	// staged /tmp nest basename, and the destination path.
	if !strings.Contains(script, "'upload'") || !strings.Contains(script, "/tmp/tree") || !strings.Contains(script, "/sandbox/repo/sub") {
		t.Errorf("cross-upload script wrong: %s", script)
	}
}

func TestLayer_AliasMap(t *testing.T) {
	l := buildLayer("s", "/sandbox/repo", "container")
	if got := l.AliasMap("http://localhost:1/x"); got != "http://host.docker.internal:1/x" {
		t.Errorf("AliasMap = %q", got)
	}
	// Unroutable ⇒ pass-through, no panic.
	if got := l.AliasMap("http://localhost:1"); got != "http://host.docker.internal:1" {
		t.Errorf("AliasMap routable = %q", got)
	}
}

func TestOpenShell_Defaults(t *testing.T) {
	c := OpenShell(Options{})
	if c.Name() != "openshell" {
		t.Errorf("name = %q", c.Name())
	}
	if c.driver != "container" || c.provider != "anthropic" || c.guestPath != "/sandbox/repo" {
		t.Errorf("defaults wrong: %+v", c)
	}
}

func TestOpenShell_LayerNoName(t *testing.T) {
	c := OpenShell(Options{})
	// No sandbox name ⇒ a loud error layer, not a nil deref.
	l := c.Layer(env.PolicySpec{})
	argv, _ := l.ExecWrap([]string{"x"}, env.ExecOpts{})
	if len(argv) == 0 || !strings.Contains(strings.Join(argv, " "), "no sandbox name") {
		t.Errorf("expected error-layer argv, got %v", argv)
	}
}
