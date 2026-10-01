package contract

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
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
	// Headers are sent as they are. A secret belongs in HeadersEnv.
	Headers map[string]string `json:"headers,omitempty"`
	// HeadersEnv maps a header to the environment variable its value is read
	// from, so the value stays out of the rendered configuration.
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
