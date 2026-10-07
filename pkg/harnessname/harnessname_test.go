package harnessname

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"claude":        ClaudeCode,
		" Claude ":      ClaudeCode,
		"claude-code":   ClaudeCode,
		"CLAUDE-CODE":   ClaudeCode,
		"codex":         Codex,
		" Codex":        Codex,
		"opencode":      OpenCode,
		"pi":            Pi,
		"generic":       "generic",
		" Some-Thing  ": "some-thing",
		"":              "",
	} {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnaliasIsExact(t *testing.T) {
	for in, want := range map[string]string{
		"claude":      ClaudeCode,
		"claude-code": ClaudeCode,
		"Claude":      "Claude",
		" claude":     " claude",
		"codex":       Codex,
		"Codex":       "Codex",
	} {
		if got := Unalias(in); got != want {
			t.Errorf("Unalias(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShort(t *testing.T) {
	for in, want := range map[string]string{
		"claude":      Claude,
		"claude-code": Claude,
		" Claude ":    Claude,
		"codex":       Codex,
		"opencode":    OpenCode,
		"other":       "other",
	} {
		if got := Short(in); got != want {
			t.Errorf("Short(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsClaude(t *testing.T) {
	for _, yes := range []string{"claude", "claude-code", " Claude-Code "} {
		if !IsClaude(yes) {
			t.Errorf("IsClaude(%q) = false", yes)
		}
	}
	for _, no := range []string{"", "codex", "claudecode", "opencode"} {
		if IsClaude(no) {
			t.Errorf("IsClaude(%q) = true", no)
		}
	}
}

func TestSpellings(t *testing.T) {
	if got, want := Spellings(ClaudeCode), []string{"claude-code", "claude"}; !slices.Equal(got, want) {
		t.Errorf("Spellings(ClaudeCode) = %v, want %v", got, want)
	}
	if got, want := Spellings(Codex), []string{"codex"}; !slices.Equal(got, want) {
		t.Errorf("Spellings(Codex) = %v, want %v", got, want)
	}
}

// Every alias resolves to a canonical id, and every short key round-trips.
func TestTablesConsistent(t *testing.T) {
	for alias, id := range aliases {
		if Normalize(alias) != alias || Normalize(id) != id {
			t.Errorf("alias table entry %q→%q is not normalized", alias, id)
		}
		if _, isAlias := aliases[id]; isAlias {
			t.Errorf("alias %q resolves to another alias %q", alias, id)
		}
	}
	for id, short := range shorts {
		if Canonical(short) != id {
			t.Errorf("short %q of %q does not canonicalize back", short, id)
		}
	}
}

// The package is a dependency-free leaf: any layer may import it.
func TestStandardLibraryOnly(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	out, err := exec.Command(goTool, "list", "-deps", "github.com/olesho/harness-wrapper/pkg/harnessname").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, ".") && dep != "github.com/olesho/harness-wrapper/pkg/harnessname" {
			t.Errorf("imports non-standard package %s", dep)
		}
	}
}
