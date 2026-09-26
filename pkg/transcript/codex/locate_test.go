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

// TestLocateLatestSession_MatchesThroughSymlinkedWorkingDir is the regression
// for the symlink defect: the wrapper passes os.Getwd() (a symlinked path like
// /tmp/x) while Codex records its resolved cwd (/private/tmp/x). A plain
// filepath.Clean leaves those unequal and the lookup silently fails for any run
// under a symlinked working directory — common on macOS, where /tmp→/private/tmp
// and even t.TempDir() lives under /var→/private/var. The locator must compare
// after resolving symlinks so the two spellings of the same directory match.
func TestLocateLatestSession_MatchesThroughSymlinkedWorkingDir(t *testing.T) {
	root := t.TempDir()

	// A real working directory and a symlink that points at it.
	realDir := filepath.Join(t.TempDir(), "real-cwd")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "link-cwd")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unsupported on this platform/filesystem: %v", err)
	}

	// Model Codex recording its *resolved* cwd (what it actually writes), while
	// the caller below queries with the *symlinked* spelling.
	recordedCwd, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	const want = "00000000-0000-0000-0000-0000000000cc"
	writeRollout(t, root, want, recordedCwd, time.Now())

	r := &Reader{SessionsRoot: root}
	got, ok := r.LocateLatestSession(linkDir)
	if !ok {
		t.Fatalf("LocateLatestSession(%q) ok=false; want a match through the symlink to recorded cwd %q", linkDir, recordedCwd)
	}
	if got != want {
		t.Fatalf("LocateLatestSession(%q) = %q, want %q", linkDir, got, want)
	}
}

// writeLaunchRollout writes a rollout for a session started at start, as Codex
// does: in the <YYYY>/<MM>/<DD> directory of the start's date in loc, with the
// start in session_meta's payload and source as the session's source (a
// launcher's name, or a subagent object).
func writeLaunchRollout(t *testing.T, root, sessionID, cwd string, start time.Time, loc *time.Location, source string) {
	t.Helper()
	local := start.In(loc)
	dir := filepath.Join(root, local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := start.UTC().Format("2006-01-02T15:04:05.000Z")
	body := `{"timestamp":"` + ts + `","type":"session_meta","payload":{"session_id":"` + sessionID + `","timestamp":"` + ts +
		`","cwd":"` + cwd + `","source":` + source + `,"cli_version":"0.157.1"}}` + "\n"
	path := filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	sessionA = "019f0001-aaaa-7000-8000-00000000000a"
	sessionB = "019f0001-bbbb-7000-8000-00000000000b"
)

// Two Codex conversations in one directory: A launched first, B while A still
// ran. Nothing on disk says which rollout belongs to which process, and the
// newest-modified rule handed A the id of B's session — whichever wrote last —
// and with it B's transcript. A's lookup now refuses; B's, launched after A's
// session started, has only its own to find.
func TestLocateLaunchSession_TwoConversationsInOneDirectory(t *testing.T) {
	root := t.TempDir()
	const cwd = "/work/shared"
	launchA := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	launchB := launchA.Add(30 * time.Second)
	writeLaunchRollout(t, root, sessionA, cwd, launchA.Add(time.Second), time.UTC, `"cli"`)
	writeLaunchRollout(t, root, sessionB, cwd, launchB.Add(time.Second), time.UTC, `"cli"`)

	r := &Reader{SessionsRoot: root}
	if got, ok := r.LocateLaunchSession(cwd, launchA); ok {
		t.Fatalf("A's lookup = %q, want none: two sessions started in the directory since A launched", got)
	}
	if got, ok := r.LocateLaunchSession(cwd, launchB); !ok || got != sessionB {
		t.Fatalf("B's lookup = (%q, %v), want (%q, true)", got, ok, sessionB)
	}
}

// The directory's earlier sessions are never the launch's, however recently
// their rollouts were written: before the launch's own session writes its
// rollout the answer is none — LocateLatestSession answers with the earlier
// one — and after, it is the launch's.
func TestLocateLaunchSession_IgnoresSessionsFromBeforeTheLaunch(t *testing.T) {
	root := t.TempDir()
	const cwd = "/work/project"
	launch := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	writeLaunchRollout(t, root, sessionA, cwd, launch.Add(-time.Hour), time.UTC, `"cli"`)

	r := &Reader{SessionsRoot: root}
	if got, ok := r.LocateLaunchSession(cwd, launch); ok {
		t.Fatalf("lookup = %q before the launch's session wrote its rollout, want none", got)
	}
	writeLaunchRollout(t, root, sessionB, cwd, launch.Add(2*time.Second), time.UTC, `"cli"`)
	if got, ok := r.LocateLaunchSession(cwd, launch); !ok || got != sessionB {
		t.Fatalf("lookup = (%q, %v), want the launch's session (%q, true)", got, ok, sessionB)
	}
}

// A session Codex starts for itself (a /review, a spawned agent) writes its
// own rollout in the same directory; it is the launch's child, not a second
// launch.
func TestLocateLaunchSession_SkipsSubagentSessions(t *testing.T) {
	root := t.TempDir()
	const cwd = "/work/project"
	launch := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	writeLaunchRollout(t, root, sessionA, cwd, launch.Add(time.Second), time.UTC, `"cli"`)
	writeLaunchRollout(t, root, sessionB, cwd, launch.Add(time.Minute), time.UTC, `{"subagent":"review"}`)

	r := &Reader{SessionsRoot: root}
	if got, ok := r.LocateLaunchSession(cwd, launch); !ok || got != sessionA {
		t.Fatalf("lookup = (%q, %v), want (%q, true)", got, ok, sessionA)
	}
}

// The start bound is whole seconds, and the day directories it prunes are a
// margin away from the launch's date: a Codex whose clock records whole
// seconds, or whose local date is the day before the launch's UTC date, is
// still found.
func TestLocateLaunchSession_Margins(t *testing.T) {
	const cwd = "/work/project"
	t.Run("whole-second start", func(t *testing.T) {
		root := t.TempDir()
		launch := time.Date(2026, 9, 26, 10, 0, 0, 700_000_000, time.UTC)
		writeLaunchRollout(t, root, sessionA, cwd, launch.Truncate(time.Second), time.UTC, `"cli"`)
		if got, ok := (&Reader{SessionsRoot: root}).LocateLaunchSession(cwd, launch); !ok || got != sessionA {
			t.Fatalf("lookup = (%q, %v), want (%q, true)", got, ok, sessionA)
		}
	})
	t.Run("previous local date", func(t *testing.T) {
		root := t.TempDir()
		launch := time.Date(2026, 9, 26, 0, 30, 0, 0, time.UTC)
		// 2026-09-25 19:30 in UTC-5: filed under 2026/09/25.
		writeLaunchRollout(t, root, sessionA, cwd, launch.Add(time.Second), time.FixedZone("UTC-5", -5*3600), `"cli"`)
		if got, ok := (&Reader{SessionsRoot: root}).LocateLaunchSession(cwd, launch); !ok || got != sessionA {
			t.Fatalf("lookup = (%q, %v), want (%q, true)", got, ok, sessionA)
		}
	})
	t.Run("old day directories are not read", func(t *testing.T) {
		root := t.TempDir()
		launch := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
		writeLaunchRollout(t, root, sessionA, cwd, launch.Add(time.Second), time.UTC, `"cli"`)
		// A rollout claiming a start after the launch, filed a week before it:
		// read, it would make the lookup ambiguous.
		dir := filepath.Join(root, "2026", "09", "19")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		ts := launch.Add(time.Minute).Format("2006-01-02T15:04:05.000Z")
		body := `{"timestamp":"` + ts + `","type":"session_meta","payload":{"session_id":"` + sessionB + `","timestamp":"` + ts + `","cwd":"` + cwd + `","source":"cli"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "rollout-x-"+sessionB+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := (&Reader{SessionsRoot: root}).LocateLaunchSession(cwd, launch); !ok || got != sessionA {
			t.Fatalf("lookup = (%q, %v), want (%q, true)", got, ok, sessionA)
		}
	})
}

// With no launch time there is no start bound: the directory's one session is
// found, and a second makes the answer ambiguous rather than the newer.
func TestLocateLaunchSession_ZeroLaunchTime(t *testing.T) {
	root := t.TempDir()
	const cwd = "/work/project"
	start := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	writeLaunchRollout(t, root, sessionA, cwd, start, time.UTC, `"cli"`)

	r := &Reader{SessionsRoot: root}
	if got, ok := r.LocateLaunchSession(cwd, time.Time{}); !ok || got != sessionA {
		t.Fatalf("lookup = (%q, %v), want (%q, true)", got, ok, sessionA)
	}
	writeLaunchRollout(t, root, sessionB, cwd, start.Add(time.Hour), time.UTC, `"cli"`)
	if got, ok := r.LocateLaunchSession(cwd, time.Time{}); ok {
		t.Fatalf("lookup = %q with two sessions in the directory, want none", got)
	}
	if _, ok := r.LocateLaunchSession("", time.Time{}); ok {
		t.Fatal("lookup with an empty working directory found a session")
	}
}
