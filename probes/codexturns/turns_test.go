package codexturns

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// line is one rollout line, as far as the probe reads it.
type line struct {
	Type, Kind, Role, Text string
	TurnID, ClientID       string
}

// rollout reads the thread's rollout: one line per entry.
func rollout(t *testing.T, l contract.Layout) []line {
	t.Helper()
	var out []line
	err := filepath.Walk(filepath.Join(l.Config, "sessions"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		f, err := os.Open(p) //nolint:gosec // the probe's own temporary directory
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var env struct {
				Type    string `json:"type"`
				Payload struct {
					Type     string `json:"type"`
					Role     string `json:"role"`
					TurnID   string `json:"turn_id"`
					ClientID string `json:"client_id"`
					Content  []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"payload"`
			}
			if json.Unmarshal(sc.Bytes(), &env) != nil {
				continue
			}
			ln := line{Type: env.Type, Kind: env.Payload.Type, Role: env.Payload.Role, TurnID: env.Payload.TurnID, ClientID: env.Payload.ClientID}
			if len(env.Payload.Content) > 0 {
				ln.Text = env.Payload.Content[0].Text
			}
			out = append(out, ln)
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatalf("reading the rollout: %v", err)
	}
	return out
}

// turn is the rollout's lines from a turn's task_started to the next.
func turn(lines []line, id string) []line {
	var out []line
	in := false
	for _, ln := range lines {
		if ln.Kind == "task_started" {
			in = ln.TurnID == id
		}
		if in {
			out = append(out, ln)
		}
	}
	return out
}

func has(lines []line, pred func(line) bool) bool {
	for _, ln := range lines {
		if pred(ln) {
			return true
		}
	}
	return false
}

const goalContext = `<codex_internal_context source="goal">`

// turnNote is what a turn/started or turn/completed says.
func turnNote(n note) (id, status string) {
	var p struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(n.params, &p)
	return p.Turn.ID, p.Turn.Status
}

// started waits for the next turn/started after from, and returns its turn.
func (s *server) started(t *testing.T, from int, what string) (string, int) {
	t.Helper()
	n, next, ok := s.await("turn/started", from, 20*time.Second)
	if !ok {
		t.Fatalf("no turn/started: %s", what)
	}
	id, _ := turnNote(n)
	return id, next
}

// completed waits for the turn id to complete, and returns how.
func (s *server) completed(t *testing.T, from int, id string) (string, int) {
	t.Helper()
	for {
		n, next, ok := s.await("turn/completed", from, 60*time.Second)
		if !ok {
			t.Fatalf("turn %s never completed", id)
		}
		if got, status := turnNote(n); got == id {
			return status, next
		}
		from = next
	}
}

// quiet reports whether no turn started for d after from.
func (s *server) quiet(from int, d time.Duration) bool {
	_, _, started := s.await("turn/started", from, d)
	return !started
}

// goalOf asks codex for the thread's goal: its objective and status, "" with
// none.
func (s *server) goalOf(id string) (objective, status string) {
	var r struct {
		Goal *struct {
			Objective string `json:"objective"`
			Status    string `json:"status"`
		} `json:"goal"`
	}
	_ = json.Unmarshal(s.call("thread/goal/get", map[string]any{"threadId": id}), &r)
	if r.Goal == nil {
		return "", ""
	}
	return r.Goal.Objective, r.Goal.Status
}

func newThread(s *server, l contract.Layout) string {
	return threadID(s.call("thread/start", map[string]any{"cwd": l.Workspace, "approvalPolicy": "never", "sandbox": "danger-full-access"}))
}

// input starts a turn on text and returns the turn id codex answers with.
func (s *server) input(thread, text, client string) string {
	var r struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(s.call("turn/start", map[string]any{
		"threadId": thread, "input": []map[string]string{{"type": "text", "text": text}}, "clientUserMessageId": client,
	}), &r)
	return r.Turn.ID
}

// ping has one turn on text, to its end.
func ping(t *testing.T, s *server, thread, text, client string) {
	t.Helper()
	m := s.mark()
	id := s.input(thread, text, client)
	if status, _ := s.completed(t, m, id); status != "completed" {
		t.Fatalf("the turn on %q ended %s", text, status)
	}
}

// userMessage reports whether codex announced a user message with the client
// id, and in which turn.
func (s *server) userMessage(client string) (turnID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notes {
		if n.method != "item/started" && n.method != "item/completed" {
			continue
		}
		var p struct {
			TurnID string `json:"turnId"`
			Item   struct {
				Type     string `json:"type"`
				ClientID string `json:"clientId"`
			} `json:"item"`
		}
		if json.Unmarshal(n.params, &p) == nil && p.Item.Type == "userMessage" && p.Item.ClientID == client {
			return p.TurnID, true
		}
	}
	return "", false
}

// restart stops codex by closing its stdin and starts another in the same
// roots, initialized.
func restart(t *testing.T, s *server, l contract.Layout) *server {
	t.Helper()
	_ = s.stdin.Close()
	_ = s.cmd.Wait()
	s2 := startServer(t, os.Getenv("HW_REAL_CODEX"), l.Workspace, append(adapter.HostEnv(), "HOME="+l.Home, "CODEX_HOME="+l.Config))
	t.Cleanup(func() { _ = s2.cmd.Process.Kill() })
	s2.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "probe", "title": "probe", "version": "0"}})
	_ = s2.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
	return s2
}

// An active goal makes codex start turns by itself: when the goal is set, and
// after each turn that completes, until the goal is complete. Such a turn has
// an id no turn/start answered with and no user message; its rollout turn
// opens with a message codex wrote to itself.
func TestGoalStartsTurns(t *testing.T) {
	_, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	m := s.mark()
	s.call("thread/goal/set", map[string]any{"threadId": id, "objective": "GOAL 1 2", "status": "active"})
	first, m := s.started(t, m, "setting an active goal starts a turn")
	status, m := s.completed(t, m, first)
	if status != "completed" {
		t.Fatalf("the goal's first turn ended %s", status)
	}
	second, m := s.started(t, m, "a goal turn that completes is followed by the next")
	if status, _ := s.completed(t, m, second); status != "completed" {
		t.Fatalf("the goal's second turn ended %s", status)
	}
	if _, status := s.goalOf(id); status != "complete" {
		t.Errorf("after the model's update_goal the goal is %q, want complete", status)
	}
	if !s.quiet(s.mark(), 2*time.Second) {
		t.Error("codex started a turn with its goal complete")
	}
	if first == second {
		t.Fatal("two goal turns share an id")
	}
	lines := rollout(t, l)
	for _, tid := range []string{first, second} {
		tl := turn(lines, tid)
		if !has(tl, func(ln line) bool {
			return ln.Kind == "message" && ln.Role == "user" && strings.HasPrefix(ln.Text, goalContext)
		}) {
			t.Errorf("goal turn %s does not open with codex's message to itself", tid)
		}
		if has(tl, func(ln line) bool { return ln.Kind == "user_message" }) {
			t.Errorf("goal turn %s holds a user message", tid)
		}
		if !has(tl, func(ln line) bool { return ln.Kind == "task_complete" && ln.TurnID == tid }) {
			t.Errorf("goal turn %s has no task_complete", tid)
		}
	}
	// A goal a client sets is logged in the rollout.
	if !has(lines, func(ln line) bool { return ln.Kind == "thread_goal_updated" }) {
		t.Error("the rollout does not log the goal a client set")
	}
}

// The model makes, and completes, a goal with its own tools, and codex says
// so as it happens — but the rollout logs neither, nor a goal's clearing: the
// rollout cannot tell what a thread's goal is.
func TestModelMakesGoal(t *testing.T) {
	_, s, _, l := setup(t)
	id := newThread(s, l)
	m := s.mark()
	made := s.input(id, "MKGOAL GOAL 1 2", "client-1")
	n, _, ok := s.await("thread/goal/updated", m, 20*time.Second)
	var up struct {
		TurnID string `json:"turnId"`
		Goal   struct {
			Objective string `json:"objective"`
			Status    string `json:"status"`
		} `json:"goal"`
	}
	_ = json.Unmarshal(n.params, &up)
	if !ok || up.TurnID != made || up.Goal.Objective != "GOAL 1 2" || up.Goal.Status != "active" {
		t.Fatalf("thread/goal/updated %s, want the input's turn %s and the goal active", n.params, made)
	}
	status, m := s.completed(t, m, made)
	if status != "completed" {
		t.Fatalf("the turn that made the goal ended %s", status)
	}
	first, m := s.started(t, m, "a goal the model made starts a turn once its turn completes")
	_, m = s.completed(t, m, first)
	second, m := s.started(t, m, "the goal's second turn")
	s.completed(t, m, second)
	if first == made || second == first {
		t.Fatalf("the goal's turns %s and %s, after the turn %s that made it", first, second, made)
	}
	if _, status := s.goalOf(id); status != "complete" {
		t.Errorf("the goal is %q, want complete", status)
	}
	m = s.mark()
	s.call("thread/goal/clear", map[string]any{"threadId": id})
	if _, _, ok := s.await("thread/goal/cleared", m, 5*time.Second); !ok {
		t.Error("no thread/goal/cleared")
	}
	if objective, _ := s.goalOf(id); objective != "" {
		t.Errorf("a cleared goal reads %q", objective)
	}
	if has(rollout(t, l), func(ln line) bool { return ln.Kind == "thread_goal_updated" }) {
		t.Error("the rollout logs a goal the model made or a client cleared: the adapter could read it there")
	}
}

// An interrupted goal turn ends interrupted and leaves the goal active, and
// codex starts no turn for the goal until a turn of an input completes.
func TestInterruptedGoalRests(t *testing.T) {
	mock, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	m := s.mark()
	s.call("thread/goal/set", map[string]any{"threadId": id, "objective": "GOAL 9 30", "status": "active"})
	own, m := s.started(t, m, "the goal's turn")
	time.Sleep(500 * time.Millisecond)
	s.call("turn/interrupt", map[string]any{"threadId": id, "turnId": own})
	if status, _ := s.completed(t, m, own); status != "interrupted" {
		t.Fatalf("the interrupted goal turn ended %s", status)
	}
	if _, status := s.goalOf(id); status != "active" {
		t.Errorf("after the interrupt the goal is %q, want active", status)
	}
	requests := len(mock.Requests())
	if !s.quiet(s.mark(), 3*time.Second) || len(mock.Requests()) != requests {
		t.Fatal("codex went on with its goal after the interrupt")
	}
	if !has(turn(rollout(t, l), own), func(ln line) bool { return ln.Kind == "turn_aborted" && ln.TurnID == own }) {
		t.Error("the interrupted goal turn has no turn_aborted in the rollout")
	}
	m = s.mark()
	ping(t, s, id, "PING two", "client-2")
	input, m := s.started(t, m, "the input's own turn")
	if input == own {
		t.Fatal("the input's turn has the goal turn's id")
	}
	// The input's turn over, codex takes the goal up again.
	if again, _ := s.started(t, m, "codex goes back to its goal after an input's turn completed"); again == input || again == own {
		t.Errorf("the turn after the input's is %s", again)
	}
	s.call("thread/goal/set", map[string]any{"threadId": id, "status": "paused"})
}

// turn/start during a running turn starts no turn: codex answers with a turn
// id that never starts, and folds the input into the running turn at its next
// model call. If that turn is interrupted first, the input is dropped.
func TestInputDuringGoalTurn(t *testing.T) {
	mock, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	m := s.mark()
	s.call("thread/goal/set", map[string]any{"threadId": id, "objective": "GOAL 9 10", "status": "active"})
	own, m := s.started(t, m, "the goal's turn")
	answered := s.input(id, "PING two", "client-2")
	if answered == "" || answered == own {
		t.Fatalf("turn/start during the goal turn answered turn %q; the goal turn is %s", answered, own)
	}
	if status, _ := s.completed(t, m, own); status != "completed" {
		t.Fatalf("the goal turn that took the input ended %s", status)
	}
	if in, ok := s.userMessage("client-2"); !ok || in != own {
		t.Errorf("the input's user message is in turn %q (announced: %v), want the running goal turn %s", in, ok, own)
	}
	tl := turn(rollout(t, l), own)
	if !has(tl, func(ln line) bool { return ln.Kind == "user_message" && ln.ClientID == "client-2" }) {
		t.Error("the goal turn's rollout does not hold the folded input")
	}
	if !has(tl, func(ln line) bool { return ln.Kind == "message" && ln.Role == "assistant" && ln.Text == "PONG two" }) {
		t.Error("the folded input was not answered in the goal turn")
	}
	s.mu.Lock()
	for _, n := range s.notes {
		if got, _ := turnNote(n); n.method == "turn/started" && got == answered {
			t.Errorf("the turn %s that turn/start answered with started", answered)
		}
	}
	s.mu.Unlock()

	// The next goal turn is interrupted while it holds an input.
	next, m := s.started(t, m, "the goal's next turn")
	dropped := s.input(id, "PING three", "client-3")
	s.call("turn/interrupt", map[string]any{"threadId": id, "turnId": next})
	if status, _ := s.completed(t, m, next); status != "interrupted" {
		t.Fatalf("the goal turn ended %s", status)
	}
	if !s.quiet(s.mark(), 3*time.Second) {
		t.Errorf("a turn started after the interrupt: codex did not drop the input (turn/start had answered %s)", dropped)
	}
	if _, ok := s.userMessage("client-3"); ok {
		t.Error("codex announced the dropped input's user message")
	}
	for _, r := range mock.Requests() {
		if r.Scenario == "PING three" {
			t.Error("the dropped input reached the model")
		}
	}
	if has(rollout(t, l), func(ln line) bool { return ln.ClientID == "client-3" }) {
		t.Error("the rollout holds the dropped input")
	}
	s.call("thread/goal/set", map[string]any{"threadId": id, "status": "paused"})
}

// Closing codex's stdin in the middle of a goal turn makes it abort the turn,
// say so in the rollout, and exit 0. What codex keeps of the thread beside
// its rollout — its name, its goal — it answers before the thread resumes,
// starting nothing; resumed, it goes back to an active goal. SIGTERM in the
// middle of a turn leaves the turn with no end.
func TestStopAndResumeOnAGoal(t *testing.T) {
	_, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	s.call("thread/name/set", map[string]any{"threadId": id, "name": "a named thread"})
	m := s.mark()
	s.call("thread/goal/set", map[string]any{"threadId": id, "objective": "GOAL 9 30", "status": "active"})
	own, _ := s.started(t, m, "the goal's turn")
	time.Sleep(500 * time.Millisecond)
	_ = s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("codex exited %v on stdin EOF, want 0", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("codex did not exit on stdin EOF in the middle of a goal turn")
	}
	if !has(turn(rollout(t, l), own), func(ln line) bool { return ln.Kind == "turn_aborted" && ln.TurnID == own }) {
		t.Error("the goal turn codex was on at stdin EOF has no turn_aborted")
	}

	s2 := startServer(t, os.Getenv("HW_REAL_CODEX"), l.Workspace, append(adapter.HostEnv(), "HOME="+l.Home, "CODEX_HOME="+l.Config))
	defer func() { _ = s2.cmd.Process.Kill() }()
	s2.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "probe", "title": "probe", "version": "0"}})
	_ = s2.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
	m = s2.mark()
	var read struct {
		Thread struct {
			Name *string `json:"name"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(s2.call("thread/read", map[string]any{"threadId": id}), &read)
	if read.Thread.Name == nil || *read.Thread.Name != "a named thread" {
		t.Errorf("thread/read before the thread resumes: name %v", read.Thread.Name)
	}
	if objective, status := s2.goalOf(id); objective != "GOAL 9 30" || status != "active" {
		t.Errorf("thread/goal/get before the thread resumes: %q %q", objective, status)
	}
	if !s2.quiet(m, time.Second) {
		t.Error("reading the thread's name and goal started a turn")
	}
	m = s2.mark()
	s2.call("thread/resume", map[string]any{"threadId": id, "cwd": l.Workspace, "approvalPolicy": "never", "sandbox": "danger-full-access"})
	resumed, _ := s2.started(t, m, "a resumed thread goes back to its active goal")
	if resumed == own {
		t.Error("the turn after the resume has the aborted turn's id")
	}
	time.Sleep(500 * time.Millisecond)
	_ = s2.cmd.Process.Signal(syscall.SIGTERM)
	_ = s2.cmd.Wait()
	if tl := turn(rollout(t, l), resumed); has(tl, func(ln line) bool { return ln.Kind == "turn_aborted" || ln.Kind == "task_complete" }) {
		t.Error("the turn codex was on at SIGTERM has an end in the rollout")
	}
}

// A goal turn that fails blocks the goal, and one that meets the account's
// usage wall marks it usageLimited: codex starts no further turn for either.
func TestFailedGoalTurnStopsTheGoal(t *testing.T) {
	for objective, want := range map[string]string{"ERR 529 99": "blocked", "LIMIT": "usageLimited"} {
		t.Run(objective, func(t *testing.T) {
			_, s, _, l := setup(t)
			id := newThread(s, l)
			ping(t, s, id, "PING one", "client-1")
			m := s.mark()
			s.call("thread/goal/set", map[string]any{"threadId": id, "objective": objective, "status": "active"})
			own, m := s.started(t, m, "the goal's turn")
			if status, _ := s.completed(t, m, own); status != "failed" {
				t.Fatalf("the goal turn ended %s, want failed", status)
			}
			if !s.quiet(s.mark(), 3*time.Second) {
				t.Error("codex started another goal turn after one failed")
			}
			if _, status := s.goalOf(id); status != want {
				t.Errorf("the goal is %q, want %s", status, want)
			}
		})
	}
}

// A goal that is not active stays as it is across a stop and a resume, and
// starts nothing. An interrupt with no turn running is refused.
func TestRestingGoalSurvivesARestart(t *testing.T) {
	for _, status := range []string{"paused", "blocked", "usageLimited", "budgetLimited", "complete"} {
		t.Run(status, func(t *testing.T) {
			_, s, _, l := setup(t)
			id := newThread(s, l)
			ping(t, s, id, "PING one", "client-1")
			s.call("thread/goal/set", map[string]any{"threadId": id, "objective": "GOAL 9 5", "status": status})
			s2 := restart(t, s, l)
			if _, got := s2.goalOf(id); got != status {
				t.Errorf("before the thread resumes the goal is %q, want %s", got, status)
			}
			m := s2.mark()
			s2.call("thread/resume", map[string]any{"threadId": id, "cwd": l.Workspace, "approvalPolicy": "never", "sandbox": "danger-full-access"})
			if !s2.quiet(m, 1500*time.Millisecond) {
				t.Errorf("a resumed thread started a turn for a goal that is %s", status)
			}
			if _, got := s2.goalOf(id); got != status {
				t.Errorf("after the resume the goal is %q, want %s", got, status)
			}
			refused := s2.call("turn/interrupt", map[string]any{"threadId": id, "turnId": "00000000-0000-4000-8000-000000000000"})
			if !strings.Contains(string(refused), "no active turn") {
				t.Errorf("an interrupt with no turn running answered %s", refused)
			}
		})
	}
}
