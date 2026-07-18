package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// TestExtractSessionID_ScrapesResumeHint locks in the legacy (≤0.141) path:
// when the screen still renders the "codex resume <uuid>" hint, ExtractSessionID
// scrapes it.
func TestExtractSessionID_ScrapesResumeHint(t *testing.T) {
	const uuid = "019f0263-cdb9-7013-a43a-4eb1f65d94f1"
	scr := screen.New(120, 40)
	_, _ = scr.Write([]byte("\x1b[H\x1b[2JTo continue this session, run codex resume " + uuid + "\r\n"))

	id, ok := New().ExtractSessionID(scr.Snapshot())
	if !ok || id != uuid {
		t.Fatalf("ExtractSessionID = %q ok=%v, want %q ok=true", id, ok, uuid)
	}
}

// TestExtractSessionID_GapOn0142Screen is the regression that captures the
// reported bug: Codex 0.142 no longer prints the resume hint, so the
// screen-scrape extractor finds nothing. This documents WHY the disk-based
// LocateSessionID fallback exists — if a future Codex restores the on-screen
// hint and this starts returning ok=true, that's a signal to revisit the
// fallback, not a failure.
func TestExtractSessionID_GapOn0142Screen(t *testing.T) {
	// A representative post-turn 0.142 screen: reply text + composer + footer,
	// with no "resume" marker anywhere.
	const post0142 = "26. line twenty-six\r\n" +
		"… (earlier lines scrolled off) …\r\n" +
		"› Summarize recent commits\r\n" +
		"gpt-5.5 default · 1.2M tokens left\r\n"
	scr := screen.New(120, 40)
	_, _ = scr.Write([]byte("\x1b[H\x1b[2J" + post0142))

	if id, ok := New().ExtractSessionID(scr.Snapshot()); ok {
		t.Fatalf("ExtractSessionID unexpectedly scraped %q from a 0.142 screen with no resume hint; "+
			"the disk fallback is what should recover the id", id)
	}
}

// TestAdapterImplementsSessionIDLocator guards the capability wiring: the chat
// layer only invokes the disk fallback if the codex adapter satisfies
// turns.SessionIDLocator. If someone drops the method this fails at compile
// time via the assignment and at run time here.
func TestAdapterImplementsSessionIDLocator(t *testing.T) {
	var _ turns.SessionIDLocator = New()
}

// TestLocateSessionID_RecoversFromDisk is the end-to-end demonstration of the
// fix: with no resume hint ever on screen, the adapter recovers the session id
// from the on-disk rollout's session_meta, keyed on the working directory.
func TestLocateSessionID_RecoversFromDisk(t *testing.T) {
	root := t.TempDir()
	cwd := "/work/proj"
	const uuid = "019f0263-cdb9-7013-a43a-4eb1f65d94f1"

	dir := filepath.Join(root, "2026", "06", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-06-26T05:25:23.303Z","type":"session_meta","payload":{"session_id":"` + uuid + `","cwd":"` + cwd + `","cli_version":"0.142.0"}}` + "\n"
	path := filepath.Join(dir, "rollout-2026-06-26T07-25-23-"+uuid+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, time.Now(), time.Now())

	a := New()
	a.SessionsRoot = root // test seam: override default ~/.codex/sessions

	id, ok := a.LocateSessionID(cwd)
	if !ok || id != uuid {
		t.Fatalf("LocateSessionID = %q ok=%v, want %q ok=true", id, ok, uuid)
	}
}
