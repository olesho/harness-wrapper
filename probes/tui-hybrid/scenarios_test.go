package tuihybrid

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
)

// A foreground subagent (mockapi's AGENT): which hooks fire, with what ids,
// and where its transcript lands.
func TestSubagent(t *testing.T) {
	realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	for i := range runs() {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r := launch(t, mock, opts{bypass: true})
			r.typeLine("AGENT PING 7")
			if _, ok := r.await("Stop", 60*time.Second, nil); !ok {
				t.Errorf("no Stop")
			}
			time.Sleep(time.Second)
			t.Logf("hooks:\n%s", r.summary())
			t.Logf("debug:\n%s", strings.Join(r.debugLines(), "\n"))
			t.Logf("transcripts: %v", r.transcripts())
			start, okS := r.await("SubagentStart", 0, nil)
			stop, okE := r.await("SubagentStop", 0, nil)
			t.Logf("SubagentStart %v %v", okS, start.Payload)
			t.Logf("SubagentStop %v %v", okE, stop.Payload)
			if !okS || !okE {
				t.Errorf("SubagentStart %v, SubagentStop %v", okS, okE)
			}
		})
	}
}

// A background command (mockapi's BG): the hooks of the input's turn, of the
// command's end, and of the turn claude starts itself to take it up.
func TestBackground(t *testing.T) {
	realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	for i := range runs() {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r := launch(t, mock, opts{bypass: true})
			r.typeLine("BG sleep 2; echo bg-out")
			if _, ok := r.await("UserPromptSubmit", 60*time.Second, func(p map[string]any) bool {
				s, _ := p["prompt"].(string)
				return strings.HasPrefix(strings.TrimSpace(s), "<task-notification>")
			}); !ok {
				t.Errorf("no task-notification prompt")
			}
			deadline := time.Now().Add(30 * time.Second)
			for r.count("Stop") < 2 && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)
			t.Logf("hooks:\n%s", r.summary())
			t.Logf("debug:\n%s", strings.Join(r.debugLines(), "\n"))
			if n := r.count("Stop"); n != 2 {
				t.Errorf("%d Stop, want 2", n)
			}
		})
	}
}

// answer is the PermissionRequest hook's decision.
func answer(behavior, message string) string {
	d := fmt.Sprintf(`{"behavior":%q}`, behavior)
	if message != "" {
		d = fmt.Sprintf(`{"behavior":%q,"message":%q}`, behavior, message)
	}
	return `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":` + d + `}}`
}

// A tool that needs permission, in claude's default mode: whether
// PermissionRequest fires, whether its answer decides, how long it may block,
// and what claude does when it does not answer.
func TestPermission(t *testing.T) {
	realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	cases := []struct {
		name    string
		hold    time.Duration
		timeout int
		reply   string
		after   time.Duration // how long the probe waits before answering
		// esc presses Esc while the hook blocks, then answers after.
		esc  bool
		long bool // runs with HW_TUI_PROBE_LONG alone
	}{
		{name: "allow", hold: 2 * time.Minute, timeout: 600, reply: answer("allow", ""), after: time.Second},
		{name: "deny", hold: 2 * time.Minute, timeout: 600, reply: answer("deny", "probe says no"), after: time.Second},
		{name: "allow-after-45s", hold: 2 * time.Minute, timeout: 600, reply: answer("allow", ""), after: 45 * time.Second},
		{name: "hook-timeout", hold: 2 * time.Minute, timeout: 5},
		{name: "no-decision", hold: time.Second, timeout: 600},
		{name: "esc", hold: 2 * time.Minute, timeout: 600, reply: answer("allow", ""), after: 2 * time.Second, esc: true},
		{name: "allow-after-11m", hold: 15 * time.Minute, timeout: 86400, reply: answer("allow", ""), after: 11 * time.Minute, long: true},
	}
	for i := range runs() {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/%d", c.name, i), func(t *testing.T) {
				if c.long != (os.Getenv("HW_TUI_PROBE_LONG") != "") {
					t.Skip("long cases run with HW_TUI_PROBE_LONG alone")
				}
				r := launch(t, mock, opts{hookTimeout: c.timeout, hold: c.hold})
				m := r.mark()
				file := "probe-" + c.name
				sent := time.Now()
				r.typeLine("TOOL touch " + file)
				pr, ok := r.await("PermissionRequest", 30*time.Second, nil)
				if !ok {
					t.Logf("hooks:\n%s", r.summary())
					t.Fatalf("no PermissionRequest; screen:\n%s", tail(r.screenFrom(m), 2000))
				}
				t.Logf("PermissionRequest after %s: %v", time.Since(sent).Round(time.Millisecond), pr.Payload)
				if c.esc {
					time.Sleep(c.after)
					_, _ = r.tty.WriteString("\x1b")
					t.Logf("Esc pressed at %s", time.Since(sent).Round(time.Millisecond))
				}
				if c.reply != "" {
					time.Sleep(c.after)
					t.Logf("screen while the hook blocks:\n%s", tail(r.screenFrom(m), 1500))
					if err := os.WriteFile(filepath.Join(r.base, "answer.json"), []byte(c.reply), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				stop, ok := r.await("Stop", 90*time.Second, nil)
				if !ok {
					t.Logf("no Stop within 90s")
				} else {
					t.Logf("Stop after %s: %v", time.Since(sent).Round(time.Millisecond), stop.Payload["last_assistant_message"])
				}
				_, statErr := os.Stat(filepath.Join(r.cwd, file))
				t.Logf("file made: %v", statErr == nil)
				t.Logf("hooks:\n%s", r.summary())
				t.Logf("debug:\n%s", strings.Join(r.debugLines(), "\n"))
				t.Logf("screen:\n%s", tail(r.screenFrom(m), 2500))
			})
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
