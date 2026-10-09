package live

import (
	"encoding/json"
	"reflect"
	"testing"
)

func hook(t *testing.T, dir, arg string, payload map[string]any) {
	t.Helper()
	b, _ := json.Marshal(payload)
	if err := HandleHook(arg, []string{EnvDir + "=" + dir}, b); err != nil {
		t.Fatalf("HandleHook(%s): %v", arg, err)
	}
}

// The UserPromptSubmit hook binds the input being typed to claude's prompt
// id, once: not a prompt of claude's own, not a queued prompt that reports a
// running turn's id; and every hook's event reaches Read, in order.
func TestBinding(t *testing.T) {
	dir := t.TempDir()
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, "session-start", map[string]any{"hook_event_name": HookSessionStart, "session_id": "s", "source": "startup"})
	if err := SetPending(dir, "n1"); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p-notify", "prompt": "<task-notification>x"})
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p1", "prompt": "PING 1"})
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p1", "prompt": "queued"})
	hook(t, dir, "stop", map[string]any{"hook_event_name": HookStop, "prompt_id": "p1", "last_assistant_message": "PONG 1", "background_tasks": []any{
		map[string]any{"id": "b1", "type": "shell", "description": "sleep", "status": "running"},
		map[string]any{"type": "shell"}, // no id: dropped
	}})
	hook(t, dir, "stop-failure", map[string]any{"hook_event_name": HookStopFailure, "prompt_id": "p1", "error": "server_error"})

	if n, ok := Bound(dir, "p1"); !ok || n != "n1" {
		t.Errorf("Bound(p1) = %q %v", n, ok)
	}
	if _, ok := Bound(dir, "p-notify"); ok {
		t.Error("claude's own prompt was bound")
	}
	if _, ok := Bound(dir, "../p1"); ok {
		t.Error("a path was taken for a prompt id")
	}
	evs, err := Read(dir)
	if err != nil || len(evs) != 6 {
		t.Fatalf("Read: %d events, %v", len(evs), err)
	}
	want := []Event{
		{Hook: HookSessionStart, SessionID: "s", Source: "startup"},
		{Hook: HookUserPrompt, PromptID: "p-notify", Prompt: "<task-notification>x"},
		{Hook: HookUserPrompt, PromptID: "p1", Prompt: "PING 1", Native: "n1"},
		{Hook: HookUserPrompt, PromptID: "p1", Prompt: "queued", Queued: true},
		{Hook: HookStop, PromptID: "p1", Message: "PONG 1", Listed: true, Background: []Task{{ID: "b1", Type: "shell", Description: "sleep"}}},
		{Hook: HookStopFailure, PromptID: "p1", Error: "server_error"},
	}
	for i, ev := range evs {
		w := want[i]
		w.At, w.Name = ev.At, ev.Name
		if !reflect.DeepEqual(ev, w) {
			t.Errorf("event %d = %+v, want %+v", i, ev, w)
		}
		Remove(dir, ev)
	}
	if evs, _ := Read(dir); len(evs) != 0 {
		t.Errorf("%d events left after Remove", len(evs))
	}
	// A new launch keeps the bindings, and no pending input.
	if err := SetPending(dir, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p2", "prompt": "PING 2"})
	if _, ok := Bound(dir, "p2"); ok {
		t.Error("a pending input outlived Prepare")
	}
	if _, ok := Bound(dir, "p1"); !ok {
		t.Error("Prepare dropped a binding")
	}
}

// Outside a TUI Session the hook does nothing.
func TestInert(t *testing.T) {
	if err := HandleHook("stop", nil, []byte("not json")); err != nil {
		t.Errorf("HandleHook without %s: %v", EnvDir, err)
	}
}

// A prompt of claude's own is recorded as claude's, bound to no input: a
// prompt queued behind its turn reports its prompt id, and is not bound
// either, though an input is pending. So is one claude submits with no input
// pending.
func TestOwnPrompts(t *testing.T) {
	dir := t.TempDir()
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p-auto", "prompt": "continue"})
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p-notify", "prompt": "\n<task-notification>\n<task-id>b1</task-id>"})
	if err := SetPending(dir, "n1"); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p-notify", "prompt": "PING 1"})
	evs, _ := Read(dir)
	if len(evs) != 3 || evs[0].Native != "" || evs[0].Queued || evs[1].Native != "" || evs[1].Queued || !evs[2].Queued || evs[2].Native != "" {
		t.Fatalf("events %+v", evs)
	}
	for _, id := range []string{"p-auto", "p-notify"} {
		if n, ok := Bound(dir, id); ok {
			t.Errorf("Bound(%s) = %q", id, n)
		}
	}
	hook(t, dir, "user-prompt-submit", map[string]any{"hook_event_name": HookUserPrompt, "prompt_id": "p1", "prompt": "PING 1"})
	if n, ok := Bound(dir, "p1"); !ok || n != "n1" {
		t.Errorf("Bound(p1) = %q %v: the input still pending was not bound", n, ok)
	}
}

func TestTaskNotification(t *testing.T) {
	for _, c := range []struct {
		prompt string
		ok     bool
		id     string
	}{
		{"<task-notification>\n<task-id>bdq37quon</task-id>\n<status>completed</status>", true, "bdq37quon"},
		{"  <task-notification><summary>x</summary>", true, ""},
		{"<task-notification><task-id>open", true, ""},
		{"PING <task-notification>", false, ""},
	} {
		if ok, id := TaskNotification(c.prompt); ok != c.ok || id != c.id {
			t.Errorf("TaskNotification(%q) = %v %q, want %v %q", c.prompt, ok, id, c.ok, c.id)
		}
	}
}
