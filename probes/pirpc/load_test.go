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

// What a load of a saved session needs of pi: one agent runs a session and
// then loses everything but the session's file, which is copied into a fresh
// agent — another agent dir, home and workspace — under the same name.

// saved is that session: two turns, PING 1 and TOOL pwd, in a workspace whose
// AGENTS.md says WORK-A. It returns the file's bytes, its name, and the
// source workspace, which is gone.
func saved(t *testing.T, mock *mockapi.Server, model string) (content []byte, name, work string) {
	t.Helper()
	a := newAgent(t, mock, model)
	writeFile(t, filepath.Join(a.work, "AGENTS.md"), "Project marker: WORK-A\n")
	p := a.start()
	p.run("in-1", "PING 1", 60*time.Second)
	p.run("in-2", "TOOL pwd", 60*time.Second)
	if !p.closeStdin() {
		t.Fatal("pi did not exit when its stdin closed")
	}
	file := a.sessionFile()
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	work, _ = filepath.EvalSymlinks(a.work)
	if err := os.RemoveAll(filepath.Dir(a.dir)); err != nil {
		t.Fatal(err)
	}
	return content, filepath.Base(file), work
}

// loaded is the fresh agent, in a workspace whose AGENTS.md says WORK-B, with
// the saved file in its session dir, and nothing else of the source's.
// rewrite names the new workspace in the file's header instead of the
// source's.
func loaded(t *testing.T, mock *mockapi.Server, model, id string, content []byte, name, srcWork string, rewrite bool) *agent {
	t.Helper()
	b := newAgent(t, mock, model)
	b.id = id
	writeFile(t, filepath.Join(b.work, "AGENTS.md"), "Project marker: WORK-B\n")
	if rewrite {
		dst, _ := filepath.EvalSymlinks(b.work)
		content = []byte(strings.Replace(string(content), `"cwd":"`+srcWork+`"`, `"cwd":"`+dst+`"`, 1))
	}
	writeFile(t, filepath.Join(b.sessions, name), string(content))
	return b
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// toolCwd is where the bash tool call since from ran: its output.
func toolCwd(p *proc, from int) string {
	for _, r := range p.since(from) {
		var e struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if r.Type == "tool_execution_end" && json.Unmarshal(r.Raw, &e) == nil && len(e.Result.Content) > 0 {
			return strings.TrimSpace(e.Result.Content[0].Text)
		}
	}
	return ""
}

// systemCwds are the cwd sections of the session's system messages, in
// order.
func systemCwds(es []entry) []string {
	var out []string
	for _, e := range es {
		var m struct {
			Message struct {
				Role     string `json:"role"`
				Sections struct {
					Cwd string `json:"cwd"`
				} `json:"sections"`
			} `json:"message"`
		}
		if json.Unmarshal(e.Raw, &m) == nil && m.Message.Role == "system" && m.Message.Sections.Cwd != "" {
			out = append(out, strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(m.Message.Sections.Cwd), "<cwd>"), "</cwd>")))
		}
	}
	return out
}

// carries reports whether the last request carried the saved turns.
func carries(mock *mockapi.Server) bool {
	reqs := mock.Requests()
	body := string(reqs[len(reqs)-1].Body)
	return strings.Contains(body, "PING 1") && strings.Contains(body, "PONG 1")
}

// --session-id finds a session only among those of pi's working directory,
// by the header's cwd: a copy whose header names another workspace is not
// found, and pi silently starts a new, empty session under the same id, in a
// file of its own, with a warning on stderr alone.
func TestLoadCopyNotFound(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	mock.KeepBodies = true
	id := sessionID(t.Name())
	content, name, srcWork := saved(t, mock, anthropicModel)
	b := loaded(t, mock, anthropicModel, id, content, name, srcWork, false)
	p := b.start()
	st := state(t, p)
	p.run("in-3", "PING 2", 60*time.Second)
	p.closeStdin()
	t.Logf("a plain copy: %+v; stderr %q", st, p.stderrText())
	if st.SessionID != id || st.MessageCount != 0 || st.SessionFile == filepath.Join(b.sessions, name) || carries(mock) {
		t.Errorf("a plain copy opened: %+v", st)
	}
	if !strings.Contains(p.stderrText(), "No project session found with id") {
		t.Errorf("stderr %q", p.stderrText())
	}
	if files := b.sessionFiles(); len(files) != 2 {
		t.Errorf("session files: %v, want the copy and a new one", files)
	}
}

// With the header's cwd rewritten to the new workspace, the copy opens under
// its id: the same file goes on, the next request carries the saved turns,
// tools run in the new workspace, and pi appends a system message naming it
// and its AGENTS.md (the source's stays in the history).
func TestLoadHeaderRewritten(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	mock.KeepBodies = true
	id := sessionID(t.Name())
	content, name, srcWork := saved(t, mock, anthropicModel)
	b := loaded(t, mock, anthropicModel, id, content, name, srcWork, true)
	dstWork, _ := filepath.EvalSymlinks(b.work)
	copied := filepath.Join(b.sessions, name)
	p := b.start()
	st := state(t, p)
	if st.SessionID != id || st.SessionFile != copied || st.MessageCount == 0 {
		t.Errorf("loaded: %+v, want session %s in %s with its messages", st, id, copied)
	}
	p.run("in-3", "PING 2", 60*time.Second)
	reqs := mock.Requests()
	if last := reqs[len(reqs)-1]; !carries(mock) || !strings.Contains(last.System, "WORK-B") || strings.Contains(last.System, "WORK-A") {
		t.Errorf("the request after the load: carries %v, system %s", carries(mock), short([]byte(last.System), 400))
	}
	from := p.run("in-4", "TOOL pwd", 60*time.Second)
	if got := toolCwd(p, from); got != dstWork {
		t.Errorf("a tool ran in %q, want %s", got, dstWork)
	}
	p.closeStdin()
	_, es := entries(t, copied)
	t.Logf("the session after the load:\n%s", describe(es))
	if files := b.sessionFiles(); len(files) != 1 {
		t.Errorf("session files: %v, want the copy alone", files)
	}
	if cwds := systemCwds(es); len(cwds) != 2 || cwds[0] != srcWork || cwds[1] != dstWork {
		t.Errorf("system messages' cwd %q, want the source's, then %s", cwds, dstWork)
	}
}

// A workspace at the source's very path needs no rewrite: the copy opens
// under its id.
func TestLoadSameWorkspacePath(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	mock.KeepBodies = true
	id := sessionID(t.Name())
	content, name, srcWork := saved(t, mock, anthropicModel)
	b := loaded(t, mock, anthropicModel, id, content, name, srcWork, false)
	if err := os.MkdirAll(srcWork, 0o700); err != nil {
		t.Fatal(err)
	}
	b.work = srcWork
	p := b.start()
	st := state(t, p)
	p.run("in-3", "PING 2", 60*time.Second)
	p.closeStdin()
	if st.SessionFile != filepath.Join(b.sessions, name) || !carries(mock) {
		t.Errorf("in the source's workspace path: %+v, carries %v", st, carries(mock))
	}
}

// --session PATH opens a session by its file, and refuses one whose header's
// cwd does not exist: pi says so on stderr and answers no command.
func TestLoadByPathRefused(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	id := sessionID(t.Name())
	content, name, srcWork := saved(t, mock, anthropicModel)
	b := loaded(t, mock, anthropicModel, id, content, name, srcWork, false)
	b.path = filepath.Join(b.sessions, name)
	p := b.start()
	if r, ok := p.await(p.send(map[string]any{"type": "get_state"}), 10*time.Second); ok {
		t.Errorf("get_state answered %+v", r)
	}
	if !strings.Contains(p.stderrText(), "Stored session working directory does not exist") {
		t.Errorf("stderr %q", p.stderrText())
	}
}

// A session saved under one provider's model goes on under another's, the
// header rewritten: the request, on the other API, carries the saved turns.
func TestLoadAnotherProvider(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	mock.KeepBodies = true
	id := sessionID(t.Name())
	content, name, srcWork := saved(t, mock, anthropicModel)
	b := loaded(t, mock, openaiModel, id, content, name, srcWork, true)
	p := b.start()
	st := state(t, p)
	from := p.run("in-3", "PING 2", 60*time.Second)
	reqs := mock.Requests()
	last := reqs[len(reqs)-1]
	if st.MessageCount == 0 || last.Model != "gpt-4.1-mini" || !strings.Contains(last.Input, "PING 1") || !strings.Contains(last.Input, "PONG 1") {
		t.Errorf("under %s: %+v; the request: model %q, input %s", openaiModel, st, last.Model, short([]byte(last.Input), 600))
	}
	if got := lastAssistant(p.since(from)).text(); got != "PONG 2" {
		t.Errorf("the answer under %s: %q", openaiModel, got)
	}
	p.closeStdin()
	_, es := entries(t, filepath.Join(b.sessions, name))
	t.Logf("the session after the load:\n%s", describe(es))
}
