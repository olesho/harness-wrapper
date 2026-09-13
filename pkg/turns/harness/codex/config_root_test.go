package codex

import (
	"os"
	"path/filepath"
	"testing"
)

// stageRelativeRollout writes a codex rollout for sessionID, recorded as run in
// cwd, under <cwd>/<codexHome>/sessions — where codex writes it when launched in
// cwd with a RELATIVE CODEX_HOME (codex canonicalizes the value against its own
// working directory at startup).
func stageRelativeRollout(t *testing.T, cwd, codexHome, sessionID string) {
	t.Helper()
	dir := filepath.Join(cwd, codexHome, "sessions", "2026", "05", "14")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-05-14T12:00:00Z","type":"session_meta","payload":{"session_id":"` + sessionID + `","cwd":"` + cwd + `","cli_version":"0.154.0"}}
{"timestamp":"2026-05-14T12:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex child reply"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-05-14T12-00-00-"+sessionID+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRelativeCodexHomeUsesChildWorkingDir is the codex twin of the claude-code
// relative-root regression: both on-disk lookups the adapter owns — the
// transcript read and the session-id fallback — must resolve a relative
// CODEX_HOME against the harness child's working directory, not the wrapper's.
func TestRelativeCodexHomeUsesChildWorkingDir(t *testing.T) {
	const sid = "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
	wd := t.TempDir()
	stageRelativeRollout(t, wd, ".codex-agent", sid)

	a := New()
	a.ConfigureFromEnv([]string{"CODEX_HOME=.codex-agent"})

	turns, err := a.ReadTranscript(sid, wd)
	if err != nil {
		t.Fatalf("rollout exists relative to the child's cwd but ReadTranscript failed: %v", err)
	}
	if len(turns) == 0 || turns[len(turns)-1].Text != "codex child reply" {
		t.Fatalf("turns = %+v, want the staged rollout", turns)
	}
	if got, ok := a.LocateSessionID(wd); !ok || got != sid {
		t.Fatalf("LocateSessionID(%q) = (%q, %v), want (%q, true)", wd, got, ok, sid)
	}
}
