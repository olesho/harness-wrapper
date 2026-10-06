package pi

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// The session files in testdata/1.0.4 are pi 1.0.4's own, written by
// probes/pirpc against the mock API (HW_PIRPC_KEEP), their temporary paths
// made neutral.

// followed reads a whole session file with FollowSession: its events, each
// with its line's entry.
func followed(t *testing.T, name string) []transcript.FollowedEvent {
	t.Helper()
	path := filepath.Join("testdata", "1.0.4", name+".jsonl")
	f, err := FollowSession(path, "probe", transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Errors) > 0 {
		t.Fatalf("%s: lines the decoder could not read: %v", name, b.Errors)
	}
	again, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != len(b.Events) {
		t.Fatalf("%s: a second Poll before Ack read %d events, the first %d", name, len(again.Events), len(b.Events))
	}
	seen := map[string]bool{}
	for i, ev := range b.Events {
		if ev.Event.NativeID != again.Events[i].Event.NativeID {
			t.Errorf("%s: event %d's identity moved between polls: %s, %s", name, i, ev.Event.NativeID, again.Events[i].Event.NativeID)
		}
		if seen[ev.Event.NativeID] {
			t.Errorf("%s: identity %s given twice", name, ev.Event.NativeID)
		}
		seen[ev.Event.NativeID] = true
		if _, ok := ev.Meta.(*Entry); !ok {
			t.Fatalf("%s: event %d has no entry: %+v", name, i, ev)
		}
	}
	return b.Events
}

func entryOf(ev transcript.FollowedEvent) *Entry { return ev.Meta.(*Entry) }

// byRole is the events whose entry has role, in order.
func byRole(evs []transcript.FollowedEvent, role string) []transcript.FollowedEvent {
	var out []transcript.FollowedEvent
	for _, ev := range evs {
		if entryOf(ev).Role == role {
			out = append(out, ev)
		}
	}
	return out
}

// A run: the input's tag, pi's system message, the user message and the
// answer, joined by parentId — the tag is the user message's grandparent
// through the system message — on both APIs.
func TestFollowARun(t *testing.T) {
	for _, name := range []string{"run-anthropic", "run-openai"} {
		evs := followed(t, name)
		tags, system, user, assistant := byRole(evs, "custom"), byRole(evs, "system"), byRole(evs, "user"), byRole(evs, "assistant")
		if len(tags) != 1 || len(system) != 1 || len(user) != 1 || len(assistant) != 1 {
			t.Fatalf("%s: tags %d, system %d, user %d, assistant %d", name, len(tags), len(system), len(user), len(assistant))
		}
		tag, sys, u, a := entryOf(tags[0]), entryOf(system[0]), entryOf(user[0]), entryOf(assistant[0])
		if tag.Tag != "in-1" || sys.ParentID != tag.ID || u.ParentID != sys.ID || a.ParentID != u.ID {
			t.Errorf("%s: tag %+v, system %+v, user %+v, assistant %+v", name, tag, sys, u, a)
		}
		if user[0].Event.Text != "PING 1" || assistant[0].Event.Text != "PONG 1" || a.StopReason != "stop" {
			t.Errorf("%s: user %q, answer %q (%s)", name, user[0].Event.Text, assistant[0].Event.Text, a.StopReason)
		}
		if tags[0].Event.Type != EventEntry || user[0].Event.UUID != u.ID {
			t.Errorf("%s: a tag's event %q, the user message's UUID %q", name, tags[0].Event.Type, user[0].Event.UUID)
		}
	}
}

// A tool round: the assistant's tool call, its result by call id, and the
// answer after it.
func TestFollowATool(t *testing.T) {
	evs := followed(t, "tool")
	var call, result, answer *transcript.FollowedEvent
	for i := range evs {
		ev := &evs[i]
		switch ev.Event.Type {
		case transcript.EventToolUse:
			call = ev
		case transcript.EventToolResult:
			result = ev
		case transcript.EventText:
			if ev.Event.Role == transcript.RoleAssistant {
				answer = ev
			}
		}
	}
	if call == nil || result == nil || answer == nil {
		t.Fatalf("call %v, result %v, answer %v", call, result, answer)
	}
	if call.Event.ToolName != "bash" || string(call.Event.ToolInput) != `{"command":"echo minimal"}` || entryOf(*call).StopReason != "toolUse" {
		t.Errorf("tool call %+v (%+v)", call.Event, entryOf(*call))
	}
	if result.Event.ToolUseID != call.Event.ToolUseID || entryOf(*result).ToolCallID != call.Event.ToolUseID || result.Event.Output != "minimal\n" || entryOf(*result).IsError {
		t.Errorf("tool result %+v (%+v)", result.Event, entryOf(*result))
	}
	if answer.Event.Text != "TOOL DONE: minimal" || entryOf(*answer).StopReason != "stop" {
		t.Errorf("answer %+v", answer.Event)
	}
}

// How runs end: the last assistant entry's stop reason and error, whether
// or not it has content; a retried attempt is an assistant error that a
// context_edit drops.
func TestFollowEnds(t *testing.T) {
	last := func(evs []transcript.FollowedEvent) *Entry {
		a := byRole(evs, "assistant")
		if len(a) == 0 {
			return nil
		}
		return entryOf(a[len(a)-1])
	}
	for _, c := range []struct{ name, stop, err string }{
		{"abort-mid-stream", "aborted", "The operation was aborted."},
		{"abort-before-first-token", "aborted", "The operation was aborted."},
		{"abort-mid-tool", "error", "The operation was aborted."},
		{"crash-mid-tool", "toolUse", ""},
		{"sigterm-mid-tool", "toolUse", ""},
	} {
		e := last(followed(t, c.name))
		if e == nil || e.StopReason != c.stop || e.ErrorMessage != c.err {
			t.Errorf("%s: last assistant %+v, want %s %q", c.name, e, c.stop, c.err)
		}
	}
	evs := followed(t, "abort-mid-tool")
	results := byRole(evs, "toolResult")
	if len(results) != 1 || !entryOf(results[0]).IsError || results[0].Event.Output != "Command aborted" {
		t.Errorf("abort-mid-tool's result: %+v", results)
	}

	evs = followed(t, "error-exhausted")
	assistant, edits := byRole(evs, "assistant"), map[string]bool{}
	for _, ev := range evs {
		if e := entryOf(ev); e.Type == "context_edit" {
			edits[e.TargetID] = true
		}
	}
	if len(assistant) != 3 || len(edits) != 2 || !edits[entryOf(assistant[0]).ID] || !edits[entryOf(assistant[1]).ID] || edits[entryOf(assistant[2]).ID] {
		t.Errorf("error-exhausted: %d attempts, edits %v: want three, the first two dropped", len(assistant), edits)
	}
	if e := entryOf(assistant[2]); e.StopReason != "error" || e.ErrorMessage == "" {
		t.Errorf("error-exhausted's last attempt: %+v", e)
	}
}

// A refused input's tag is in the session with no user message of its own:
// the user message after it belongs to no tag.
func TestFollowARefusedTag(t *testing.T) {
	evs := followed(t, "refused-while-busy")
	parents := map[string]string{}
	var tags []*Entry
	for _, ev := range evs {
		e := entryOf(ev)
		parents[e.ID] = e.ParentID
		if e.Tag != "" {
			tags = append(tags, e)
		}
	}
	if len(tags) != 2 {
		t.Fatalf("tags: %+v", tags)
	}
	for _, u := range byRole(evs, "user") {
		e := entryOf(u)
		p := parents[e.ID]
		if p == tags[1].ID || parents[p] == tags[1].ID {
			t.Errorf("the refused input's tag %s is the user message %q's ancestor", tags[1].Tag, u.Event.Text)
		}
	}
}

// SessionFile finds a session's file in a --session-dir by its id and
// header, and says fs.ErrNotExist until pi has written it.
func TestSessionFile(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("testdata", "1.0.4", "run-anthropic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "2026-10-06T09-53-48-065Z_probe-testarun-anthropic.jsonl")
	if err := os.WriteFile(want, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := SessionFile(dir, "probe-testarun-anthropic"); err != nil || got != want {
		t.Errorf("SessionFile: %q, %v; want %q", got, err, want)
	}
	if _, err := SessionFile(dir, "probe-testarun"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a prefix of the id: %v, want fs.ErrNotExist (the header confirms)", err)
	}
	if _, err := SessionFile(dir, "../x"); err == nil {
		t.Error("a path as a session id was taken")
	}
}
