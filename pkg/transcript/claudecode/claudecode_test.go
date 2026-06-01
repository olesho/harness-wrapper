package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func TestEncodedCWD(t *testing.T) {
	got := EncodedCWD("/Users/olehluchkiv/Work/aether/harness-wrapper")
	want := "-Users-olehluchkiv-Work-aether-harness-wrapper"
	if got != want {
		t.Errorf("EncodedCWD: got %q want %q", got, want)
	}
}

// TestEncodedCWD_NonAlphanumeric is the regression test for the empty-transcript
// bug: paths under ~/.loom (the fleet/daemon worktree root) contain a '.', which
// Claude Code encodes to '-'. The old '/'-only encoding produced the wrong dir
// ("-...-.loom-...") so the transcript file was never found.
func TestEncodedCWD_NonAlphanumeric(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/oleh/.loom/WS/agent", "-Users-oleh--loom-WS-agent"}, // the bug: '.' → '-'
		{"/a/b_c/d.e", "-a-b-c-d-e"},                                 // underscores + dots
		{"/with-dash/x", "-with-dash-x"},                             // existing '-' preserved (no collapsing)
		{"/p/v1.2.3", "-p-v1-2-3"},
	}
	for _, tc := range cases {
		if got := EncodedCWD(tc.in); got != tc.want {
			t.Errorf("EncodedCWD(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadAgainstFixture(t *testing.T) {
	dir := t.TempDir()
	cwd := "/some/work/dir"
	projDir := filepath.Join(dir, "projects", EncodedCWD(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-05-14T12:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"text","text":"there"}]},"timestamp":"2026-05-14T12:00:05Z"}
{"type":"system"}
{"type":"attachment"}
`
	path := filepath.Join(projDir, "sess-uuid.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Reader{ProjectsRoot: filepath.Join(dir, "projects")}
	evs, err := r.Read("sess-uuid", cwd)
	if err != nil {
		t.Fatal(err)
	}
	// Tool-aware parser emits ONE event per content block, so the assistant
	// line's two text blocks become two events (not one joined turn).
	if len(evs) != 3 {
		t.Fatalf("expected 3 events, got %d", len(evs))
	}
	if evs[0].Role != "user" || evs[0].Text != "hello" {
		t.Errorf("event 0 mismatch: %+v", evs[0])
	}
	if evs[1].Role != "assistant" || evs[1].Text != "hi" {
		t.Errorf("event 1 mismatch: %+v", evs[1])
	}
	if evs[2].Role != "assistant" || evs[2].Text != "there" {
		t.Errorf("event 2 mismatch: %+v", evs[2])
	}
}

// TestReadReturnsCanonicalEvents locks the P2b contract: Read returns canonical
// transcript.Event values (Type=text, Source=file) with the native message UUID
// captured for dedup identity.
func TestReadReturnsCanonicalEvents(t *testing.T) {
	dir := t.TempDir()
	cwd := "/some/work/dir"
	projDir := filepath.Join(dir, "projects", EncodedCWD(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","uuid":"u-1","message":{"role":"user","content":"hello"},"timestamp":"2026-05-14T12:00:00Z"}
`
	if err := os.WriteFile(filepath.Join(projDir, "sess.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	evs, err := (&Reader{ProjectsRoot: filepath.Join(dir, "projects")}).Read("sess", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	e := evs[0]
	if e.Type != transcript.EventText {
		t.Errorf("Type = %q, want %q", e.Type, transcript.EventText)
	}
	if e.Source != transcript.SourceFile {
		t.Errorf("Source = %q, want %q", e.Source, transcript.SourceFile)
	}
	if e.UUID != "u-1" {
		t.Errorf("UUID = %q, want u-1 (native message uuid)", e.UUID)
	}
	// Text events carry a source-prefixed, per-event NativeID (line uuid alone
	// would collapse multiple blocks from one line); ID() returns it.
	if e.NativeID == "" || e.ID() != e.NativeID {
		t.Errorf("NativeID/ID() = %q / %q, want non-empty and equal", e.NativeID, e.ID())
	}
}

// TestEventsToolAware locks the parity behavior: tool_use/tool_result blocks
// become distinct tool events (not dropped), with stable cross-source tool ids.
func TestEventsToolAware(t *testing.T) {
	body := `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"running"},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]},"timestamp":"2026-05-14T12:00:00Z"}
{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"file.go"}]}]},"timestamp":"2026-05-14T12:00:01Z"}
`
	evs, err := Events([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3 (text + tool_use + tool_result)", len(evs))
	}
	if evs[1].Type != transcript.EventToolUse || evs[1].ToolName != "Bash" || evs[1].ToolUseID != "tu_1" {
		t.Errorf("tool_use event wrong: %+v", evs[1])
	}
	if evs[1].ID() != "tool-use:tu_1" {
		t.Errorf("tool_use ID() = %q, want tool-use:tu_1", evs[1].ID())
	}
	if evs[2].Type != transcript.EventToolResult || evs[2].ToolUseID != "tu_1" || evs[2].Output == "" {
		t.Errorf("tool_result event wrong: %+v", evs[2])
	}
	if evs[2].ID() != "tool-result:tu_1" {
		t.Errorf("tool_result ID() = %q, want tool-result:tu_1", evs[2].ID())
	}
}

func TestReadMissingFile(t *testing.T) {
	r := &Reader{ProjectsRoot: t.TempDir()}
	if _, err := r.Read("missing", "/no/such/dir"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// Smoke-test against the real ~/.claude/projects/ if it exists.
func TestReadAgainstRealCorpus(t *testing.T) {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	// cwd is .../pkg/transcript/claudecode; walk up to repo root.
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			break
		}
		cwd = filepath.Dir(cwd)
	}
	projDir := filepath.Join(home, ".claude", "projects", EncodedCWD(cwd))
	entries, err := os.ReadDir(projDir)
	if err != nil {
		t.Skipf("no Claude Code transcripts at %s", projDir)
	}
	var sessionUUID string
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".jsonl" {
			sessionUUID = name[:len(name)-len(".jsonl")]
			break
		}
	}
	if sessionUUID == "" {
		t.Skip("no .jsonl files in project dir")
	}
	if _, err := New().Read(sessionUUID, cwd); err != nil {
		t.Errorf("Read against real corpus failed: %v", err)
	}
}
