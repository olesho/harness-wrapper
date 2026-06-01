package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func TestResumeArgs(t *testing.T) {
	if got := (resumer{}).ResumeArgs("uuid-1"); !reflect.DeepEqual(got, []string{"--resume", "uuid-1"}) {
		t.Errorf("ResumeArgs = %v, want [--resume uuid-1]", got)
	}
	if got := (resumer{}).ResumeArgs(""); got != nil {
		t.Errorf("ResumeArgs(\"\") = %v, want nil", got)
	}
}

func TestResolveCapabilities(t *testing.T) {
	rp := Profile{}.Resolve(harness.ResolveContext{})
	if rp.Resume == nil || rp.Hooks == nil {
		t.Fatal("Resolve must populate Resume + Hooks for gemini")
	}
	// Stream/SessionID are intentionally nil (unverified stream-json schema).
	if rp.Stream != nil || rp.SessionID != nil {
		t.Error("gemini must NOT register Stream/SessionID (schema unverified)")
	}
}

func TestHookSpecShape(t *testing.T) {
	spec := hookProvider{}.HookSpec()
	if spec.ConfigPath != filepath.Join(".gemini", "settings.json") {
		t.Errorf("ConfigPath = %q", spec.ConfigPath)
	}
	byArg := map[string]harness.HookEntry{}
	for _, e := range spec.Events {
		byArg[e.Arg] = e
	}
	// Native event names differ from Claude's.
	if byArg["session-start"].NativeEvent != "SessionStart" {
		t.Errorf("session-start native = %q, want SessionStart", byArg["session-start"].NativeEvent)
	}
	if byArg["stop"].NativeEvent != "AfterAgent" {
		t.Errorf("stop native = %q, want AfterAgent (gemini turn end)", byArg["stop"].NativeEvent)
	}
	if byArg["session-end"].NativeEvent != "SessionEnd" {
		t.Errorf("session-end native = %q, want SessionEnd", byArg["session-end"].NativeEvent)
	}
	if spec.Yield == nil || spec.Yield.NativeEvent != "BeforeTool" {
		t.Errorf("Yield = %+v, want BeforeTool/yield-guard", spec.Yield)
	}
}

// writeGeminiTranscript writes a Gemini JSONL transcript under <root>/chats and
// returns its path.
func writeGeminiTranscript(t *testing.T, root, sessionID string) string {
	t.Helper()
	dir := filepath.Join(root, "tmp", "proj", "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"sessionId":"` + sessionID + `","kind":"main"}
{"role":"user","parts":[{"text":"hi gemini"}],"timestamp":"2026-05-14T10:00:01Z"}
{"role":"model","parts":[{"text":"hello there"}],"timestamp":"2026-05-14T10:00:02Z"}
`
	path := filepath.Join(dir, "session-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func payload(t *testing.T, sessionID, transcriptPath string) []byte {
	t.Helper()
	b, _ := json.Marshal(geminiHookPayload{SessionID: sessionID, TranscriptPath: transcriptPath})
	return b
}

func TestParseHookPayloadStopReadsTranscript(t *testing.T) {
	root := t.TempDir()
	tpath := writeGeminiTranscript(t, root, "g-sess")
	ctx := harness.HookContext{ConfigDir: root, Home: root}

	evs, err := hookProvider{}.ParseHookPayload(ctx, "stop", payload(t, "g-sess", tpath))
	if err != nil {
		t.Fatalf("ParseHookPayload(stop): %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
	if evs[0].HarnessSessionID != "g-sess" || evs[0].Event.Text != "hi gemini" {
		t.Errorf("event 0 wrong: %+v", evs[0])
	}
	if evs[1].Event.Role != transcript.RoleAssistant {
		t.Errorf("event 1 role = %q, want assistant (model→assistant)", evs[1].Event.Role)
	}
}

func TestParseHookPayloadSessionStartMarker(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir()}
	evs, err := hookProvider{}.ParseHookPayload(ctx, "session-start", payload(t, "g9", "/ignored"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Event.Type != transcript.EventSessionMeta || evs[0].HarnessSessionID != "g9" {
		t.Fatalf("got %+v, want one session marker for g9", evs)
	}
}

func TestParseHookPayloadRejectsBadPaths(t *testing.T) {
	root := t.TempDir()
	ctx := harness.HookContext{ConfigDir: root, Home: root}
	// Outside the gemini root.
	outside := filepath.Join(t.TempDir(), "x.jsonl")
	_ = os.WriteFile(outside, []byte(`{"sessionId":"s"}`), 0o600)
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "stop", payload(t, "s", outside)); err == nil {
		t.Error("expected error for transcript_path outside the gemini root")
	}
	// Inside the root but wrong extension.
	notJSONL := filepath.Join(root, "notes.txt")
	_ = os.WriteFile(notJSONL, []byte("x"), 0o600)
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "stop", payload(t, "s", notJSONL)); err == nil {
		t.Error("expected error for non-.jsonl transcript_path")
	}
}

func TestParseHookPayloadUnknownEventAndEmptySession(t *testing.T) {
	ctx := harness.HookContext{Home: t.TempDir()}
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "bogus", payload(t, "s", "/x")); err == nil {
		t.Error("expected error for unknown event")
	}
	if _, err := (hookProvider{}).ParseHookPayload(ctx, "stop", payload(t, "", "/x")); err == nil {
		t.Error("expected error for empty session_id")
	}
}

func TestEnsureConfigWritesGeminiSettings(t *testing.T) {
	wt := t.TempDir()
	if err := (hookProvider{}).EnsureConfig(wt, []string{"/abs/loom", "hooks"}); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(wt, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("gemini settings.json not written: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("invalid settings.json: %v", err)
	}
	var hooks map[string][]harness.SettingsHookMatcher
	if err := json.Unmarshal(top["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"SessionStart", "AfterAgent", "SessionEnd", "BeforeTool"} {
		if len(hooks[ev]) == 0 {
			t.Errorf("gemini event %s missing", ev)
		}
	}
	// The command names gemini (not claude) and is shell-guarded.
	cmd := hooks["AfterAgent"][0].Hooks[0].Command
	if !harness.IsManagedHookCommand(cmd) {
		t.Errorf("AfterAgent command not owner-marked: %s", cmd)
	}
}

func TestRegisteredViaInit(t *testing.T) {
	p, ok := harness.For("gemini")
	if !ok || p.Name() != "gemini" {
		t.Fatalf("gemini not registered: ok=%v", ok)
	}
}
