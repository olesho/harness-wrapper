package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	cc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// TestRelativeConfigRootUsesChildWorkingDir: claude takes CLAUDE_CONFIG_DIR
// verbatim, so a relative root is relative to the harness child's cwd — the
// workingDir ReadTranscript is handed — not to the wrapper's own cwd.
func TestRelativeConfigRootUsesChildWorkingDir(t *testing.T) {
	wd := t.TempDir()
	dir := filepath.Join(wd, ".agent-config", "projects", cc.EncodedCWD(wd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"child reply\"}]}}\n")
	if err := os.WriteFile(filepath.Join(dir, "review-session.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.ConfigureFromEnv([]string{"CLAUDE_CONFIG_DIR=.agent-config"})
	if _, err := a.ReadTranscript("review-session", wd); err != nil {
		t.Fatalf("transcript exists relative to child's cwd but reader failed: %v", err)
	}
}
