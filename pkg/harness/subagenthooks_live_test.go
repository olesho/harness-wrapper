package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// TestSubagentHooksLive runs the real claude on a real account with the
// default hook spec installed as Run installs it, rendered to this binary as
// the hook command, and has it delegate one question to a subagent — once
// waiting for it, once in the background. Either way the spool must hold the
// subagent's start marker from claude's SubagentStart, and its transcript and
// stop marker from SubagentStop — all tagged with the subagent's session
// under the parent's — and nothing from the retired pre-task. When claude
// waited, post-task reads the same transcript again, with the same ids; in
// the background, post-task fires at the launch and has nothing to read
// (ADR-011). It spends two short turns on a small model:
//
//	HW_LIVE_ACCOUNT=1 HW_LIVE_TOKEN_FILE=<file holding a `claude setup-token` token> \
//	  go test ./pkg/harness -run SubagentHooksLive -v
//
// The token is read from the file and passed only as CLAUDE_CODE_OAUTH_TOKEN
// in claude's environment; it is never logged. HW_LIVE_MODEL overrides the
// model (default haiku).
func TestSubagentHooksLive(t *testing.T) {
	if os.Getenv("HW_LIVE_ACCOUNT") != "1" {
		t.Skip("uses a real account: set HW_LIVE_ACCOUNT=1 and HW_LIVE_TOKEN_FILE (spends two short turns)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	tok, err := os.ReadFile(os.Getenv("HW_LIVE_TOKEN_FILE"))
	if err != nil || len(strings.TrimSpace(string(tok))) == 0 {
		t.Fatalf("HW_LIVE_TOKEN_FILE must name a file holding the token: %v", err)
	}
	model := os.Getenv("HW_LIVE_MODEL")
	if model == "" {
		model = "haiku"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	version, _ := exec.Command(bin, "--version").Output()
	t.Logf("claude %s", strings.TrimSpace(string(version)))
	const question = "'What is 17+25? Reply with only the number.'"

	// The prompt asks for each mode; the checks follow the mode claude
	// actually chose, read from its Agent call.
	for _, tc := range []struct{ name, how string }{
		{"waited", "Do not run it in the background: wait for its answer."},
		{"background", "Run it with run_in_background set to true, and when it has finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			atStop, atPostTask, background := runSubagentLive(t, bin, strings.TrimSpace(string(tok)), model, self,
				"Use your subagent tool (subagent type general-purpose) to ask a subagent "+question+" "+tc.how+
					" Then reply with exactly: SUBAGENT SAID <the number>")
			t.Logf("claude ran the subagent in the background: %v", background)
			if background {
				// post-task fired at the launch: it read at most the
				// subagent's first records.
				return
			}
			// post-task, which fires after SubagentStop, read the same file
			// again: every record SubagentStop read comes again with the
			// same id and content, so a consumer dedups the copies.
			if len(atPostTask) == 0 {
				t.Fatal("post-task read nothing for a subagent claude waited for")
			}
			byID := map[string]transcript.ParsedEvent{}
			for _, pe := range atPostTask {
				byID[pe.Event.ID()] = pe
			}
			for _, pe := range atStop {
				if again, ok := byID[pe.Event.ID()]; !ok || !reflect.DeepEqual(again, pe) {
					t.Errorf("SubagentStop's %s is not in post-task's copy as it is: %+v", pe.Event.ID(), again)
				}
			}
		})
	}
}

// runSubagentLive runs one claude -p turn with prompt and checks what every
// subagent turn must spool, returning the subagent's transcript events from
// SubagentStop and from post-task, and whether claude ran the subagent in the
// background.
func runSubagentLive(t *testing.T, bin, tok, model, self, prompt string) (atStop, atPostTask []transcript.ParsedEvent, background bool) {
	t.Helper()
	root := t.TempDir()
	wd, cfg, home, spool := filepath.Join(root, "work"), filepath.Join(root, "cfg"), filepath.Join(root, "home"), filepath.Join(root, "spool")
	for _, d := range []string{wd, cfg, home, spool} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realWD, _ := filepath.EvalSymlinks(wd)
	writeJSON := func(path string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(cfg, ".claude.json"), map[string]any{
		"hasCompletedOnboarding": true, "bypassPermissionsModeAccepted": true,
		"projects": map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
	})
	settings := filepath.Join(cfg, "settings.json")
	writeJSON(settings, map[string]any{"skipDangerousModePermissionPrompt": true})
	spec := *claudeHooks(t).HookSpec()
	spec.Owner = "hw-live"
	if err := harness.EnsureSettingsJSONHooks(settings, &spec, []string{self}, "claude"); err != nil {
		t.Fatal(err)
	}

	env := append(
		harnessenv.Cleaned(),
		"CLAUDE_CONFIG_DIR="+cfg,
		"CLAUDE_CODE_OAUTH_TOKEN="+tok,
		"DISABLE_AUTOUPDATER=1",
		harness.EnvSpool+"="+spool,
		harness.EnvHookCwd+"="+wd,
		harness.EnvHome+"="+home,
		harness.EnvConfigDir+"="+cfg,
		hookHandlerEnv+"=1",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-p", "--model", model, "--permission-mode", "bypassPermissions", prompt)
	cmd.Dir, cmd.Env = wd, env
	out, err := cmd.CombinedOutput()
	t.Logf("claude -p: %v; reply %q", err, oneLine(string(out), 200))
	if err != nil {
		t.Fatalf("claude -p failed: %v", err)
	}

	got, err := harness.ReadSpool(spool)
	if err != nil {
		t.Fatalf("ReadSpool: %v", err)
	}
	var (
		parentSession string
		starts, stops []transcript.ParsedEvent
	)
	for _, b := range got.Batches {
		name := b.Receipt.Name
		if strings.HasPrefix(name, "pre-task-") {
			t.Errorf("the retired pre-task hook fired: %s", name)
		}
		for _, pe := range b.Events {
			switch {
			case strings.HasPrefix(name, "session-start-") || strings.HasPrefix(name, "user-prompt-submit-"):
				parentSession = pe.HarnessSessionID
			case strings.HasPrefix(name, harness.HookArgSubagentStart+"-"):
				starts = append(starts, pe)
			case strings.HasPrefix(name, harness.HookArgSubagentStop+"-") && pe.Event.Type == transcript.EventSubagentStop:
				stops = append(stops, pe)
			case strings.HasPrefix(name, harness.HookArgSubagentStop+"-"):
				atStop = append(atStop, pe)
			case strings.HasPrefix(name, "post-task-"):
				atPostTask = append(atPostTask, pe)
			}
		}
		t.Logf("spool %s: %d events", name, len(b.Events))
	}
	if parentSession == "" {
		t.Fatal("no session marker: the lifecycle hooks did not fire")
	}
	if len(starts) != 1 || len(stops) != 1 {
		t.Fatalf("got %d start and %d stop markers, want one each", len(starts), len(stops))
	}
	start, stop := starts[0], stops[0]
	t.Logf("start %s/%s type=%q; stop type=%q last=%q; %d transcript events at SubagentStop, %d at post-task",
		start.ParentSessionID, start.HarnessSessionID, start.Event.AgentType, stop.Event.AgentType, oneLine(stop.Event.Text, 80), len(atStop), len(atPostTask))
	if start.ParentSessionID != parentSession || start.HarnessSessionID == "" || start.HarnessSessionID == parentSession {
		t.Errorf("start sessions %q under %q, want the subagent under the parent %q", start.HarnessSessionID, start.ParentSessionID, parentSession)
	}
	if stop.HarnessSessionID != start.HarnessSessionID || stop.ParentSessionID != parentSession {
		t.Errorf("stop sessions %q under %q, want the start's", stop.HarnessSessionID, stop.ParentSessionID)
	}
	if start.Event.Source != transcript.SourceHook || stop.Event.Source != transcript.SourceHook {
		t.Errorf("marker sources %q / %q, want hook", start.Event.Source, stop.Event.Source)
	}
	if start.Event.AgentType != "general-purpose" || stop.Event.AgentType != "general-purpose" {
		t.Errorf("agent types %q / %q, want general-purpose", start.Event.AgentType, stop.Event.AgentType)
	}
	if !strings.Contains(stop.Event.Text, "42") {
		t.Errorf("stop's last reply = %q, want the subagent's answer", stop.Event.Text)
	}
	// SubagentStop waited for claude to write the reply it handed over.
	var answered bool
	for _, pe := range atStop {
		answered = answered || pe.Event.Role == transcript.RoleAssistant && strings.Contains(pe.Event.Text, "42")
	}
	if !answered {
		t.Errorf("the subagent's transcript at SubagentStop lacks its reply: %s", eventKinds(events(atStop)))
	}
	// The subagent's transcript as claude left it: whatever a hook read of
	// it comes again, with the same id, from the file itself.
	files, _ := filepath.Glob(filepath.Join(cfg, "projects", "*", parentSession, "subagents", "agent-"+start.HarnessSessionID+".jsonl"))
	if len(files) != 1 {
		t.Fatalf("the subagent's transcript under %s: %v", cfg, files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	final, err := claudecode.Events(data)
	if err != nil {
		t.Fatal(err)
	}
	inFile := map[string]transcript.Event{}
	for _, e := range final {
		inFile[e.ID()] = e
	}
	t.Logf("the subagent's transcript at claude's exit: %s; SubagentStop read %s; post-task %s",
		eventKinds(final), eventKinds(events(atStop)), eventKinds(events(atPostTask)))
	for _, pe := range append(append([]transcript.ParsedEvent(nil), atStop...), atPostTask...) {
		if pe.HarnessSessionID != start.HarnessSessionID || pe.ParentSessionID != parentSession || pe.Event.Source == transcript.SourceHook {
			t.Errorf("transcript event %+v: sessions %q under %q", pe.Event, pe.HarnessSessionID, pe.ParentSessionID)
		}
		if e, ok := inFile[pe.Event.ID()]; !ok || !reflect.DeepEqual(e, pe.Event) {
			t.Errorf("transcript event %s is not in the file as it is", pe.Event.ID())
		}
	}
	return atStop, atPostTask, ranInBackground(t, cfg, parentSession)
}

// ranInBackground reports whether the parent's subagent call, in its
// transcript, asked for the background.
func ranInBackground(t *testing.T, cfg, parentSession string) bool {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(cfg, "projects", "*", parentSession+".jsonl"))
	if len(files) != 1 {
		t.Fatalf("the parent's transcript under %s: %v", cfg, files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	evs, err := claudecode.Events(data)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	background := false
	for _, e := range evs {
		if e.Type != transcript.EventToolUse {
			continue
		}
		calls = append(calls, e.ToolName)
		var in struct {
			RunInBackground bool `json:"run_in_background"`
		}
		if (e.ToolName == "Agent" || e.ToolName == "Task") && json.Unmarshal(e.ToolInput, &in) == nil && in.RunInBackground {
			background = true
		}
	}
	t.Logf("the parent's tool calls: %v", calls)
	return background
}

func events(pes []transcript.ParsedEvent) []transcript.Event {
	out := make([]transcript.Event, len(pes))
	for i, pe := range pes {
		out[i] = pe.Event
	}
	return out
}

// eventKinds lists each event's role and type, in order.
func eventKinds(evs []transcript.Event) string {
	kinds := make([]string, len(evs))
	for i, e := range evs {
		kinds[i] = e.Role + "/" + e.Type
	}
	return "[" + strings.Join(kinds, " ") + "]"
}
