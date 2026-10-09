package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
)

// A PreToolUse payload is written to the spool as one file named after its
// hook; without a spool the helper is inert; a bad invocation never blocks.
func TestRun(t *testing.T) {
	spool := t.TempDir()
	env := []string{harnesscore.EnvSpool + "=" + spool, harnesscore.EnvHome + "=" + t.TempDir()}
	payload := `{"session_id":"s-1","transcript_path":"/nowhere.jsonl","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_1","tool_input":{"command":"echo hi"}}`
	var out, errs bytes.Buffer
	if code := run([]string{"claude", harnesscore.HookArgPreToolUse}, env, strings.NewReader(payload), &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	files, _ := filepath.Glob(filepath.Join(spool, "*-"+harnesscore.HookArgPreToolUse+"-*.json"))
	if len(files) != 1 {
		t.Fatalf("spool holds %v, want one pre-tool-use file (stderr %q)", files, errs.String())
	}
	sc, err := harnesscore.ReadSpool(spool)
	if err != nil || len(sc.Batches) != 1 || len(sc.Batches[0].Events) != 1 || sc.Batches[0].Events[0].Event.ToolUseID != "toolu_1" {
		t.Errorf("ReadSpool = %+v %v", sc, err)
	}

	empty := t.TempDir()
	if code := run([]string{"claude", harnesscore.HookArgPreToolUse}, nil, strings.NewReader(payload), &out, &errs); code != 0 {
		t.Errorf("without a spool: exit %d", code)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("wrote without a spool: %v", entries)
	}
	if code := run([]string{"codex", "stop"}, env, strings.NewReader(payload), &out, &errs); code != 0 {
		t.Errorf("a bad invocation: exit %d", code)
	}
}

// The TUI's permission hook hands claude the transport's answer on stdout;
// outside a TUI Session it is inert.
func TestRunPermission(t *testing.T) {
	payload := `{"hook_event_name":"PermissionRequest","session_id":"s","prompt_id":"p","tool_name":"Bash","tool_input":{"command":"touch x"}}`
	var out, errs bytes.Buffer
	if code := run([]string{live.Harness, live.PermissionArg}, nil, strings.NewReader(payload), &out, &errs); code != 0 || out.Len() != 0 {
		t.Fatalf("outside a Session: exit %d, wrote %q", code, out.String())
	}
	dir := t.TempDir()
	if err := live.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			evs, _ := live.Read(dir)
			for _, ev := range evs {
				if ev.Ask != nil && ev.Hook == live.HookPermission {
					_ = live.WriteAnswer(dir, ev.Ask.ID, live.Answer{Decision: []byte(`{"behavior":"deny","message":"no"}`)})
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	code := run([]string{live.Harness, live.PermissionArg}, []string{live.EnvDir + "=" + dir}, strings.NewReader(payload), &out, &errs)
	if code != 0 || !strings.Contains(out.String(), `"behavior":"deny"`) {
		t.Errorf("exit %d, wrote %q (stderr %q)", code, out.String(), errs.String())
	}
}
