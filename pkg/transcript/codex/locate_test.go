package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRollout writes a minimal rollout JSONL whose first line is a
// session_meta envelope carrying the given session id + cwd, then stamps
// the file's mtime so locator ordering is deterministic.
func writeRollout(t *testing.T, root, sessionID, cwd string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "06", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-06-26T05:25:23.303Z","type":"session_meta","payload":{"session_id":"` + sessionID + `","cwd":"` + cwd + `","cli_version":"0.142.0"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}
`
	path := filepath.Join(dir, "rollout-2026-06-26T07-25-23-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLocateLatestSession_PicksNewestMatchingCwd is the core regression for the
// Codex 0.142 session-id gap: the screen no longer prints "codex resume <uuid>",
// so the on-disk session_meta is the only anchor. The locator must select the
// newest rollout whose cwd matches the working directory, version-independently.
func TestLocateLatestSession_PicksNewestMatchingCwd(t *testing.T) {
	root := t.TempDir()
	cwd := "/work/project"
	base := time.Date(2026, 6, 26, 7, 0, 0, 0, time.UTC)

	// Older session in the target cwd.
	writeRollout(t, root, "00000000-0000-0000-0000-000000000001", cwd, base)
	// Newer session in the SAME cwd — this is the one we want.
	want := "00000000-0000-0000-0000-000000000002"
	writeRollout(t, root, want, cwd, base.Add(2*time.Minute))
	// Even-newer session in a DIFFERENT cwd — must be ignored.
	writeRollout(t, root, "00000000-0000-0000-0000-000000000003", "/other/dir", base.Add(5*time.Minute))

	r := &Reader{SessionsRoot: root}
	got, ok := r.LocateLatestSession(cwd)
	if !ok {
		t.Fatal("LocateLatestSession returned ok=false, want a match")
	}
	if got != want {
		t.Fatalf("LocateLatestSession = %q, want %q (newest rollout in matching cwd)", got, want)
	}
}

func TestLocateLatestSession_NoMatchReturnsFalse(t *testing.T) {
	root := t.TempDir()
	writeRollout(t, root, "00000000-0000-0000-0000-000000000001", "/some/where", time.Now())

	r := &Reader{SessionsRoot: root}
	if got, ok := r.LocateLatestSession("/different/cwd"); ok {
		t.Fatalf("LocateLatestSession = %q, ok=true; want no match for unrelated cwd", got)
	}
}

func TestLocateLatestSession_EmptyWorkingDirReturnsFalse(t *testing.T) {
	root := t.TempDir()
	writeRollout(t, root, "00000000-0000-0000-0000-000000000001", "/some/where", time.Now())

	r := &Reader{SessionsRoot: root}
	if _, ok := r.LocateLatestSession(""); ok {
		t.Fatal("LocateLatestSession with empty workingDir returned ok=true, want false")
	}
}

// TestLocateLatestSession_ToleratesJunkFiles ensures non-session_meta, empty,
// and malformed-JSON rollouts don't crash the walk or get mis-selected.
func TestLocateLatestSession_ToleratesJunkFiles(t *testing.T) {
	root := t.TempDir()
	cwd := "/work/project"
	dir := filepath.Join(root, "2026", "06", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Empty file.
	if err := os.WriteFile(filepath.Join(dir, "empty.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Malformed first line.
	if err := os.WriteFile(filepath.Join(dir, "garbage.jsonl"), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// First line is not a session_meta.
	if err := os.WriteFile(filepath.Join(dir, "nometa.jsonl"), []byte(`{"type":"response_item","payload":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A valid one we should still find.
	want := "00000000-0000-0000-0000-0000000000aa"
	writeRollout(t, root, want, cwd, time.Now())

	r := &Reader{SessionsRoot: root}
	got, ok := r.LocateLatestSession(cwd)
	if !ok || got != want {
		t.Fatalf("LocateLatestSession = %q ok=%v, want %q ok=true despite junk files", got, ok, want)
	}
}

// TestLocateLatestSession_CleansPaths verifies trailing-slash / non-canonical
// cwds still match the recorded session_meta cwd.
func TestLocateLatestSession_CleansPaths(t *testing.T) {
	root := t.TempDir()
	writeRollout(t, root, "00000000-0000-0000-0000-0000000000bb", "/work/project", time.Now())

	r := &Reader{SessionsRoot: root}
	if got, ok := r.LocateLatestSession("/work/project/"); !ok {
		t.Fatalf("LocateLatestSession(%q) ok=false; want match after path clean (got %q)", "/work/project/", got)
	}
}
