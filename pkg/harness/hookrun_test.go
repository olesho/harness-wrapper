package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	_ "github.com/olesho/harness-wrapper/pkg/harness/all" // register claude
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func TestHandleHookEventInertWithoutSpool(t *testing.T) {
	// No HW_EVENT_SPOOL ⇒ inert: returns nil and writes nothing, even for a
	// valid payload. This is what makes a leftover hook entry safe on a non-loom
	// run (review #5).
	home := t.TempDir()
	sid := "sess-1"
	dir := filepath.Join(home, ".claude", "projects", "proj")
	_ = os.MkdirAll(dir, 0o755)
	tpath := filepath.Join(dir, sid+".jsonl")
	_ = os.WriteFile(tpath, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600)
	stdin, _ := json.Marshal(map[string]string{"session_id": sid, "transcript_path": tpath})

	env := []string{harness.EnvHome + "=" + home, harness.EnvHookCwd + "=/wt"} // NO spool
	if _, err := harness.HandleHookEvent("claude", "stop", env, stdin); err != nil {
		t.Fatalf("inert handler should return nil, got %v", err)
	}
}

func TestHandleHookEventWritesSpoolAndDrains(t *testing.T) {
	home := t.TempDir()
	spool := t.TempDir()
	sid := "sess-xyz"
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tpath := filepath.Join(dir, sid+".jsonl")
	body := `{"type":"user","uuid":"u1","timestamp":"2026-05-14T12:00:00Z","message":{"role":"user","content":"hi"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-05-14T12:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
`
	if err := os.WriteFile(tpath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]string{"session_id": sid, "transcript_path": tpath})
	env := []string{
		harness.EnvSpool + "=" + spool,
		harness.EnvHome + "=" + home,
		harness.EnvHookCwd + "=/wt",
	}

	if _, err := harness.HandleHookEvent("claude", "stop", env, stdin); err != nil {
		t.Fatalf("HandleHookEvent: %v", err)
	}
	// Exactly one completed .json spool file (no leftover .tmp).
	entries, _ := os.ReadDir(spool)
	if len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".json" {
		t.Fatalf("spool dir = %v, want one .json file", names(entries))
	}

	evs, err := harness.DrainSpool(spool)
	if err != nil {
		t.Fatalf("DrainSpool: %v", err)
	}
	if len(evs) != 2 || evs[0].Event.Text != "hi" || evs[1].Event.Text != "hello" {
		t.Fatalf("drained events = %+v, want hi/hello", evs)
	}
	for _, e := range evs {
		if e.HarnessSessionID != sid {
			t.Errorf("event session id = %q, want %q", e.HarnessSessionID, sid)
		}
	}
	// Drain consumed the file: a second drain yields nothing.
	again, _ := harness.DrainSpool(spool)
	if len(again) != 0 {
		t.Errorf("second drain returned %d events, want 0 (files consumed)", len(again))
	}
}

func TestHandleHookEventSessionStartSpoolsMarker(t *testing.T) {
	spool := t.TempDir()
	stdin, _ := json.Marshal(map[string]string{"session_id": "s-77", "transcript_path": "/whatever"})
	env := []string{harness.EnvSpool + "=" + spool, harness.EnvHome + "=" + t.TempDir()}
	if _, err := harness.HandleHookEvent("claude", "session-start", env, stdin); err != nil {
		t.Fatalf("HandleHookEvent(session-start): %v", err)
	}
	evs, err := harness.DrainSpool(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Event.Type != transcript.EventSessionMeta || evs[0].HarnessSessionID != "s-77" {
		t.Fatalf("got %+v, want one session marker for s-77", evs)
	}
}

// writeClaudeTranscript writes a two-line Claude transcript for session sid
// under home/.claude/projects/proj and returns the hook stdin payload for it.
func writeClaudeTranscript(t *testing.T, home, sid string) []byte {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tpath := filepath.Join(dir, sid+".jsonl")
	body := `{"type":"user","uuid":"u1","timestamp":"2026-05-14T12:00:00Z","message":{"role":"user","content":"hi"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-05-14T12:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
`
	if err := os.WriteFile(tpath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]string{"session_id": sid, "transcript_path": tpath})
	return stdin
}

// A resume launch (HW_HARNESS_SESSION_ID set) drops a stale PARENT hook fired
// for a DIFFERENT session before it reaches the spool: env → HookContext →
// filterResumeSession end-to-end.
func TestHandleHookEventResumeGuardDropsMismatchedParent(t *testing.T) {
	home := t.TempDir()
	spool := t.TempDir()
	stdin := writeClaudeTranscript(t, home, "stale-session")
	env := []string{
		harness.EnvSpool + "=" + spool,
		harness.EnvHome + "=" + home,
		harness.EnvHookCwd + "=/wt",
		harness.EnvHarnessSessionID + "=resumed-session", // expected id ≠ fired session
	}
	if _, err := harness.HandleHookEvent("claude", "stop", env, stdin); err != nil {
		t.Fatalf("HandleHookEvent: %v", err)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("stale parent hook should write nothing on resume, spool = %v", names(entries))
	}
}

// A resume launch whose fired session MATCHES the expected id spools normally.
func TestHandleHookEventResumeGuardKeepsMatchingParent(t *testing.T) {
	home := t.TempDir()
	spool := t.TempDir()
	stdin := writeClaudeTranscript(t, home, "resumed-session")
	env := []string{
		harness.EnvSpool + "=" + spool,
		harness.EnvHome + "=" + home,
		harness.EnvHookCwd + "=/wt",
		harness.EnvHarnessSessionID + "=resumed-session", // expected id == fired session
	}
	if _, err := harness.HandleHookEvent("claude", "stop", env, stdin); err != nil {
		t.Fatalf("HandleHookEvent: %v", err)
	}
	evs, err := harness.DrainSpool(spool)
	if err != nil {
		t.Fatalf("DrainSpool: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("matching resume parent should spool both events, got %d", len(evs))
	}
}

// Without HW_HARNESS_SESSION_ID (fresh start / non-resume) the guard is disarmed:
// events spool regardless of the fired session id.
func TestHandleHookEventFreshStartDisarmsResumeGuard(t *testing.T) {
	home := t.TempDir()
	spool := t.TempDir()
	stdin := writeClaudeTranscript(t, home, "any-session")
	env := []string{
		harness.EnvSpool + "=" + spool,
		harness.EnvHome + "=" + home,
		harness.EnvHookCwd + "=/wt",
		// NO HW_HARNESS_SESSION_ID
	}
	if _, err := harness.HandleHookEvent("claude", "stop", env, stdin); err != nil {
		t.Fatalf("HandleHookEvent: %v", err)
	}
	evs, err := harness.DrainSpool(spool)
	if err != nil {
		t.Fatalf("DrainSpool: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("fresh start should spool all events, got %d", len(evs))
	}
}

func TestHandleHookEventUnknownHarness(t *testing.T) {
	env := []string{harness.EnvSpool + "=" + t.TempDir()}
	if _, err := harness.HandleHookEvent("nonesuch", "stop", env, []byte("{}")); err == nil {
		t.Fatal("expected error for unregistered harness")
	}
}

func TestDrainSpoolConcurrentWritersNoTornReads(t *testing.T) {
	// Many writers racing (temp+rename) produce only whole files; the drain sees
	// every event exactly once, never a partial record.
	spool := t.TempDir()
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Reach into the package via an exported helper path: emulate a hook
			// writing one event by spooling through HandleHookEvent (session-start
			// needs only a session id, no file).
			stdin, _ := json.Marshal(map[string]string{"session_id": strconv.Itoa(i), "transcript_path": "/x"})
			env := []string{harness.EnvSpool + "=" + spool, harness.EnvHome + "=/h"}
			_, _ = harness.HandleHookEvent("claude", "session-start", env, stdin)
		}(i)
	}
	wg.Wait()

	evs, err := harness.DrainSpool(spool)
	if err != nil {
		t.Fatalf("DrainSpool: %v", err)
	}
	if len(evs) != n {
		t.Fatalf("drained %d events, want %d (no lost/torn writes)", len(evs), n)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		if seen[e.HarnessSessionID] {
			t.Errorf("duplicate session id %q", e.HarnessSessionID)
		}
		seen[e.HarnessSessionID] = true
	}
}

func TestDrainSpoolMissingDirNotError(t *testing.T) {
	evs, err := harness.DrainSpool(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || evs != nil {
		t.Fatalf("missing spool dir: got (%v, %v), want (nil, nil)", evs, err)
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}
