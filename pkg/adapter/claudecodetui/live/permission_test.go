package live

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// permission runs the PermissionRequest hook in the background; its result
// comes on the channel: what it wrote for claude, and its error.
func permission(t *testing.T, dir string) chan [2]any {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": HookPermission, "session_id": "s", "prompt_id": "p1", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "touch x"}, "permission_suggestions": []any{map[string]any{"type": "addRules"}},
	})
	done := make(chan [2]any, 1)
	go func() {
		var out bytes.Buffer
		err := HandlePermission([]string{EnvDir + "=" + dir}, payload, &out)
		done <- [2]any{out.String(), err}
	}()
	return done
}

// awaitAsk waits for the hook's ask.
func awaitAsk(t *testing.T, dir string) Event {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		evs, _ := Read(dir)
		for _, ev := range evs {
			if ev.Hook == HookPermission {
				Remove(dir, ev)
				return ev
			}
		}
	}
	t.Fatal("no ask")
	return Event{}
}

// The hook reports the ask, waits for its answer with no deadline of its own,
// hands claude the decision, and says it did; a withdrawal hands claude
// nothing; with the Session's directory gone it ends.
func TestHandlePermission(t *testing.T) {
	dir := t.TempDir()
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	done := permission(t, dir)
	ev := awaitAsk(t, dir)
	a := ev.Ask
	if a == nil || !validID.MatchString(a.ID) || a.Tool != "Bash" || a.PID != os.Getpid() || ev.PromptID != "p1" ||
		string(a.Input) != `{"command":"touch x"}` || string(a.Suggestions) != `[{"type":"addRules"}]` {
		t.Fatalf("the ask: %+v %+v", ev, a)
	}
	select {
	case r := <-done:
		t.Fatalf("the hook ended with no answer: %v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := WriteAnswer(dir, a.ID, Answer{Decision: json.RawMessage(`{"behavior":"allow"}`)}); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r[1] != nil || r[0] != `{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}`+"\n" {
		t.Errorf("the hook wrote %q, %v", r[0], r[1])
	}
	evs, _ := Read(dir)
	if len(evs) != 1 || evs[0].Hook != HookDelivered || evs[0].Ask == nil || evs[0].Ask.ID != a.ID {
		t.Errorf("after the answer: %+v", evs)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, answersDir)); len(left) != 0 {
		t.Errorf("answers left: %v", left)
	}
	for _, e := range evs {
		Remove(dir, e)
	}

	done = permission(t, dir)
	ev = awaitAsk(t, dir)
	if err := WriteAnswer(dir, ev.Ask.ID, Answer{}); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r[0] != "" || r[1] != nil {
		t.Errorf("withdrawn: the hook wrote %q, %v", r[0], r[1])
	}
	if evs, _ := Read(dir); len(evs) != 0 {
		t.Errorf("withdrawn: events %+v", evs)
	}

	done = permission(t, dir)
	awaitAsk(t, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r[0] != "" {
			t.Errorf("the directory gone: the hook wrote %q", r[0])
		}
	case <-time.After(5 * time.Second):
		t.Error("the hook outlived the Session's directory")
	}
	if err := WriteAnswer(t.TempDir(), "../x", Answer{}); err == nil {
		t.Error("an answer named outside the directory was written")
	}
}
