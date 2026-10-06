package pirpc

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
)

// count is how many records of typ rs hold.
func count(rs []record, typ string) int {
	n := 0
	for _, r := range rs {
		if r.Type == typ {
			n++
		}
	}
	return n
}

// firstText waits for the run's first text delta, from a mark on.
func (p *proc) firstText(from int, d time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r, i, ok := p.waitFor("message_update", from, time.Until(deadline))
		if !ok {
			break
		}
		if strings.Contains(string(r.Raw), `"text_delta"`) {
			return
		}
		from = i + 1
	}
	p.t.Fatalf("no text delta in %s", d)
}

// alive reports whether a process whose command line holds marker runs.
func alive(marker string) bool {
	return exec.Command("pgrep", "-f", marker).Run() == nil
}

// How an input's run ends, as the session's entries record it: an exhausted
// API error ends with an assistant message whose stopReason is "error", an
// abort mid-stream or before the first token with "aborted", and an abort
// mid-tool with an errored tool result and then an assistant "error" — so
// the session alone cannot tell an interrupt mid-tool from a failure.
func TestOutcomes(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()

	t.Run("exhausted error", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.run("in-1", "ERR 529 99", 90*time.Second)
		rs := p.since(from)
		last := lastAssistant(rs)
		t.Logf("retries %d; last assistant: stop %q, error %q", count(rs, "auto_retry_start"), last.StopReason, last.ErrorMessage)
		if last.StopReason != "error" {
			t.Errorf("stopReason %q, want error", last.StopReason)
		}
		_, es := entries(t, a.sessionFile())
		t.Logf("session:\n%s", describe(es))
		if e := es[len(es)-1]; e.Message == nil || e.Message.StopReason != "error" {
			t.Errorf("the session ends with %s, want the failed assistant message", e.Raw)
		}
	})

	t.Run("abort mid-stream", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.mark()
		p.prompt("in-1", "SLOW 60")
		p.firstText(from, 30*time.Second)
		r := p.call(map[string]any{"type": "abort"})
		_, settled, ok := p.waitFor("agent_settled", from, 0)
		t.Logf("abort: %+v; agent_settled before its answer: %v (record %d)", r, ok, settled)
		last := lastAssistant(p.since(from))
		if last == nil || last.StopReason != "aborted" {
			t.Errorf("last assistant %+v, want aborted", last)
		}
		_, es := entries(t, a.sessionFile())
		t.Logf("session:\n%s", describe(es))
	})

	t.Run("abort before the first token", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.mark()
		p.prompt("in-1", "STALL 30")
		p.mustWait("message_start", from, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		r := p.call(map[string]any{"type": "abort"})
		t.Logf("abort: %+v", r)
		last := lastAssistant(p.since(from))
		if last == nil || last.StopReason != "aborted" {
			t.Errorf("last assistant %+v, want aborted", last)
		}
		_, es := entries(t, a.sessionFile())
		t.Logf("session:\n%s", describe(es))
	})

	t.Run("abort mid-tool", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.mark()
		p.prompt("in-1", "TOOL sleep 31.7")
		p.mustWait("tool_execution_start", from, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		if !alive("sleep 31.7") {
			t.Fatal("the tool's command is not running")
		}
		r := p.call(map[string]any{"type": "abort"})
		t.Logf("abort: %+v", r)
		rs := p.since(from)
		last := lastAssistant(rs)
		t.Logf("last assistant: stop %q, error %q", last.StopReason, last.ErrorMessage)
		if alive("sleep 31.7") {
			t.Error("the tool's command outlived the abort")
		}
		_, es := entries(t, a.sessionFile())
		t.Logf("session:\n%s", describe(es))
		var result *message
		for _, e := range es {
			if e.Message != nil && e.Message.Role == "toolResult" {
				result = e.Message
			}
		}
		if result == nil || !result.IsError {
			t.Errorf("tool result %+v, want an error", result)
		}
		if e := es[len(es)-1]; e.Message == nil || e.Message.Role != "assistant" || e.Message.StopReason != "error" {
			t.Errorf("the session ends with %s, want an assistant error after the aborted tool", e.Raw)
		}
	})
}

// A retried error: auto_retry_start, then the attempt that succeeds; the
// session keeps the failed attempt and a context_edit that drops it.
func TestRetry(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	a := newAgent(t, mock, anthropicModel)
	p := a.start()
	from := p.run("in-1", "ERR 529 1", 60*time.Second)
	rs := p.since(from)
	for _, r := range rs {
		if strings.HasPrefix(r.Type, "auto_retry") {
			t.Logf("%s", r.Raw)
		}
	}
	if count(rs, "auto_retry_start") != 1 || lastAssistant(rs).text() != "RECOVERED" {
		t.Errorf("retries %d, answer %q: want one retry, then RECOVERED", count(rs, "auto_retry_start"), lastAssistant(rs).text())
	}
	_, es := entries(t, a.sessionFile())
	t.Logf("session:\n%s", describe(es))
}

// Usage walls and other errors, on both APIs: what pi retries, and the text
// each ends with. The text is all a client has to tell them apart.
func TestErrorTexts(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	for _, c := range []struct{ name, model, input string }{
		{"anthropic usage wall", anthropicModel, "LIMIT"},
		{"anthropic 429", anthropicModel, "ERR 429 99"},
		{"anthropic 500", anthropicModel, "ERR 500 99"},
		{"openai usage wall", openaiModel, "LIMIT"},
		{"openai 529", openaiModel, "ERR 529 99"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAgent(t, mock, c.model)
			p := a.start()
			from := p.run("in-1", c.input, 120*time.Second)
			rs := p.since(from)
			last := lastAssistant(rs)
			var end string
			for _, r := range rs {
				if r.Type == "auto_retry_end" {
					end = string(r.Raw)
				}
			}
			t.Logf("%s: retries %d, stop %q, error %q; %s", c.input, count(rs, "auto_retry_start"), last.StopReason, last.ErrorMessage, end)
			if last.StopReason != "error" || last.ErrorMessage == "" {
				t.Errorf("last assistant %+v, want an error with its text", last)
			}
		})
	}
}

// A crash: what the session holds when pi is killed at each point of a run.
// Mid-stream it has the input's user message and no assistant message;
// mid-tool the assistant's tool call and no result, and the tool's command
// survives pi. Reopened, pi does not go on with the run.
func TestCrashes(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	mock.KeepBodies = true

	t.Run("mid-stream", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.mark()
		p.prompt("in-1", "SLOW 60")
		p.firstText(from, 30*time.Second)
		p.kill()
		_, es := entries(t, a.sessionFile())
		t.Logf("after the crash:\n%s", describe(es))
		if lastUser(es) != "SLOW 60" || es[len(es)-1].Message == nil || es[len(es)-1].Message.Role != "user" {
			t.Errorf("want the session to end with the input's user message")
		}

		p = a.start()
		st := state(t, p)
		time.Sleep(2 * time.Second)
		t.Logf("reopened: %+v; records %d", st, p.mark())
		if st.IsStreaming {
			t.Error("reopened, pi goes on with the run")
		}
		p.run("in-2", "PING 2", 60*time.Second)
		reqs := mock.Requests()
		body := string(reqs[len(reqs)-1].Body)
		t.Logf("the next input's request carries the crashed input: %v", strings.Contains(body, "SLOW 60"))
		_, es = entries(t, a.sessionFile())
		t.Logf("session:\n%s", describe(es))
	})

	t.Run("mid-tool", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.mark()
		p.prompt("in-1", "TOOL sleep 32.9")
		p.mustWait("tool_execution_start", from, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		p.kill()
		survived := alive("sleep 32.9")
		t.Logf("the tool's command survived the crash: %v", survived)
		_ = exec.Command("pkill", "-f", "sleep 32.9").Run()
		if !survived {
			t.Error("want the tool's command to outlive a kill -9 of pi")
		}
		_, es := entries(t, a.sessionFile())
		t.Logf("after the crash:\n%s", describe(es))
		last := es[len(es)-1]
		if last.Message == nil || last.Message.Role != "assistant" || last.Message.StopReason != "toolUse" {
			t.Errorf("the session ends with %s, want the assistant's tool call", last.Raw)
		}
	})

	t.Run("at once", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		p.prompt("in-1", "PING 1")
		time.Sleep(30 * time.Millisecond)
		p.kill()
		files := a.sessionFiles()
		t.Logf("killed 30 ms after the prompt: session files %v", files)
		for _, f := range files {
			_, es := entries(t, f)
			t.Logf("%s:\n%s", f, describe(es))
		}
	})
}

// Stopping pi: closing its stdin ends it at once, idle or not; SIGTERM ends
// it and the tool commands it runs.
func TestStop(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()

	t.Run("SIGTERM mid-tool", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.mark()
		p.prompt("in-1", "TOOL sleep 33.7")
		p.mustWait("tool_execution_start", from, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		if !p.term() {
			t.Fatal("pi did not exit on SIGTERM")
		}
		time.Sleep(300 * time.Millisecond)
		survived := alive("sleep 33.7")
		_ = exec.Command("pkill", "-f", "sleep 33.7").Run()
		t.Logf("exit %v; the tool's command survived: %v", p.cmd.ProcessState, survived)
		if survived {
			t.Error("the tool's command outlived SIGTERM")
		}
		_, es := entries(t, a.sessionFile())
		t.Logf("session:\n%s", describe(es))
	})

	t.Run("stdin mid-tool", func(t *testing.T) {
		a := newAgent(t, mock, anthropicModel)
		p := a.start()
		from := p.mark()
		p.prompt("in-1", "TOOL sleep 34.3")
		p.mustWait("tool_execution_start", from, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		exited := p.closeStdin()
		time.Sleep(300 * time.Millisecond)
		survived := alive("sleep 34.3")
		_ = exec.Command("pkill", "-f", "sleep 34.3").Run()
		t.Logf("exited %v (%v); the tool's command survived: %v", exited, p.cmd.ProcessState, survived)
		_, es := entries(t, a.sessionFile())
		t.Logf("session:\n%s", describe(es))
	})
}
