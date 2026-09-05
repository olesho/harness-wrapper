package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// stageClaudeTranscript writes a two-line Claude Code session log under
// <configRoot>/projects/<encoded cwd>/<sessionID>.jsonl, exactly where claude
// writes it when it is launched with CLAUDE_CONFIG_DIR=<configRoot>. The slug
// is taken from the SYMLINK-RESOLVED working dir because that is what claude
// itself encodes (macOS temp dirs live under /var → /private/var).
func stageClaudeTranscript(t *testing.T, configRoot, workingDir, sessionID, reply string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		resolved = workingDir
	}
	projDir := filepath.Join(configRoot, "projects", transcriptcc.EncodedCWD(resolved))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-05-14T12:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + reply + `"}]},"timestamp":"2026-05-14T12:00:05Z"}
`
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeConfigDir_TranscriptFoundUnderPerAgentRoot is the regression test
// for PUPPET-494: a profiled agent is launched with CLAUDE_CONFIG_DIR pointing
// at its OWN config root, so its transcript lives under
// <CLAUDE_CONFIG_DIR>/projects/... — not under the operator's ~/.claude. Before
// the fix the chat layer's claude-code adapter always read the operator's home,
// so HistoryWithSource failed with ENOENT and every caller silently degraded to
// the screen-scraped store fallback.
func TestClaudeConfigDir_TranscriptFoundUnderPerAgentRoot(t *testing.T) {
	const sessID = "11111111-2222-3333-4444-555555555555"
	const reply = "the model's own words"

	cfgDir := filepath.Join(t.TempDir(), "claude") // the per-agent CLAUDE_CONFIG_DIR
	wd := t.TempDir()                              // the agent worktree
	stageClaudeTranscript(t, cfgDir, wd, sessID, reply)

	// A real Open over a real PTY: this is the wiring under test, not a
	// hand-built Conversation.
	script := fakeharness.New("claude-code").Session(sessID).Idle().StayAliveUntilStopped().Build()
	conv := openFake(t, script, func(o *Options) {
		o.WorkingDir = wd
		o.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+cfgDir)
	})
	conv.captureRawSessionID("claude --resume " + sessID)

	turns, src, err := conv.HistoryWithSource(context.Background())
	if err != nil {
		t.Fatalf("HistoryWithSource: %v", err)
	}
	if src != HistorySourceTranscript {
		t.Fatalf("source = %q, want %q", src, HistorySourceTranscript)
	}
	if len(turns) != 2 || turns[1].Text != reply {
		t.Fatalf("turns = %+v, want the staged transcript ending in %q", turns, reply)
	}
}

// TestClaudeConfigDir_UnsetKeepsHomeDefault guards the overwhelmingly common
// operator case: no CLAUDE_CONFIG_DIR in the launch env, so the reader must keep
// its <home>/.claude/projects default. A careless fix breaks exactly this.
func TestClaudeConfigDir_UnsetKeepsHomeDefault(t *testing.T) {
	const sessID = "22222222-3333-4444-5555-666666666666"
	const reply = "home-rooted transcript"

	home := t.TempDir()
	t.Setenv("HOME", home)
	// An EMPTY value must read as unset too (edge case 4) — and this test box
	// really does export CLAUDE_CONFIG_DIR, so without it os.Environ() below
	// would smuggle the operator's profile in.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	wd := t.TempDir()
	stageClaudeTranscript(t, filepath.Join(home, ".claude"), wd, sessID, reply)

	script := fakeharness.New("claude-code").Session(sessID).Idle().StayAliveUntilStopped().Build()
	conv := openFake(t, script, func(o *Options) {
		o.WorkingDir = wd
		// os.Environ() carries the t.Setenv'd HOME and a blank CLAUDE_CONFIG_DIR.
		o.Env = os.Environ()
	})
	conv.captureRawSessionID("claude --resume " + sessID)

	turns, src, err := conv.HistoryWithSource(context.Background())
	if err != nil {
		t.Fatalf("HistoryWithSource: %v", err)
	}
	if src != HistorySourceTranscript {
		t.Fatalf("source = %q, want %q", src, HistorySourceTranscript)
	}
	if len(turns) != 2 || turns[1].Text != reply {
		t.Fatalf("turns = %+v, want the home-rooted transcript ending in %q", turns, reply)
	}
}

// TestConfigureAdapterEnv_NilEnvFallsBackToProcess pins the documented
// Options.Env default: nil means "the current process's environment", so a
// caller that never sets Env still gets its own CLAUDE_CONFIG_DIR honoured.
func TestConfigureAdapterEnv_NilEnvFallsBackToProcess(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)

	a := claudecode.New()
	configureAdapterEnv(a, nil)

	if want := filepath.Join(cfgDir, "projects"); a.ProjectsRoot != want {
		t.Fatalf("ProjectsRoot = %q, want %q", a.ProjectsRoot, want)
	}
}

// TestConfigureAdapterEnv_NonConfigurableAdapterIsNoOp keeps the capability
// OPTIONAL: an adapter that does not implement turns.EnvConfigurable (generic,
// opencode, pi) must pass through untouched rather than panic.
func TestConfigureAdapterEnv_NonConfigurableAdapterIsNoOp(t *testing.T) {
	a, err := resolveAdapter("generic")
	if err != nil {
		t.Fatal(err)
	}
	configureAdapterEnv(a, []string{"CLAUDE_CONFIG_DIR=/nowhere"})
}
