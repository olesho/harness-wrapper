package harness_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// runHooksDumpingChildEnv drives harness.Run in hooks mode with a harness child
// that writes its own environment to a file and exits. Every hook subprocess the
// real harness fires inherits exactly this environment, so it is the contract
// under test: what HW_HARNESS_CONFIG_DIR the hooks are handed.
func runHooksDumpingChildEnv(t *testing.T, wc wrapper.Config) []string {
	t.Helper()
	envFile := filepath.Join(t.TempDir(), "child.env")
	wc.BinaryPath = "/bin/sh"
	wc.Args = []string{"-c", `env > "$0"`, envFile}
	wc.Harness = "claude"
	wc.Stdout = io.Discard
	cfg := harness.Config{
		Wrapper:        wc,
		RunID:          "config-root",
		TranscriptMode: harness.TranscriptHooks,
		HookCommand:    []string{"/abs/loom", "hooks"},
		OnEvent:        func(transcript.EventEnvelope) error { return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := harness.Run(ctx, cfg); err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("child env dump: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// envWithout returns the process environment minus every assignment of key, so
// a case controls CLAUDE_CONFIG_DIR itself rather than inheriting the host's.
func envWithout(key string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// stageTranscript writes a one-turn claude session log at path.
func stageTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","uuid":"a1","timestamp":"2026-05-14T12:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"agent reply"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fireStopHook runs the hook subprocess logic in-process with the child's
// environment (and a live spool, since Run removed its own) for a Stop hook
// whose payload hands over transcriptPath exactly as claude would.
func fireStopHook(t *testing.T, childEnv []string, sessionID, transcriptPath string) error {
	t.Helper()
	spool := t.TempDir()
	env := append(append([]string(nil), childEnv...), harness.EnvSpool+"="+spool)
	payload, err := json.Marshal(map[string]string{"session_id": sessionID, "transcript_path": transcriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = harness.HandleHookEvent("claude", "stop", env, payload)
	return err
}

// TestRunHookEnvCarriesEffectiveConfigRoot pins what the hooks are told the
// harness config root is, for each way the harness can be launched with one.
// claude takes CLAUDE_CONFIG_DIR verbatim, so a RELATIVE root is relative to the
// harness child's working directory — and so are the transcript paths it hands
// its hooks — never to the wrapper's.
func TestRunHookEnvCarriesEffectiveConfigRoot(t *testing.T) {
	const sid = "hook-root-sess"

	t.Run("inherited env", func(t *testing.T) {
		wt := t.TempDir()
		root := filepath.Join(t.TempDir(), "agent-claude")
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		childEnv := runHooksDumpingChildEnv(t, wrapper.Config{WorkingDir: wt}) // Env nil: the child inherits
		if got := harness.EnvLookup(childEnv, "CLAUDE_CONFIG_DIR"); got != root {
			t.Fatalf("precondition: child CLAUDE_CONFIG_DIR = %q, want %q", got, root)
		}
		if got := harness.EnvLookup(childEnv, harness.EnvConfigDir); got != root {
			t.Fatalf("%s = %q, want the inherited root %q", harness.EnvConfigDir, got, root)
		}
		tpath := filepath.Join(root, "projects", transcriptcc.EncodedCWD(wt), sid+".jsonl")
		stageTranscript(t, tpath)
		if err := fireStopHook(t, childEnv, sid, tpath); err != nil {
			t.Fatalf("stop hook rejected the agent's own transcript: %v", err)
		}
	})

	t.Run("relative root", func(t *testing.T) {
		wt := t.TempDir()
		env := append(envWithout("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR=.agent-claude")
		childEnv := runHooksDumpingChildEnv(t, wrapper.Config{WorkingDir: wt, Env: env})
		want := filepath.Join(wt, ".agent-claude")
		if got := harness.EnvLookup(childEnv, harness.EnvConfigDir); got != want {
			t.Fatalf("%s = %q, want %q (relative to the child's cwd, not the wrapper's)", harness.EnvConfigDir, got, want)
		}
		// claude hands over the path built from its verbatim relative root.
		rel := filepath.Join(".agent-claude", "projects", transcriptcc.EncodedCWD(wt), sid+".jsonl")
		stageTranscript(t, filepath.Join(wt, rel))
		if err := fireStopHook(t, childEnv, sid, rel); err != nil {
			t.Fatalf("stop hook rejected the agent's own relative transcript path: %v", err)
		}
	})

	t.Run("explicit absolute root", func(t *testing.T) {
		wt := t.TempDir()
		root := filepath.Join(t.TempDir(), "agent-claude")
		env := append(envWithout("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR="+root)
		childEnv := runHooksDumpingChildEnv(t, wrapper.Config{WorkingDir: wt, Env: env})
		if got := harness.EnvLookup(childEnv, harness.EnvConfigDir); got != root {
			t.Fatalf("%s = %q, want %q", harness.EnvConfigDir, got, root)
		}
	})

	t.Run("no root keeps the home default", func(t *testing.T) {
		wt := t.TempDir()
		childEnv := runHooksDumpingChildEnv(t, wrapper.Config{WorkingDir: wt, Env: envWithout("CLAUDE_CONFIG_DIR")})
		for _, kv := range childEnv {
			if strings.HasPrefix(kv, harness.EnvConfigDir+"=") {
				t.Fatalf("unprofiled run assigns %q; the hook must keep its HOME-derived default", kv)
			}
		}
	})
}
