package profile

import "path"

// Harness names a coding-agent CLI whose config root can be provisioned. The
// string values match the directory names used under a profile source tree
// (profiles/<agent>/<harness>/) and the harness names used elsewhere in this
// module.
type Harness string

// The harnesses whose config-root layout this package describes.
const (
	Claude Harness = "claude"
	Codex  Harness = "codex"
)

// Harnesses returns every known harness in a stable order.
func Harnesses() []Harness { return []Harness{Claude, Codex} }

// Validate reports whether h is a harness this package knows about,
// returning ErrUnknownHarness if it is not. Methods that cannot return an
// error (EnvVar, Binary) yield zero values for an unknown harness.
func (h Harness) Validate() error {
	for _, known := range Harnesses() {
		if h == known {
			return nil
		}
	}
	return ErrUnknownHarness
}

// EnvVar is the environment variable that points the harness at a config root.
// It returns "" for an unknown harness.
func (h Harness) EnvVar() string {
	switch h {
	case Claude:
		return "CLAUDE_CONFIG_DIR"
	case Codex:
		return "CODEX_HOME"
	default:
		return ""
	}
}

// Binary is the on-PATH executable name, i.e. the thing whose `--version`
// output becomes Manifest.HarnessVersion. It returns "" for an unknown harness.
func (h Harness) Binary() string {
	switch h {
	case Claude:
		return "claude"
	case Codex:
		return "codex"
	default:
		return ""
	}
}

// seedFiles are copied into a config root only if absent there, and are never
// manifested or hashed: the harness rewrites them at runtime, so hashing them
// would make every profile fail verification within minutes.
var seedFiles = map[Harness][]string{
	// The auth pair plus user-scope state; claude rewrites both continuously.
	Claude: {".claude.json", ".credentials.json"},
	// codex's auth file. Listed for codex only: claude has no auth.json, and
	// declaring it there anyway would misdescribe the contract.
	Codex: {"auth.json"},
}

// excludedDirs are never walked, so nothing beneath them is provisioned,
// seeded, or manifested. Every entry is a runtime artefact directory the
// harness owns.
var excludedDirs = map[Harness][]string{
	Claude: {
		"backups",         // claude's own config backups
		"cache",           // derived data
		"history",         // prompt history
		"paste-cache",     // pasted-content spool
		"plugins",         // installed at runtime
		"projects",        // per-project state and transcripts
		"session-env",     // per-session environment snapshots
		"sessions",        // live and past session state
		"shell-snapshots", // captured shell environments
		"todos",           // per-session todo state
	},
	Codex: {
		"history",  // prompt history
		"log",      // rollout logs
		"sessions", // live and past session state
	},
}

// junkFiles are never provisioned or seeded, from any harness.
var junkFiles = []string{".DS_Store"}

// SeedFiles returns the copy-if-absent files for the harness, or nil for an
// unknown one. The returned slice is a copy; mutating it does not affect the
// package's table.
func (h Harness) SeedFiles() []string { return cloneList(seedFiles[h]) }

// ExcludedDirs returns the directory names never walked for the harness, or nil
// for an unknown one. The returned slice is a copy.
func (h Harness) ExcludedDirs() []string { return cloneList(excludedDirs[h]) }

// IsSeed reports whether the relative path is a seed file. Seeds match at the
// TOP LEVEL ONLY: "auth.json" is a seed for codex, "skills/auth.json" is
// ordinary provisioned content.
func (h Harness) IsSeed(rel string) bool {
	if path.Dir(rel) != "." {
		return false
	}
	for _, s := range seedFiles[h] {
		if rel == s {
			return true
		}
	}
	return false
}

// IsExcludedDir reports whether a directory with this base name is skipped for
// the harness.
func (h Harness) IsExcludedDir(name string) bool {
	for _, d := range excludedDirs[h] {
		if name == d {
			return true
		}
	}
	return false
}

// isJunk reports whether a file's base name is never provisioned or seeded.
func isJunk(name string) bool {
	for _, j := range junkFiles {
		if name == j {
			return true
		}
	}
	return false
}

func cloneList(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
