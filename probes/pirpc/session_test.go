package pirpc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
)

// A prompt's run, on each API: the prompt is answered "started", the run ends
// in agent_settled, and the session file holds the tag, the user message and
// the assistant's answer with stopReason "stop", in that order; between the
// tag and the user message only pi's system message, which it writes when the
// system prompt changed. The model saw the input without its tag, and the key
// from auth.json in the provider's header.
func TestARun(t *testing.T) {
	for _, c := range []struct {
		name, model, auth string
	}{
		{"anthropic", anthropicModel, anthropicKey},
		{"openai", openaiModel, "Bearer " + openaiKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock := mockapi.Start()
			defer mock.Close()
			a := newAgent(t, mock, c.model)
			p := a.start()
			from := p.run("in-1", "PING 1", 60*time.Second)
			if got := lastAssistant(p.since(from)); got == nil || got.text() != "PONG 1" {
				t.Errorf("answer %+v, want PONG 1", got)
			}

			_, es := entries(t, a.sessionFile())
			t.Logf("session:\n%s", describe(es))
			tag, user, assistant := -1, -1, -1
			for i, e := range es {
				switch {
				case e.Type == "custom" && e.CustomType == "hw.input":
					tag = i
				case e.Message != nil && e.Message.Role == "user":
					user = i
				case e.Message != nil && e.Message.Role == "assistant":
					assistant = i
				}
			}
			if tag < 0 || user < tag || assistant < user || es[assistant].Message.StopReason != "stop" {
				t.Fatalf("tag %d, user %d, assistant %d: want the tag, then the user message, then a stop", tag, user, assistant)
			}
			for _, e := range es[tag+1 : user] {
				if e.Message == nil || e.Message.Role != "system" {
					t.Errorf("between the tag and the user message: %s", e.Raw)
				}
			}
			if es[user].Message.text() != "PING 1" {
				t.Errorf("user message %q, want the input without its tag", es[user].Message.text())
			}
			reqs := mock.Requests()
			if len(reqs) == 0 || reqs[len(reqs)-1].Scenario != "PING 1" || reqs[len(reqs)-1].Auth != c.auth {
				t.Errorf("the model saw %+v", reqs)
			}
		})
	}
}

// A prompt sent while a run is busy is refused: success false, and it never
// runs. pi runs input hooks before it checks, so the refused input's tag is in
// the session, with no user message of its own: a tag proves nothing until
// its user message follows. A prompt for a provider pi has no key for is
// refused too, before pi writes anything.
func TestReceipt(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	a := newAgent(t, mock, anthropicModel)
	p := a.start()
	from := p.mark()
	if r, ok := p.await(p.prompt("in-1", "SLOW 30"), 30*time.Second); !ok || !r.Success {
		t.Fatalf("SLOW: %+v", r)
	}
	p.mustWait("message_update", from, 30*time.Second)
	busy, ok := p.await(p.prompt("in-2", "PING 2"), 30*time.Second)
	if !ok || busy.Success {
		t.Fatalf("a prompt while busy: %+v (answered %v), want a refusal", busy, ok)
	}
	t.Logf("refused while busy: %q", busy.Error)
	p.mustWait("agent_settled", from, 60*time.Second)
	_, es := entries(t, a.sessionFile())
	t.Logf("session:\n%s", describe(es))
	tagged := false
	for _, e := range es {
		if e.Type == "custom" && strings.Contains(string(e.Data), `"in-2"`) {
			tagged = true
		}
		if e.Message != nil && e.Message.text() == "PING 2" {
			t.Errorf("the refused input has a user message: %s", e.Raw)
		}
	}
	if !tagged {
		t.Error("want the refused input's tag in the session: its input hook ran")
	}
	for _, r := range mock.Requests() {
		if r.Scenario == "PING 2" {
			t.Error("the refused input reached the model")
		}
	}

	b := newAgent(t, mock, anthropicModel)
	b.writeJSON("auth.json", map[string]any{})
	q := b.start()
	r, ok := q.await(q.prompt("in-3", "PING 3"), 15*time.Second)
	t.Logf("no key: answered %v %+v", ok, r)
	if ok && r.Success {
		t.Errorf("a prompt with no key was accepted: %+v", r)
	}
	var files []string
	_ = filepath.WalkDir(b.sessions, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 0 {
		t.Errorf("session files after a refused prompt: %v", files)
	}
}

// --session-id reopens the session it names, after pi exits on stdin's end:
// the same file, the conversation continued. If the file is gone, the same
// flag silently starts a new session under that id.
func TestReopen(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	mock.KeepBodies = true
	a := newAgent(t, mock, anthropicModel)
	p := a.start()
	p.run("in-1", "PING 1", 60*time.Second)
	file := a.sessionFile()
	st := state(t, p)
	if st.SessionID != a.id || st.SessionFile != file {
		t.Errorf("state %+v, want session %s in %s", st, a.id, file)
	}
	if !p.closeStdin() {
		t.Fatal("pi did not exit when its stdin closed")
	}
	t.Logf("exit on stdin's end: %v", p.cmd.ProcessState)

	p = a.start()
	st = state(t, p)
	if st.SessionID != a.id || st.SessionFile != file {
		t.Errorf("reopened: %+v, want session %s in %s", st, a.id, file)
	}
	p.run("in-2", "PING 2", 60*time.Second)
	reqs := mock.Requests()
	if last := string(reqs[len(reqs)-1].Body); !strings.Contains(last, "PING 1") || !strings.Contains(last, "PONG 1") {
		t.Errorf("the reopened session's request does not carry the first turn: %s", short([]byte(last), 400))
	}
	_, es := entries(t, file)
	t.Logf("session:\n%s", describe(es))
	p.closeStdin()

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	p = a.start()
	st = state(t, p)
	t.Logf("reopened with its file gone: %+v; stderr %q", st, p.stderrText())
	if st.SessionID != a.id || st.MessageCount != 0 {
		t.Errorf("with its file gone: %+v, want a new, empty session %s", st, a.id)
	}
}

type sessionState struct {
	SessionID    string `json:"sessionId"`
	SessionFile  string `json:"sessionFile"`
	MessageCount int    `json:"messageCount"`
	IsStreaming  bool   `json:"isStreaming"`
	Model        struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	} `json:"model"`
}

func state(t *testing.T, p *proc) sessionState {
	t.Helper()
	r := p.call(map[string]any{"type": "get_state"})
	var s sessionState
	if !r.Success || json.Unmarshal(r.Data, &s) != nil {
		t.Fatalf("get_state: %+v", r)
	}
	return s
}

// The tag extension shows itself in get_commands. A tagged input that starts
// with "/" reaches the model as it is unless it names a command or template;
// a tag on an input without the extension reaches the model.
func TestTags(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	a := newAgent(t, mock, anthropicModel)
	if err := os.MkdirAll(filepath.Join(a.dir, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.dir, "prompts", "greet.md"), []byte("PING 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := a.start()
	cmds := commands(t, p)
	t.Logf("commands: %v", cmds)
	if cmds["hw-tag"] != "extension" {
		t.Errorf("get_commands: no hw-tag extension command in %v", cmds)
	}

	for _, c := range []struct{ tag, text string }{
		{"in-1", "/nothing here"},
		{"in-2", "/greet"},
		{"in-4", "/hw-tag"},
	} {
		from := p.run(c.tag, c.text, 60*time.Second)
		_, es := entries(t, a.sessionFile())
		user := lastUser(es)
		t.Logf("%q after a tag: the user message is %q, the model answered %q", c.text, user, lastAssistant(p.since(from)).text())
	}
	r := p.call(map[string]any{"type": "prompt", "message": "/hw-tag"})
	t.Logf("/hw-tag untagged: %+v", r)

	b := newAgent(t, mock, anthropicModel)
	b.ext = nil
	q := b.start()
	if cmds := commands(t, q); cmds["hw-tag"] != "" {
		t.Errorf("hw-tag without the extension: %v", cmds)
	}
	q.run("in-3", "PING 3", 60*time.Second)
	_, es := entries(t, b.sessionFile())
	if u := lastUser(es); !strings.HasPrefix(u, "<!--hw:in-3-->") {
		t.Errorf("without the extension the user message is %q, want the tag left in it", u)
	}
}

func commands(t *testing.T, p *proc) map[string]string {
	t.Helper()
	r := p.call(map[string]any{"type": "get_commands"})
	var d struct {
		Commands []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"commands"`
	}
	if !r.Success || json.Unmarshal(r.Data, &d) != nil {
		t.Fatalf("get_commands: %+v", r)
	}
	out := map[string]string{}
	for _, c := range d.Commands {
		out[c.Name] = c.Source
	}
	return out
}

func lastUser(es []entry) string {
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Message != nil && es[i].Message.Role == "user" {
			return es[i].Message.text()
		}
	}
	return ""
}

// lastAssistant is the last assistant message among records' message_end
// events.
func lastAssistant(rs []record) *message {
	var last *message
	for _, r := range rs {
		if r.Type != "message_end" {
			continue
		}
		var ev struct {
			Message message `json:"message"`
		}
		if json.Unmarshal(r.Raw, &ev) == nil && ev.Message.Role == "assistant" {
			m := ev.Message
			last = &m
		}
	}
	return last
}
