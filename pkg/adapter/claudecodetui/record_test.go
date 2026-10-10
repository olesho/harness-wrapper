package claudecodetui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	tclaude "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

const recSession = "1b399a27-8232-480e-803d-e21e0078f21b"

// entry is a transcript line in claude 2.1.283's TUI shape.
func entry(typ, uuid, parent, promptID, ts string, content any, stop string) string {
	m := map[string]any{"type": typ, "uuid": uuid, "parentUuid": parent, "isSidechain": false, "timestamp": ts, "sessionId": recSession}
	msg := map[string]any{"role": typ, "content": content}
	if typ == "assistant" {
		msg["stop_reason"], msg["id"] = stop, "msg_"+uuid[:8]
	}
	m["message"] = msg
	if promptID != "" {
		m["promptId"] = promptID
		if _, prompt := content.(string); prompt {
			m["promptSource"] = "typed"
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// The TUI record matches a prompt entry to its input by the prompt id the
// UserPromptSubmit hook bound, and takes a fresh session's first reply, which
// claude may write before the prompt, as that prompt's — read whole, or in
// small reads.
func TestRecordBindsPrompts(t *testing.T) {
	text := func(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }
	lines := []string{
		`{"type":"mode","mode":"normal","sessionId":"` + recSession + `"}`,
		// the race: the reply is on disk before its prompt
		entry("assistant", "6032e365-e6f9-4986-9df2-b2fe0472daef", "9c334cc1-0000-4000-8000-000000000000", "", "2026-10-07T15:54:29.947Z", text("PONG 1"), "end_turn"),
		entry("user", "535ab12f-992d-4d9b-9f70-90fa7e8ffa28", "", "p-one", "2026-10-07T15:54:29.830Z", "PING 1", ""),
		entry("user", "7a000000-0000-4000-8000-000000000001", "6032e365-e6f9-4986-9df2-b2fe0472daef", "p-two", "2026-10-07T15:55:00.000Z", "TOOL echo hi", ""),
		entry("assistant", "7a000000-0000-4000-8000-000000000002", "7a000000-0000-4000-8000-000000000001", "", "2026-10-07T15:55:00.100Z",
			[]map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "echo hi"}}}, "tool_use"),
		entry("user", "7a000000-0000-4000-8000-000000000003", "7a000000-0000-4000-8000-000000000002", "p-two", "2026-10-07T15:55:00.200Z",
			[]map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "hi"}}, ""),
		entry("assistant", "7a000000-0000-4000-8000-000000000004", "7a000000-0000-4000-8000-000000000003", "", "2026-10-07T15:55:00.300Z", text("TOOL DONE: hi"), "end_turn"),
		// a prompt no input was bound to: typed by someone else
		entry("user", "7a000000-0000-4000-8000-000000000005", "7a000000-0000-4000-8000-000000000004", "p-other", "2026-10-07T15:56:00.000Z", "hello", ""),
		entry("assistant", "7a000000-0000-4000-8000-000000000006", "7a000000-0000-4000-8000-000000000005", "", "2026-10-07T15:56:00.100Z", text("ok"), "end_turn"),
	}
	for _, max := range []int{contract.MaxObserveBytes, 256} {
		l, oc, m := tuiAgent(t, strings.Join(lines, "\n")+"\n", map[string]string{"in-one": "n-one", "in-two": "n-two"}, map[string]string{"p-one": "n-one", "p-two": "n-two"})
		saved := promptLagFor
		promptLagFor = 50 * time.Millisecond
		r, err := Profile{}.Record(adapter.RecordSource{SessionID: recSession, OpenConfig: oc, Layout: l, Markers: m})
		promptLagFor = saved
		if err != nil {
			t.Fatal(err)
		}
		items := readAll(t, r, max)
		got := map[string]string{}
		for _, o := range items {
			got[string(o.Kind)+":"+o.Entry] = o.InputID
		}
		for k, want := range map[string]string{
			"assistant_text:6032e365-e6f9-4986-9df2-b2fe0472daef": "in-one",
			"user_input:535ab12f-992d-4d9b-9f70-90fa7e8ffa28":     "in-one",
			"user_input:7a000000-0000-4000-8000-000000000001":     "in-two",
			"tool_use:7a000000-0000-4000-8000-000000000002":       "in-two",
			"tool_result:7a000000-0000-4000-8000-000000000003":    "in-two",
			"assistant_text:7a000000-0000-4000-8000-000000000004": "in-two",
			"user_input:7a000000-0000-4000-8000-000000000005":     "",
			"assistant_text:7a000000-0000-4000-8000-000000000006": "",
		} {
			if in, ok := got[k]; !ok || in != want {
				t.Errorf("max %d: %s: input %q (delivered %v), want %q", max, k, in, ok, want)
			}
		}
		ends := 0
		for _, o := range items {
			if o.Kind == contract.KindTurnEnded {
				ends++
			}
		}
		if ends != 2 {
			t.Errorf("max %d: %d turn ends, want 2", max, ends)
		}
		for in, want := range map[string]contract.RecoveredOutcome{"in-one": contract.RecoveredCompleted, "in-two": contract.RecoveredCompleted} {
			mk, _, _ := m.Lookup(in)
			if got, err := r.Recover(context.Background(), mk); err != nil || got.Outcome != want {
				t.Errorf("Recover(%s) = %+v %v, want %s", in, got, err, want)
			}
		}
	}
}

// tuiAgent lays out an agent of this profile whose Session's transcript is
// data, with a marker for each input and a binding for each prompt.
func tuiAgent(t *testing.T, data string, inputs, bound map[string]string) (contract.Layout, []byte, *adapter.Markers) {
	t.Helper()
	base := t.TempDir()
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"), Workspace: filepath.Join(base, "workspace"),
		Secrets: filepath.Join(base, "secrets"), Scratch: filepath.Join(base, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Profile{}.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/claude-code", Layout: l, Spec: contract.AgentSpec{PermissionPosture: contract.PostureBypass}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := claudecode.ParseOpenConfig(res.OpenConfig)
	f, err := tclaude.FollowEntries(recSession, cfg.WorkingDir, cfg.Env, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path(), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := live.Dir(cfg.Spool, recSession)
	if err := live.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	for p, n := range bound {
		if err := os.WriteFile(filepath.Join(dir, "prompts", p), []byte(n+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := adapter.OpenMarkers(l.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	for in, native := range inputs {
		if err := m.Write(adapter.Marker{InputID: in, Native: native, SessionID: recSession, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return l, res.OpenConfig, m
}

// readAll reads the record to its end, chunk by chunk, committing each; a
// read the record holds back is read again until it is given.
func readAll(t *testing.T, r adapter.Reader, max int) []contract.Observation {
	t.Helper()
	var items []contract.Observation
	empty := time.Now()
	for i := 0; i < 10000; i++ {
		ch, err := r.Read(context.Background(), max)
		if err != nil {
			t.Fatal(err)
		}
		if len(ch.Items) == 0 && ch.Checkpoint == nil && ch.Reset == nil && ch.Rescan == nil && len(ch.Faults) == 0 {
			if time.Since(empty) > time.Second {
				return items
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		empty = time.Now()
		items = append(items, ch.Items...)
		if err := r.Commit(ch); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the record never ends")
	return nil
}

// The TUI record takes a read's entries in the order of their times: the Stop
// hooks' summary after a reply claude wrote before its prompt still ends the
// turn, once, read whole or in small reads.
func TestRecordEndsAfterStopHooks(t *testing.T) {
	text := func(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }
	lines := []string{
		entry("assistant", "6032e365-e6f9-4986-9df2-b2fe0472daef", "9c334cc1-0000-4000-8000-000000000000", "", "2026-10-07T15:54:29.947Z", text("PONG 1"), "end_turn"),
		entry("user", "535ab12f-992d-4d9b-9f70-90fa7e8ffa28", "", "p-one", "2026-10-07T15:54:29.830Z", "PING 1", ""),
		`{"type":"system","subtype":"stop_hook_summary","uuid":"7b000000-0000-4000-8000-000000000001","parentUuid":"6032e365-e6f9-4986-9df2-b2fe0472daef","isSidechain":false,"hookCount":1,"preventedContinuation":false,"timestamp":"2026-10-07T15:54:29.990Z","sessionId":"` + recSession + `"}`,
	}
	for _, max := range []int{contract.MaxObserveBytes, 256} {
		l, oc, m := tuiAgent(t, strings.Join(lines, "\n")+"\n", map[string]string{"in-one": "n-one"}, map[string]string{"p-one": "n-one"})
		saved := promptLagFor
		promptLagFor = 50 * time.Millisecond
		r, err := Profile{}.Record(adapter.RecordSource{SessionID: recSession, OpenConfig: oc, Layout: l, Markers: m})
		promptLagFor = saved
		if err != nil {
			t.Fatal(err)
		}
		var ends []contract.Observation
		for _, o := range readAll(t, r, max) {
			if o.Kind == contract.KindTurnEnded {
				ends = append(ends, o)
			}
		}
		if len(ends) != 1 || ends[0].InputID != "in-one" {
			t.Errorf("max %d: turn ends %+v, want in-one's once", max, ends)
		}
	}
}
