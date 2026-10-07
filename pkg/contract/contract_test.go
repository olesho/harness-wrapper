package contract

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	for v, want := range map[string][2]int{
		"harness-adapter/1.0": {1, 0}, "harness-adapter/1.12": {1, 12}, "harness-adapter/2.3": {2, 3},
	} {
		ma, mi, err := ParseVersion(v)
		if err != nil || ma != want[0] || mi != want[1] {
			t.Errorf("ParseVersion(%q) = %d, %d, %v", v, ma, mi, err)
		}
	}
	for _, v := range []string{"", "harness-adapter/1", "harness-adapter/01.0", "harness-adapter/1.-1", "engine/1.0", "harness-adapter/0.1"} {
		if _, _, err := ParseVersion(v); err == nil {
			t.Errorf("ParseVersion(%q) accepted", v)
		}
	}
	if !Compatible(Version) || Compatible("harness-adapter/2.0") || Compatible("nonsense") {
		t.Error("Compatible: want only major 1")
	}
}

func TestModelsJSON(t *testing.T) {
	for in, want := range map[string]Models{`"any"`: {Any: true}, `["a","b"]`: {IDs: []string{"a", "b"}}, `[]`: {IDs: []string{}}} {
		var m Models
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if m.Any != want.Any || len(m.IDs) != len(want.IDs) {
			t.Errorf("%s: %+v", in, m)
		}
		out, _ := json.Marshal(m)
		if string(out) != in {
			t.Errorf("round trip of %s: %s", in, out)
		}
	}
	var m Models
	if json.Unmarshal([]byte(`"some"`), &m) == nil {
		t.Error(`"some" accepted as models`)
	}
	if !(Models{IDs: []string{"x"}}).Allows("") || (Models{IDs: []string{"x"}}).Allows("y") || !(Models{Any: true}).Allows("y") {
		t.Error("Allows")
	}
}

// An agent has as many Sessions open at once as the Descriptor's limit with
// concurrent_sessions, and one without; a limit is declared with the
// capability alone, and is at least 2.
func TestCheckSessions(t *testing.T) {
	side := Descriptor{Capabilities: []Capability{CapConcurrentSessions}, Limits: Limits{MaxSessions: 8}}
	if err := CheckSessions(side); err != nil || side.Sessions() != 8 {
		t.Errorf("8 side by side: %v, %d Sessions", err, side.Sessions())
	}
	if err := CheckSessions(Descriptor{}); err != nil || (Descriptor{}).Sessions() != 1 {
		t.Errorf("no capability, no limit: %v, %d Sessions", err, (Descriptor{}).Sessions())
	}
	for name, d := range map[string]Descriptor{
		"no limit":           {Capabilities: []Capability{CapConcurrentSessions}},
		"one":                {Capabilities: []Capability{CapConcurrentSessions}, Limits: Limits{MaxSessions: 1}},
		"without capability": {Limits: Limits{MaxSessions: 4}},
	} {
		if err := CheckSessions(d); CodeOf(err) != CodeProtocol {
			t.Errorf("%s: %v, want protocol", name, err)
		}
		if d.Sessions() != 1 {
			t.Errorf("%s: %d Sessions, want 1", name, d.Sessions())
		}
	}
}

func testDescriptor() Descriptor {
	return Descriptor{
		Contract: Version, Harness: HarnessInfo{Name: "h", Version: "1", Adapter: "a"},
		CredentialKinds: []string{"tok"},
		Spec: SpecSupport{
			Models: Models{IDs: []string{"m"}}, Efforts: []string{"high"},
			Instructions: []string{InstructionPersona}, Skills: true,
			Connectors: []string{ConnectorStdio}, PermissionPostures: []string{PostureBypass},
		},
	}
}

func TestMinorOf(t *testing.T) {
	if m, err := MinorOf(Version); err != nil || m != Minor {
		t.Errorf("MinorOf(%q) = %d, %v, want %d", Version, m, err, Minor)
	}
	if m, err := MinorOf("harness-adapter/1.2"); err != nil || m != 2 {
		t.Errorf("MinorOf(1.2) = %d, %v", m, err)
	}
	if _, err := MinorOf("harness-adapter/1"); err == nil {
		t.Error("MinorOf accepted a version with no minor")
	}
}

// Every capability of the set was added by a minor this package defines, and
// one outside it by none.
func TestCapabilitySince(t *testing.T) {
	for _, v := range Capability("").Values() {
		if s := Capability(v).Since(); s < 0 || s > Minor {
			t.Errorf("%s.Since() = %d, want 0..%d", v, s, Minor)
		}
	}
	if s := Capability("telepathy").Since(); s != -1 {
		t.Errorf("an unknown capability's Since = %d, want -1", s)
	}
	for c, want := range map[Capability]int{CapRateLimits: 0, CapAutonomousTurns: 1, CapLoginKeeper: 2, CapBackgroundTurns: 3, CapConcurrentSessions: 6} {
		if s := c.Since(); s != want {
			t.Errorf("%s.Since() = %d, want %d", c, s, want)
		}
	}
}

func TestCheckCapabilities(t *testing.T) {
	d := func(v string, caps ...Capability) Descriptor { return Descriptor{Contract: v, Capabilities: caps} }
	for _, ok := range []Descriptor{
		d(Version, CapResume, CapSessionLoad, CapConcurrentSessions),
		d("harness-adapter/1.0", CapResume, CapRateLimits),
		d("harness-adapter/1.3", CapBackgroundTurns, CapLoginKeeper),
		d("harness-adapter/1.2", "telepathy"),
	} {
		if err := CheckCapabilities(ok); err != nil {
			t.Errorf("%s %v: %v", ok.Contract, ok.Capabilities, err)
		}
	}
	for _, bad := range []Descriptor{
		d("harness-adapter/1.2", CapResume, CapConcurrentSessions),
		d("harness-adapter/1.0", CapSessionLoad),
		d("harness-adapter/1.2", CapBackgroundTurns),
		d("harness-adapter/1", CapResume),
	} {
		var e *Error
		if err := CheckCapabilities(bad); !errors.As(err, &e) || e.Code != CodeProtocol {
			t.Errorf("%s %v: %v, want protocol", bad.Contract, bad.Capabilities, err)
		}
	}
}

func TestCheckSpec(t *testing.T) {
	d := testDescriptor()
	ok := AgentSpec{
		Model: "m", Effort: "high", Instructions: Instructions{Persona: "p"},
		Skills:     []Skill{{Name: "s", Files: []SkillFile{{Path: "SKILL.md"}, {Path: "ref/a.md"}}}},
		Connectors: []Connector{{Name: "c", Stdio: &StdioConnector{Command: "/bin/x"}}},
		Credential: &CredentialRef{Kind: "tok"}, PermissionPosture: PostureBypass,
	}
	if err := CheckSpec(d, ok); err != nil {
		t.Fatalf("a supported spec: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*AgentSpec)
		code   Code
		field  string
	}{
		"model":        {func(s *AgentSpec) { s.Model = "other" }, CodeUnsupported, "model"},
		"effort":       {func(s *AgentSpec) { s.Effort = "max" }, CodeUnsupported, "effort"},
		"workspace":    {func(s *AgentSpec) { s.Instructions.Workspace = "w" }, CodeUnsupported, "instructions.workspace"},
		"memory":       {func(s *AgentSpec) { s.Memory = &Memory{} }, CodeUnsupported, "memory"},
		"http":         {func(s *AgentSpec) { s.Connectors[0] = Connector{Name: "c", HTTP: &HTTPConnector{URL: "u"}} }, CodeUnsupported, "connectors[0].http"},
		"both":         {func(s *AgentSpec) { s.Connectors[0].HTTP = &HTTPConnector{URL: "u"} }, CodeInvalidSpec, "connectors[0]"},
		"credential":   {func(s *AgentSpec) { s.Credential.Kind = "other" }, CodeUnsupported, "credential.kind"},
		"posture":      {func(s *AgentSpec) { s.PermissionPosture = "gated" }, CodeUnsupported, "permission_posture"},
		"no SKILL.md":  {func(s *AgentSpec) { s.Skills[0].Files = []SkillFile{{Path: "a.md"}} }, CodeInvalidSpec, "skills[0]"},
		"escape":       {func(s *AgentSpec) { s.Skills[0].Files[1].Path = "../x" }, CodeInvalidSpec, "skills[0].files[1].path"},
		"skill twice":  {func(s *AgentSpec) { s.Skills = append(s.Skills, s.Skills[0]) }, CodeInvalidSpec, "skills[1].name"},
		"bad name":     {func(s *AgentSpec) { s.Connectors[0].Name = "a b" }, CodeInvalidSpec, "connectors[0].name"},
		"bad posture":  {func(s *AgentSpec) { s.PermissionPosture = "yolo" }, CodeInvalidSpec, "permission_posture"},
		"no command":   {func(s *AgentSpec) { s.Connectors[0].Stdio.Command = "" }, CodeInvalidSpec, "connectors[0].stdio.command"},
		"persona gone": {func(s *AgentSpec) { s.Instructions.Persona = ""; s.Instructions.Workspace = "w" }, CodeUnsupported, "instructions.workspace"},
	} {
		s := ok
		s.Skills = append([]Skill(nil), ok.Skills...)
		s.Skills[0].Files = append([]SkillFile(nil), ok.Skills[0].Files...)
		s.Connectors = append([]Connector(nil), ok.Connectors...)
		st := *ok.Connectors[0].Stdio
		s.Connectors[0].Stdio = &st
		cred := *ok.Credential
		s.Credential = &cred
		tc.mutate(&s)
		err := CheckSpec(d, s)
		var e *Error
		if !errors.As(err, &e) || e.Code != tc.code || e.Field != tc.field {
			t.Errorf("%s: %v, want %s on %s", name, err, tc.code, tc.field)
		}
	}
}

func TestCheckRelPath(t *testing.T) {
	for _, p := range []string{"a", "a/b.md", "SKILL.md", "x.y/z"} {
		if err := CheckRelPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "/a", "../a", "a/../b", "a//b", "./a", "a/", `a\b`, "a/./b"} {
		if CheckRelPath(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestProvisionResultValidate(t *testing.T) {
	good := ProvisionResult{Files: []File{TextFile(RootConfig, "a.json", "0600", "{}")}, OpenConfig: []byte("{}")}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]ProvisionResult{
		"secrets root": {Files: []File{TextFile(RootSecrets, "t", "0600", "")}},
		"escape":       {Files: []File{TextFile(RootConfig, "../t", "0600", "")}},
		"wide mode":    {Files: []File{TextFile(RootConfig, "t", "0666", "")}},
		"setuid":       {Files: []File{TextFile(RootConfig, "t", "4644", "")}},
		"not octal":    {Files: []File{TextFile(RootConfig, "t", "rw", "")}},
		"twice":        {Files: []File{TextFile(RootConfig, "t", "0600", ""), TextFile(RootConfig, "t", "0600", "")}},
		"both":         {Files: []File{{Root: RootConfig, Path: "t", Mode: "0600", Text: new(string), Bytes: []byte("x")}}},
		"neither":      {Files: []File{{Root: RootConfig, Path: "t", Mode: "0600"}}},
		"big config":   {OpenConfig: make([]byte, MaxOpenConfigBytes+1)},
		"bad history":  {HistoryRoots: []RootPath{{Root: RootSecrets, Path: "x"}}},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLayoutValidate(t *testing.T) {
	l := Layout{Home: "/h", Config: "/c", Workspace: "/w", Secrets: "/s", Scratch: "/x"}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Layout){
		"relative": func(l *Layout) { l.Home = "h" },
		"unclean":  func(l *Layout) { l.Config = "/c/../c" },
		"empty":    func(l *Layout) { l.Scratch = "" },
		"shared":   func(l *Layout) { l.Secrets = "/h" },
	} {
		m := l
		mutate(&m)
		if m.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInputValidate(t *testing.T) {
	if err := Text("in_1", "hi").Validate(10); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]Input{
		"no id":      {Content: []ContentPart{{Type: ContentText, Text: "x"}}},
		"no content": {InputID: "in_1"},
		"empty text": {InputID: "in_1", Content: []ContentPart{{Type: ContentText}}},
		"image":      {InputID: "in_1", Content: []ContentPart{{Type: "image", Text: "x"}}},
		"too big":    Text("in_1", strings.Repeat("x", 11)),
	} {
		err := in.Validate(10)
		if err == nil || CertaintyOf(err) != NotSubmitted {
			t.Errorf("%s: %v, want a not_submitted refusal", name, err)
		}
	}
}

func TestOpenRequestValidate(t *testing.T) {
	cp := &Checkpoint{Format: 1, Data: []byte("x")}
	for name, tc := range map[string]struct {
		req OpenRequest
		ok  bool
	}{
		"fresh":             {OpenRequest{Mode: OpenFresh}, true},
		"fresh with id":     {OpenRequest{Mode: OpenFresh, SessionID: "s1"}, true},
		"fresh checkpoint":  {OpenRequest{Mode: OpenFresh, Checkpoint: cp}, false},
		"reopen":            {OpenRequest{Mode: OpenReopen, SessionID: "s1", Checkpoint: cp}, true},
		"reopen without id": {OpenRequest{Mode: OpenReopen}, false},
		"bad mode":          {OpenRequest{Mode: "resume"}, false},
		"bad id":            {OpenRequest{Mode: OpenFresh, SessionID: "a b"}, false},
		"bad checkpoint":    {OpenRequest{Mode: OpenReopen, SessionID: "s1", Checkpoint: &Checkpoint{}}, false},
	} {
		if err := tc.req.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestErrors(t *testing.T) {
	err := error(&Error{Code: CodeOpenFailed, Reason: OpenBinaryNotFound, Message: "no claude"})
	if !errors.Is(err, &Error{Code: CodeOpenFailed}) || !errors.Is(err, &Error{Code: CodeOpenFailed, Reason: OpenBinaryNotFound}) {
		t.Error("errors.Is by code and reason")
	}
	if errors.Is(err, &Error{Code: CodeOpenFailed, Reason: OpenAuthRequired}) || errors.Is(err, &Error{Code: CodeBusy}) {
		t.Error("errors.Is matched another code or reason")
	}
	if CodeOf(errors.New("x")) != CodeInternal || CodeOf(err) != CodeOpenFailed {
		t.Error("CodeOf")
	}
	if CertaintyOf(errors.New("lost")) != MaybeSubmitted || CertaintyOf(&Error{Code: CodeBusy, Certainty: NotSubmitted}) != NotSubmitted {
		t.Error("CertaintyOf: a lost result is maybe_submitted")
	}
	if !strings.Contains(err.Error(), "binary_not_found") {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestChoiceValidate(t *testing.T) {
	for _, c := range []Choice{{OptionID: "a"}, {OptionIDs: []string{"a"}}, {Text: "t"}} {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []Choice{{}, {OptionID: "a", Text: "t"}} {
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestObservationID(t *testing.T) {
	o := NewObservation(KindToolStarted, "tu_1", OriginLive, testTime, ToolUseData{ToolUseID: "tu_1", Name: "Bash"})
	if o.ID != "tool_started:tu_1" || o.Key() != "tu_1" {
		t.Errorf("id %q key %q", o.ID, o.Key())
	}
	var d ToolUseData
	if err := o.Decode(&d); err != nil || d.Name != "Bash" {
		t.Errorf("decode: %+v %v", d, err)
	}
	if KindToolStarted.Capability() != CapToolsObserved || KindTurnEnded.Capability() != "" {
		t.Error("Kind.Capability")
	}
}

type nopAdapter struct{ Adapter }

func TestRegistry(t *testing.T) {
	Register("registry-test", nopAdapter{})
	if _, ok := Lookup("registry-test"); !ok {
		t.Fatal("Lookup after Register")
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup of an unregistered name")
	}
	found := false
	for _, n := range Names() {
		found = found || n == "registry-test"
	}
	if !found {
		t.Error("Names lacks a registered name")
	}
	defer func() {
		if recover() == nil {
			t.Error("a second Register under one name did not panic")
		}
	}()
	Register("registry-test", nopAdapter{})
}

// The contract links nothing but the standard library: a runtime or a
// framework program imports it without linking any of harness-wrapper's
// harness code.
func TestStandardLibraryOnly(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	out, err := exec.Command(goTool, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(out)) {
		if p != "github.com/olesho/harness-wrapper/pkg/contract" {
			t.Errorf("pkg/contract depends on %s", p)
		}
	}
}

var testTime = mustTime("2026-09-28T12:00:00Z")

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// An http connector's headers are tokens, each named once across headers,
// headers_env and headers_file in any case, and a header's file is an
// absolute path.
func TestCheckSpecHeaders(t *testing.T) {
	d := testDescriptor()
	d.Spec.Connectors = []string{ConnectorStdio, ConnectorHTTP}
	spec := func(h HTTPConnector) AgentSpec {
		h.URL = "https://mcp.example.com/mcp"
		return AgentSpec{Model: "m", Connectors: []Connector{{Name: "c", HTTP: &h}}, PermissionPosture: PostureBypass}
	}
	if err := CheckSpec(d, spec(HTTPConnector{
		Headers: map[string]string{"X-Team": "t"}, HeadersEnv: map[string]string{"X-Env": "V"}, HeadersFile: map[string]string{"Authorization": "/s/auth"},
	})); err != nil {
		t.Fatalf("well-formed headers: %v", err)
	}
	for name, tc := range map[string]struct {
		h     HTTPConnector
		field string
	}{
		"bad header name": {HTTPConnector{HeadersFile: map[string]string{"X Key": "/s/k"}}, "connectors[0].http.headers_file"},
		"named twice":     {HTTPConnector{Headers: map[string]string{"authorization": "a"}, HeadersFile: map[string]string{"Authorization": "/s/k"}}, "connectors[0].http.headers_file"},
		"relative file":   {HTTPConnector{HeadersFile: map[string]string{"Authorization": "secrets/k"}}, "connectors[0].http.headers_file"},
		"unclean file":    {HTTPConnector{HeadersFile: map[string]string{"Authorization": "/s/../k"}}, "connectors[0].http.headers_file"},
		"bad env header":  {HTTPConnector{HeadersEnv: map[string]string{"X:Y": "V"}}, "connectors[0].http.headers_env"},
	} {
		var e *Error
		if err := CheckSpec(d, spec(tc.h)); !errors.As(err, &e) || e.Code != CodeInvalidSpec || e.Field != tc.field {
			t.Errorf("%s: %v, want invalid_spec on %s", name, err, tc.field)
		}
	}
}
