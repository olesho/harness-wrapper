package pi

import (
	"os"
	"path/filepath"
	"testing"
)

const sessionUUID = "0281fd4a-0a10-4dfe-adca-9b61b3777255"

// writeSession lays down a pi-shaped session file under
// <root>/sessions/<dir>/<name> and returns the config root to hand to a
// Reader.
func writeSession(t *testing.T, dir, name, body string) string {
	t.Helper()
	root := t.TempDir()
	sessDir := filepath.Join(root, "sessions", dir)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root
}

// canonicalBody is a representative session: header + a string-content
// user message, a block-content assistant message, a skipped
// model_change control line, a toolResult (→ system), and an empty
// assistant message that must be dropped.
const canonicalBody = `{"type":"session","version":3,"id":"` + sessionUUID + `","timestamp":"2024-12-03T14:00:00.000Z","cwd":"/work/proj"}
{"type":"message","id":"a1b2c3d4","parentId":null,"timestamp":"2024-12-03T14:00:01.000Z","message":{"role":"user","content":"Hello pi"}}
{"type":"model_change","id":"mc000001","timestamp":"2024-12-03T14:00:01.500Z","model":"claude-sonnet-4-5"}
{"type":"message","id":"b2c3d4e5","parentId":"a1b2c3d4","timestamp":"2024-12-03T14:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"},{"type":"text","text":"How can I help?"}],"provider":"anthropic","model":"claude-sonnet-4-5"}}
{"type":"message","id":"c3d4e5f6","parentId":"b2c3d4e5","timestamp":"2024-12-03T14:00:03.000Z","message":{"role":"toolResult","toolCallId":"call_123","toolName":"bash","content":[{"type":"text","text":"output"}],"isError":false}}
{"type":"message","id":"d4e5f6a7","parentId":"c3d4e5f6","timestamp":"2024-12-03T14:00:04.000Z","message":{"role":"assistant","content":[{"type":"image","source":"…"}]}}
`

func TestRead_CanonicalSession(t *testing.T) {
	root := writeSession(t, slugForCwd("/work/proj"), "20241203T140000_"+sessionUUID+".jsonl", canonicalBody)

	turns, err := (&Reader{Root: root}).Read(sessionUUID, "/work/proj")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("want 3 turns (user, assistant, toolResult→system), got %d: %+v", len(turns), turns)
	}

	if turns[0].Role != "user" || turns[0].Text != "Hello pi" {
		t.Errorf("turn0 = {%q,%q}, want user/Hello pi", turns[0].Role, turns[0].Text)
	}
	if turns[1].Role != "assistant" || turns[1].Text != "Hi!\n\nHow can I help?" {
		t.Errorf("turn1 = {%q,%q}, want assistant with joined text blocks", turns[1].Role, turns[1].Text)
	}
	if turns[2].Role != "system" || turns[2].Text != "output" {
		t.Errorf("turn2 = {%q,%q}, want system/output (toolResult)", turns[2].Role, turns[2].Text)
	}
	if turns[0].Timestamp.IsZero() {
		t.Error("expected a parsed RFC3339 timestamp on turn0")
	}
}

// With no working directory the slug fast-path is skipped and locate()
// must find the file by walking every sessions/*/ directory.
func TestRead_WalkFallbackWithoutWorkingDir(t *testing.T) {
	root := writeSession(t, "--some-other-slug--", "20241203T140000_"+sessionUUID+".jsonl", canonicalBody)

	turns, err := (&Reader{Root: root}).Read(sessionUUID, "")
	if err != nil {
		t.Fatalf("Read (walk): %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("walk fallback: want 3 turns, got %d", len(turns))
	}
}

// A filename can contain a shared substring; the header "id" is the
// authority. A file named for the right id but whose header disagrees
// must not be returned.
func TestRead_HeaderMismatchIsNotReturned(t *testing.T) {
	body := `{"type":"session","version":3,"id":"some-other-id","timestamp":"2024-12-03T14:00:00.000Z","cwd":"/work/proj"}
{"type":"message","id":"x","timestamp":"2024-12-03T14:00:01.000Z","message":{"role":"user","content":"nope"}}
`
	root := writeSession(t, slugForCwd("/work/proj"), "20241203T140000_"+sessionUUID+".jsonl", body)

	if _, err := (&Reader{Root: root}).Read(sessionUUID, "/work/proj"); err == nil {
		t.Error("expected not-found error when the header id disagrees with the requested id")
	}
}

func TestRead_EmptySessionIDErrors(t *testing.T) {
	if _, err := (&Reader{Root: t.TempDir()}).Read("", "/work/proj"); err == nil {
		t.Error("expected error for empty session id")
	}
}

func TestRead_PiCodingAgentDirEnvOverride(t *testing.T) {
	root := writeSession(t, slugForCwd("/work/proj"), "20241203T140000_"+sessionUUID+".jsonl", canonicalBody)
	t.Setenv("PI_CODING_AGENT_DIR", root)

	// Root left empty so the env override is exercised.
	turns, err := (&Reader{}).Read(sessionUUID, "/work/proj")
	if err != nil {
		t.Fatalf("Read with PI_CODING_AGENT_DIR: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("want 3 turns via env override, got %d", len(turns))
	}
}

func TestSlugForCwd(t *testing.T) {
	cases := map[string]string{
		"/home/user/myproject": "--home-user-myproject--",
		"/work/proj":           "--work-proj--",
		"/work/proj/":          "--work-proj--",
	}
	for in, want := range cases {
		if got := slugForCwd(in); got != want {
			t.Errorf("slugForCwd(%q) = %q, want %q", in, got, want)
		}
	}
}
