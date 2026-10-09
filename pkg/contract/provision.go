package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ProvisionRequest is what the Supervisor asks an adapter to render.
type ProvisionRequest struct {
	// Contract is the version the request is written in.
	Contract string `json:"contract"`
	// HarnessRoot is the absolute, read-only root of the pinned harness
	// distribution — its hook helper and vendor binary — supplied by the
	// Supervisor from the Runtime's installation metadata. Provision may
	// render paths beneath it without inspecting the filesystem.
	HarnessRoot string `json:"harness_root"`
	// Layout is the agent's writable roots.
	Layout Layout `json:"layout"`
	// Spec is the harness-neutral Agent Spec.
	Spec AgentSpec `json:"spec"`
	// Load, when set, says the environment is one a saved Session is loaded
	// into (capability session_load): the result then carries the
	// relocations that take the Session's history to where the harness looks
	// for it here. Provision refuses a source its Descriptor's Load does not
	// name, with CodeUnsupported.
	Load *LoadSource `json:"load,omitempty"`
}

// LoadSource is where a saved Session comes from: what its archive's metadata
// says of the environment it was saved in.
type LoadSource struct {
	// Format is the archive's format.
	Format int `json:"format"`
	// Harness is the saving Descriptor's harness: its name, its version then,
	// and the adapter that ran it.
	Harness HarnessInfo `json:"harness"`
	// Layout is the source environment's roots, as they were there.
	Layout Layout `json:"layout"`
	// Workspace is the source's workspace as the harness resolved it, every
	// symlink followed: a harness that names its record for its working
	// directory named it for this path, and the new environment cannot
	// resolve a path of a machine that is gone. Layout's roots, in a request
	// that loads, are resolved paths too: Provision resolves none.
	Workspace string `json:"workspace"`
}

// Layout is an agent's roots. They are distinct, absolute and clean, exist
// before the Host starts, and are writable only by the agent's workload
// identity.
type Layout struct {
	// Home is the agent's home directory.
	Home string `json:"home"`
	// Config is the harness's configuration root.
	Config string `json:"config"`
	// Workspace is where the agent works: its repositories.
	Workspace string `json:"workspace"`
	// Secrets holds the staged credential files.
	Secrets string `json:"secrets"`
	// Scratch is the adapter's private durable side area: submission
	// markers, and for harness-wrapper the hook spool.
	Scratch string `json:"scratch"`
}

// Roots names a Layout's roots.
type Root string

// The roots a file or reference may name.
const (
	RootHome      Root = "home"
	RootConfig    Root = "config"
	RootWorkspace Root = "workspace"
	RootSecrets   Root = "secrets"
	RootScratch   Root = "scratch"
)

// Values lists the set.
func (Root) Values() []string { return []string{"home", "config", "workspace", "secrets", "scratch"} }

// Path is root's directory in l, "" for an unknown root.
func (l Layout) Path(root Root) string {
	switch root {
	case RootHome:
		return l.Home
	case RootConfig:
		return l.Config
	case RootWorkspace:
		return l.Workspace
	case RootSecrets:
		return l.Secrets
	case RootScratch:
		return l.Scratch
	}
	return ""
}

// Validate checks that every root is a clean absolute path, and that no two
// are the same.
func (l Layout) Validate() error {
	seen := map[string]Root{}
	for _, r := range []Root{RootHome, RootConfig, RootWorkspace, RootSecrets, RootScratch} {
		p := l.Path(r)
		if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return &Error{Code: CodeInvalidSpec, Field: "layout." + string(r), Message: fmt.Sprintf("%q is not a clean absolute path", p)}
		}
		if other, dup := seen[p]; dup {
			return &Error{Code: CodeInvalidSpec, Field: "layout." + string(r), Message: fmt.Sprintf("same directory as %s", other)}
		}
		seen[p] = r
	}
	return nil
}

// AgentSpec is the harness-neutral definition of an agent. Provision refuses
// a field the harness's Descriptor does not name, with CodeUnsupported.
type AgentSpec struct {
	// Model is the model id; empty is the harness's default.
	Model string `json:"model,omitempty"`
	// Effort is the reasoning effort; empty is the harness's default.
	Effort string `json:"effort,omitempty"`
	// Instructions are the agent's standing instructions.
	Instructions Instructions `json:"instructions,omitempty"`
	// Skills are installed where the harness finds them.
	Skills []Skill `json:"skills,omitempty"`
	// Connectors are the agent's tool servers.
	Connectors []Connector `json:"connectors,omitempty"`
	// Memory, when set, gives the agent a memory directory seeded with its
	// files.
	Memory *Memory `json:"memory,omitempty"`
	// PermissionPosture is bypass (the harness runs every tool; isolation is
	// external) or gated (needs prompts).
	PermissionPosture string `json:"permission_posture,omitempty"`
	// Credential names the kind of the credential Open will stage; nil for a
	// harness that needs none.
	Credential *CredentialRef `json:"credential,omitempty"`
	// VersionPolicy is how strictly Open holds the harness binary to the
	// version the adapter pins (Descriptor.Harness.Version): strict refuses
	// any other, flexible — and empty, the default — runs whatever version
	// is installed, keeping every check of what the adapter relies on (1.7).
	// Every adapter of 1.7 or later honours it, behind no capability; a
	// request of an earlier minor carries none (CheckMinor).
	VersionPolicy VersionPolicy `json:"version_policy,omitempty"`
}

// VersionPolicy is how strictly a Session's harness is held to the adapter's
// pinned version (1.7).
type VersionPolicy string

// Version policies.
const (
	// VersionStrict: Open refuses a harness binary whose version is not the
	// pin, or cannot be learned, with open_failed version_unsupported.
	VersionStrict VersionPolicy = "strict"
	// VersionFlexible: Open checks no version. Whatever else the adapter
	// checks of the harness's protocol or capabilities it still checks,
	// failing with the reason it always did. An empty policy is flexible.
	VersionFlexible VersionPolicy = "flexible"
)

// Values lists the set.
func (VersionPolicy) Values() []string { return []string{"strict", "flexible"} }

// VersionPolicySince is the minor that added AgentSpec.VersionPolicy.
const VersionPolicySince = 7

// Valid reports whether p is in the set, or empty.
func (p VersionPolicy) Valid() bool {
	return p == "" || p == VersionStrict || p == VersionFlexible
}

// Admits reports whether a Session under p may run the harness at version
// running, "" when it could not be learned, when the adapter pins pinned:
// under strict only the pin, under flexible any version.
func (p VersionPolicy) Admits(pinned, running string) bool {
	return p != VersionStrict || (running != "" && running == pinned)
}

// CheckMinor refuses a request that uses what a newer minor than the one it
// is written in added, with CodeProtocol naming the field: a caller sends an
// adapter requests no newer than the adapter's minor (Compatible), and an
// adapter of that minor would drop such a field unread.
func CheckMinor(req ProvisionRequest) error {
	minor, err := MinorOf(req.Contract)
	if err != nil {
		return &Error{Code: CodeProtocol, Field: "contract", Message: err.Error()}
	}
	if req.Spec.VersionPolicy != "" && minor < VersionPolicySince {
		return &Error{Code: CodeProtocol, Field: "spec.version_policy", Message: fmt.Sprintf("from %s%d.%d, and the request is %s", versionPrefix, Major, VersionPolicySince, req.Contract)}
	}
	return nil
}

// Instructions are an agent's standing instructions.
type Instructions struct {
	// Persona is who the agent is and how it works: appended to the
	// harness's system prompt.
	Persona string `json:"persona,omitempty"`
	// Workspace describes the workspace — which repository is where — in the
	// file the harness reads from its working directory.
	Workspace string `json:"workspace,omitempty"`
}

// Skill is a skill directory: its files, one of which is SKILL.md at the top.
type Skill struct {
	Name  string      `json:"name"`
	Files []SkillFile `json:"files"`
}

// SkillFile is one file of a skill, at a relative slash path.
type SkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Connector is one tool server. It has exactly one of Stdio or HTTP.
type Connector struct {
	Name  string          `json:"name"`
	Stdio *StdioConnector `json:"stdio,omitempty"`
	HTTP  *HTTPConnector  `json:"http,omitempty"`
}

// StdioConnector is a server the harness starts and speaks to on its pipes.
type StdioConnector struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// HTTPConnector is a server at a URL.
type HTTPConnector struct {
	URL string `json:"url"`
	// Headers are sent as they are. A secret belongs in HeadersFile.
	Headers map[string]string `json:"headers,omitempty"`
	// HeadersFile maps a header to the absolute path of a file holding its
	// value (1.3). The harness reads the file when it connects, where it can,
	// so the value stays out of the rendered configuration and of every
	// process's environment; where it cannot, its adapter reads the file
	// when it starts the harness and gives the value to that process alone.
	// The host may rewrite a file between launches.
	HeadersFile map[string]string `json:"headers_file,omitempty"`
	// HeadersEnv maps a header to the environment variable its value is read
	// from, so the value stays out of the rendered configuration, though in
	// the harness's environment and its children's. HeadersFile supersedes
	// it.
	HeadersEnv map[string]string `json:"headers_env,omitempty"`
}

// Memory is an agent's memory directory, seeded with its files.
type Memory struct {
	Files []MemoryFile `json:"files,omitempty"`
}

// MemoryFile is one seeded memory file, at a relative slash path.
type MemoryFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// CredentialRef names a credential's kind.
type CredentialRef struct {
	Kind string `json:"kind"`
}

// Permission postures.
const (
	PostureBypass = "bypass"
	PostureGated  = "gated"
)

// Connector transports.
const (
	ConnectorStdio = "stdio"
	ConnectorHTTP  = "http"
)

// Instruction kinds.
const (
	InstructionPersona   = "persona"
	InstructionWorkspace = "workspace"
)

// CheckSpec refuses an Agent Spec that uses a field or value d does not
// support, with CodeUnsupported naming the field, or that is malformed, with
// CodeInvalidSpec. An adapter's Provision calls it first; it checks only what
// the Descriptor can say.
func CheckSpec(d Descriptor, s AgentSpec) error {
	unsupported := func(field, format string, args ...any) error {
		return &Error{Code: CodeUnsupported, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	invalid := func(field, format string, args ...any) error {
		return &Error{Code: CodeInvalidSpec, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	if !d.Spec.Models.Allows(s.Model) {
		return unsupported("model", "model %q is not one %s takes", s.Model, d.Harness.Name)
	}
	if s.Effort != "" && !supports(d.Spec.Efforts, s.Effort) {
		return unsupported("effort", "effort %q", s.Effort)
	}
	if s.Instructions.Persona != "" && !supports(d.Spec.Instructions, InstructionPersona) {
		return unsupported("instructions.persona", "%s takes no persona", d.Harness.Name)
	}
	if s.Instructions.Workspace != "" && !supports(d.Spec.Instructions, InstructionWorkspace) {
		return unsupported("instructions.workspace", "%s takes no workspace instructions", d.Harness.Name)
	}
	if len(s.Skills) > 0 && !d.Spec.Skills {
		return unsupported("skills", "%s takes no skills", d.Harness.Name)
	}
	if s.Memory != nil && !d.Spec.Memory {
		return unsupported("memory", "%s keeps no memory", d.Harness.Name)
	}
	names := map[string]bool{}
	for i, c := range s.Connectors {
		field := "connectors[" + strconv.Itoa(i) + "]"
		if !validName(c.Name) {
			return invalid(field+".name", "%q", c.Name)
		}
		if names[c.Name] {
			return invalid(field+".name", "%q given twice", c.Name)
		}
		names[c.Name] = true
		switch {
		case (c.Stdio == nil) == (c.HTTP == nil):
			return invalid(field, "want exactly one of stdio and http")
		case c.Stdio != nil && !supports(d.Spec.Connectors, ConnectorStdio):
			return unsupported(field+".stdio", "%s takes no stdio connectors", d.Harness.Name)
		case c.HTTP != nil && !supports(d.Spec.Connectors, ConnectorHTTP):
			return unsupported(field+".http", "%s takes no http connectors", d.Harness.Name)
		case c.Stdio != nil && c.Stdio.Command == "":
			return invalid(field+".stdio.command", "missing")
		case c.HTTP != nil && c.HTTP.URL == "":
			return invalid(field+".http.url", "missing")
		}
		if c.HTTP != nil {
			if err := validHeaders(field+".http", *c.HTTP); err != nil {
				return err
			}
		}
	}
	switch s.PermissionPosture {
	case "":
	case PostureBypass, PostureGated:
		if !supports(d.Spec.PermissionPostures, s.PermissionPosture) {
			return unsupported("permission_posture", "%q", s.PermissionPosture)
		}
	default:
		return invalid("permission_posture", "%q, want bypass or gated", s.PermissionPosture)
	}
	if s.Credential != nil && !supports(d.CredentialKinds, s.Credential.Kind) {
		return unsupported("credential.kind", "%q", s.Credential.Kind)
	}
	if !s.VersionPolicy.Valid() {
		return invalid("version_policy", "%q, want strict or flexible", s.VersionPolicy)
	}
	if minor, err := MinorOf(d.Contract); s.VersionPolicy != "" && (err != nil || minor < VersionPolicySince) {
		return unsupported("version_policy", "%s declares %s, before version policies", d.Harness.Name, d.Contract)
	}
	skills := map[string]bool{}
	for i, sk := range s.Skills {
		field := "skills[" + strconv.Itoa(i) + "]"
		if !validName(sk.Name) || sk.Name == "." || sk.Name == ".." {
			return invalid(field+".name", "%q", sk.Name)
		}
		if skills[sk.Name] {
			return invalid(field+".name", "%q given twice", sk.Name)
		}
		skills[sk.Name] = true
		top, paths := false, map[string]bool{}
		for j, f := range sk.Files {
			if err := CheckRelPath(f.Path); err != nil {
				return invalid(field+".files["+strconv.Itoa(j)+"].path", "%v", err)
			}
			if paths[f.Path] {
				return invalid(field+".files["+strconv.Itoa(j)+"].path", "%q given twice", f.Path)
			}
			paths[f.Path] = true
			top = top || f.Path == "SKILL.md"
		}
		if !top {
			return invalid(field, "no top-level SKILL.md")
		}
	}
	if s.Memory != nil {
		paths := map[string]bool{}
		for j, f := range s.Memory.Files {
			if err := CheckRelPath(f.Path); err != nil {
				return invalid("memory.files["+strconv.Itoa(j)+"].path", "%v", err)
			}
			if paths[f.Path] {
				return invalid("memory.files["+strconv.Itoa(j)+"].path", "%q given twice", f.Path)
			}
			paths[f.Path] = true
		}
	}
	return nil
}

// validHeaders: each header a token, named once across headers,
// headers_env and headers_file in any case, and each file an absolute path.
func validHeaders(field string, h HTTPConnector) error {
	seen := map[string]bool{}
	for _, part := range []struct {
		name string
		m    map[string]string
	}{{"headers", h.Headers}, {"headers_env", h.HeadersEnv}, {"headers_file", h.HeadersFile}} {
		names := make([]string, 0, len(part.m))
		for n := range part.m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if !validToken(n) {
				return &Error{Code: CodeInvalidSpec, Field: field + "." + part.name, Message: fmt.Sprintf("header %q", n)}
			}
			if seen[strings.ToLower(n)] {
				return &Error{Code: CodeInvalidSpec, Field: field + "." + part.name, Message: fmt.Sprintf("header %q given twice", n)}
			}
			seen[strings.ToLower(n)] = true
			if p := part.m[n]; part.name == "headers_file" && (!filepath.IsAbs(p) || filepath.Clean(p) != p) {
				return &Error{Code: CodeInvalidSpec, Field: field + ".headers_file", Message: fmt.Sprintf("header %q: %q is not an absolute path", n, p)}
			}
		}
	}
	return nil
}

// validToken is an HTTP header's name: one or more token characters
// (RFC 9110).
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r > '~' || r <= ' ' || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r) {
			return false
		}
	}
	return true
}

// validName is a connector's or skill's name: 1–64 of [A-Za-z0-9._-].
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// CheckRelPath refuses a path that is not relative, clean and slash-separated,
// with no empty, "." or ".." component: what a file beneath a root may be.
func CheckRelPath(p string) error {
	if p == "" || len(p) > 1024 {
		return fmt.Errorf("path %q is empty or longer than 1024", p)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || !fs.ValidPath(p) || path.Clean(p) != p {
		return fmt.Errorf("path %q is not a clean relative slash path", p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == "." || c == ".." || c == "" {
			return fmt.Errorf("path %q has component %q", p, c)
		}
	}
	return nil
}

// ProvisionResult is what Provision rendered, for the Supervisor to apply.
//
// The Supervisor writes each file beneath its root without following a
// symlink at any component, owned by the agent's workload identity, with the
// file's mode, atomically: a temporary file in the same directory, fsync,
// rename, then fsync of the directory. It journals the result and keeps a
// manifest of the files it wrote; applying a later result replaces the files
// it names and removes those the previous manifest named that it does not.
// Files it did not write — the harness's own record — are never touched. A
// result is applied only while no Host runs for the agent, and completely
// before the next Open; after a crash it is applied again.
type ProvisionResult struct {
	// Files are the files to write.
	Files []File `json:"files"`
	// OpenConfig holds whatever the harness needs to start — argv, env,
	// transport — and is opaque to the Supervisor and the Host, which pass
	// it back to NewSession and OpenRecord unchanged. At most
	// MaxOpenConfigBytes.
	OpenConfig []byte `json:"open_config"`
	// HistoryRoots are what export and archive include.
	HistoryRoots []RootPath `json:"history_roots,omitempty"`
	// SecretPaths are what export and archive must skip.
	SecretPaths []RootPath `json:"secret_paths,omitempty"`
	// HistoryRelocations are, for a request that loads, where the saved
	// history goes in the new environment when that is not where it was:
	// prefix rules, each moving the path From, and everything beneath it, to
	// To. A saved path no rule names keeps its root and path. Both ends of a
	// rule lie at or beneath a history root and outside every secret path; no
	// two rules' sources, and no two destinations, are one another or nested.
	// The Supervisor restores each saved file at Relocate's answer, and
	// rewrites nothing inside it.
	HistoryRelocations []Relocation `json:"history_relocations,omitempty"`
	// HistoryRewrites are, for a request that loads, the one change a
	// restore makes inside a saved file (since 1.8): in each file at or
	// beneath a rule's Path, where the relocations put it, whose first line
	// (at most MaxRewriteLine bytes) is a JSON object with a top-level string
	// Field equal to From, that value becomes To; every other byte of the
	// file stays as it was. A harness that finds a Session by the working
	// directory its record names needs it: pi's session header. Path is
	// history and no secret path, and no two rules' paths are one another or
	// nested. RewriteFirstLine applies them.
	HistoryRewrites []Rewrite `json:"history_rewrites,omitempty"`
}

// Rewrite replaces, in the first line of each saved file at or beneath Path,
// the value From of the JSON object's top-level field Field with To.
type Rewrite struct {
	Path  RootPath `json:"path"`
	Field string   `json:"field"`
	From  string   `json:"from"`
	To    string   `json:"to"`
}

// MaxRewriteLine bounds the first line a rewrite reads: a longer one is left
// as it is.
const MaxRewriteLine = 64 << 10

// RewriteFirstLine is line — a restored file's first line, at at, without
// its newline — as rs rewrite it, and whether one did. Only the value is
// replaced, encoded as JSON with no HTML escaping; the bytes around it stay.
func RewriteFirstLine(rs []Rewrite, at RootPath, line []byte) ([]byte, bool) {
	if len(line) > MaxRewriteLine {
		return line, false
	}
	for _, r := range rs {
		if !at.Within(r.Path) {
			continue
		}
		if out, ok := rewriteField(line, r.Field, r.From, r.To); ok {
			return out, true
		}
	}
	return line, false
}

// rewriteField replaces the top-level string field's value from with to in
// the JSON object line, or reports false.
func rewriteField(line []byte, field, from, to string) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		if key, _ := k.(string); key != field {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil || v != from {
			return nil, false
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if enc.Encode(to) != nil {
			return nil, false
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		out := append(append(append([]byte(nil), line[:start]...), bytes.TrimSuffix(b.Bytes(), []byte("\n"))...), line[end:]...)
		return out, true
	}
	return nil, false
}

// Relocation moves a saved path, and everything beneath it, to another.
type Relocation struct {
	From RootPath `json:"from"`
	To   RootPath `json:"to"`
}

// File is one rendered file. It has exactly one of Text and Bytes.
type File struct {
	// Root is config, workspace or home.
	Root Root `json:"root"`
	// Path is relative to Root, clean, with no ".." or symlink component.
	Path string `json:"path"`
	// Mode is the octal permission, like "0600"; no wider than 0644.
	Mode  string  `json:"mode"`
	Text  *string `json:"text,omitempty"`
	Bytes []byte  `json:"bytes,omitempty"`
}

// Content is the file's content.
func (f File) Content() []byte {
	if f.Text != nil {
		return []byte(*f.Text)
	}
	return f.Bytes
}

// FileMode is the file's mode, parsed.
func (f File) FileMode() (fs.FileMode, error) {
	m, err := strconv.ParseUint(f.Mode, 8, 32)
	if err != nil || len(f.Mode) < 3 {
		return 0, fmt.Errorf("mode %q is not octal", f.Mode)
	}
	return fs.FileMode(m), nil
}

// TextFile is a text file.
func TextFile(root Root, path, mode, text string) File {
	return File{Root: root, Path: path, Mode: mode, Text: &text}
}

// RootPath names a path beneath a root.
type RootPath struct {
	Root Root   `json:"root"`
	Path string `json:"path"`
}

// Within reports whether p is q, or beneath it.
func (p RootPath) Within(q RootPath) bool {
	return p.Root == q.Root && (p.Path == q.Path || strings.HasPrefix(p.Path, q.Path+"/"))
}

// String is the path as an archive names it: its root, then its path.
func (p RootPath) String() string { return string(p.Root) + "/" + p.Path }

// Relocate is where the saved path p goes in a new environment under rules:
// moved by the rule whose source it is, or lies beneath, and itself when no
// rule names it.
func Relocate(rules []Relocation, p RootPath) RootPath {
	for _, r := range rules {
		if p.Within(r.From) {
			return RootPath{Root: r.To.Root, Path: r.To.Path + strings.TrimPrefix(p.Path, r.From.Path)}
		}
	}
	return p
}

// Archived reports whether p is part of the history r names: at or beneath a
// history root, and outside every secret path.
func (r ProvisionResult) Archived(p RootPath) bool {
	in := false
	for _, h := range r.HistoryRoots {
		in = in || p.Within(h)
	}
	for _, s := range r.SecretPaths {
		if p.Within(s) {
			return false
		}
	}
	return in
}

// Validate checks r against the Supervisor's rules: files beneath config,
// workspace or home, clean relative paths each named once, exactly one of text
// and bytes, modes no wider than 0644, and a bounded open_config.
func (r ProvisionResult) Validate() error {
	invalid := func(field, format string, args ...any) error {
		return &Error{Code: CodeProtocol, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	seen := map[RootPath]bool{}
	for i, f := range r.Files {
		field := "files[" + strconv.Itoa(i) + "]"
		switch f.Root {
		case RootConfig, RootWorkspace, RootHome:
		default:
			return invalid(field+".root", "%q, want config, workspace or home", f.Root)
		}
		if err := CheckRelPath(f.Path); err != nil {
			return invalid(field+".path", "%v", err)
		}
		key := RootPath{Root: f.Root, Path: f.Path}
		if seen[key] {
			return invalid(field+".path", "%s/%s given twice", f.Root, f.Path)
		}
		seen[key] = true
		if (f.Text == nil) == (f.Bytes == nil) {
			return invalid(field, "want exactly one of text and bytes")
		}
		mode, err := f.FileMode()
		if err != nil {
			return invalid(field+".mode", "%v", err)
		}
		if mode&^0o644 != 0 {
			return invalid(field+".mode", "%s is wider than 0644", f.Mode)
		}
	}
	if len(r.OpenConfig) > MaxOpenConfigBytes {
		return invalid("open_config", "%d bytes, more than %d", len(r.OpenConfig), MaxOpenConfigBytes)
	}
	checkPath := func(field string, p RootPath) error {
		if p.Root != RootHome && p.Root != RootConfig && p.Root != RootWorkspace && p.Root != RootScratch {
			return invalid(field+".root", "%q", p.Root)
		}
		if err := CheckRelPath(p.Path); err != nil {
			return invalid(field+".path", "%v", err)
		}
		return nil
	}
	for i, p := range append(append([]RootPath(nil), r.HistoryRoots...), r.SecretPaths...) {
		if err := checkPath("paths["+strconv.Itoa(i)+"]", p); err != nil {
			return err
		}
	}
	for i, m := range r.HistoryRelocations {
		field := "history_relocations[" + strconv.Itoa(i) + "]"
		if err := checkPath(field+".from", m.From); err != nil {
			return err
		}
		if err := checkPath(field+".to", m.To); err != nil {
			return err
		}
		if !r.Archived(m.From) || !r.Archived(m.To) {
			return invalid(field, "%s to %s: both must be history, and no secret path", m.From, m.To)
		}
		for j, other := range r.HistoryRelocations[:i] {
			if m.From.Within(other.From) || other.From.Within(m.From) {
				return invalid(field+".from", "%s overlaps history_relocations[%d]'s %s", m.From, j, other.From)
			}
			if m.To.Within(other.To) || other.To.Within(m.To) {
				return invalid(field+".to", "%s collides with history_relocations[%d]'s %s", m.To, j, other.To)
			}
		}
	}
	for i, w := range r.HistoryRewrites {
		field := "history_rewrites[" + strconv.Itoa(i) + "]"
		if err := checkPath(field+".path", w.Path); err != nil {
			return err
		}
		if !r.Archived(w.Path) {
			return invalid(field+".path", "%s: must be history, and no secret path", w.Path)
		}
		if w.Field == "" || w.From == "" || w.From == w.To {
			return invalid(field, "a field, and a value from that the rewrite changes")
		}
		for j, other := range r.HistoryRewrites[:i] {
			if w.Path.Within(other.Path) || other.Path.Within(w.Path) {
				return invalid(field+".path", "%s overlaps history_rewrites[%d]'s %s", w.Path, j, other.Path)
			}
		}
	}
	return nil
}

// CheckLoad refuses a load d's adapter does not take, with CodeUnsupported
// naming what, or whose source is malformed, with CodeInvalidSpec. An
// adapter's Provision calls it for a request that loads.
func CheckLoad(d Descriptor, l LoadSource) error {
	unsupported := func(field, format string, args ...any) error {
		return &Error{Code: CodeUnsupported, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	switch {
	case !d.Has(CapSessionLoad) || d.Load == nil:
		return unsupported("load", "%s loads no saved Session", d.Harness.Name)
	case l.Harness.Name != d.Harness.Name:
		return unsupported("load.harness.name", "a Session of %q, not of %s", l.Harness.Name, d.Harness.Name)
	}
	format := false
	for _, f := range d.Load.Formats {
		format = format || f == l.Format
	}
	if !format {
		return unsupported("load.format", "archive format %d; %s loads %v", l.Format, d.Harness.Adapter, d.Load.Formats)
	}
	if !supports(d.Load.Sources, l.Harness.Version) {
		return unsupported("load.harness.version", "a Session %s %s saved; %s %s loads those of %v",
			l.Harness.Name, l.Harness.Version, d.Harness.Name, d.Harness.Version, d.Load.Sources)
	}
	if err := l.Layout.Validate(); err != nil {
		var e *Error
		if errors.As(err, &e) {
			return &Error{Code: CodeInvalidSpec, Field: "load." + e.Field, Message: e.Message}
		}
		return err
	}
	if !filepath.IsAbs(l.Workspace) || filepath.Clean(l.Workspace) != l.Workspace {
		return &Error{Code: CodeInvalidSpec, Field: "load.workspace", Message: fmt.Sprintf("%q is not a clean absolute path", l.Workspace)}
	}
	return nil
}
