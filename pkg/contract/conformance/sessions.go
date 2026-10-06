package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The scenarios of Sessions side by side (capability concurrent_sessions):
// several Sessions of one agent open at once, over one layout and one staged
// credential, each in a Host of its own as far as the adapter can tell. The
// kit opens as many as the Descriptor allows where the number is the point —
// the most an adapter may declare is the most it has run — and two
// elsewhere.

// sideBySide reports whether the adapter runs Sessions side by side; it logs
// the skip when not.
func (c *check) sideBySide() bool {
	if c.has(contract.CapConcurrentSessions) {
		return true
	}
	c.t.Logf("no %s: skipped", contract.CapConcurrentSessions)
	return false
}

// pingOf is the number Session i says in round r: each its own, so that a
// reply tells whose it is.
func pingOf(i, r int) int { return 1000 + 100*i + r }

// sideMark marks what Session i's tool calls run.
func sideMark(i int) string { return fmt.Sprintf("side-%d-", i) }

// openAll opens n fresh Sessions of a at once, and watches each.
func (c *check) openAll(a *agent, n int, rule string) ([]contract.Session, []*host) {
	return c.openAllWith(n, rule, func(int) contract.OpenRequest { return c.openRequest(a, contract.OpenFresh, "", nil) })
}

// openAllWith opens n Sessions at once, the i-th from req(i), and watches
// each. Once every Open returned, a failed one stops the scenario.
func (c *check) openAllWith(n int, rule string, req func(i int) contract.OpenRequest) ([]contract.Session, []*host) {
	ss := make([]contract.Session, n)
	for i := range ss {
		s, err := c.f.Adapter.NewSession(req(i))
		if err != nil {
			c.stop(rule, "NewSession of Session %d: %v", i, err)
		}
		ss[i] = s
		c.cleanups = append(c.cleanups, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = s.Close(ctx, contract.ClosePark, 0)
		})
	}
	ids, errs := make([]string, n), make([]error, n)
	ctx, cancel := c.ctx()
	defer cancel()
	var wg sync.WaitGroup
	for i, s := range ss {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Open(ctx)
			ids[i], errs[i] = res.SessionID, err
		}()
	}
	wg.Wait()
	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Sprintf("Session %d: %v", i, err))
		}
	}
	if len(failed) > 0 {
		c.stop(rule, "%d of %d Sessions opened at once failed: %s", len(failed), n, strings.Join(failed, "; "))
	}
	hs := make([]*host, n)
	for i, s := range ss {
		hs[i] = c.watch(s)
		hs[i].s, hs[i].id = s, ids[i]
	}
	return ss, hs
}

// sendAll sends each Session its text at once, and returns the input ids in
// order. Once every Send returned, a failed one stops the scenario.
func (c *check) sendAll(ss []contract.Session, text func(i int) string, rule string) []string {
	ins, errs := make([]string, len(ss)), make([]error, len(ss))
	for i := range ss {
		ins[i] = newInputID()
		c.mark(ins[i])
	}
	ctx, cancel := c.ctx()
	defer cancel()
	var wg sync.WaitGroup
	for i, s := range ss {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Send(ctx, contract.Text(ins[i], text(i)))
			if err == nil && (res.Receipt != contract.ReceiptSubmitted || res.TurnID == "") {
				err = fmt.Errorf("%+v, want submitted with a turn id", res)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			c.stop(rule, "Send(%q) to Session %d: %v", text(i), i, err)
		}
	}
	return ins
}

// started waits for each input's turn to reach the record.
func (c *check) started(hs []*host, ins []string, rule string) {
	for i, in := range ins {
		if _, ok := hs[i].awaitKind(contract.KindUserInput, in); !ok {
			c.stop(rule, "Session %d's turn never started", i)
		}
	}
}

// completes requires the input's turn to complete, with want in its reply;
// a tool call's reply is the harness's own, so its want is "".
func (c *check) completes(h *host, in, want, rule, format string, args ...any) {
	c.t.Helper()
	if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted || !strings.Contains(end.Text, want) {
		c.fail(rule, "%s: turn_ended %+v, want completed with %q", fmt.Sprintf(format, args...), end, want)
	}
}

// concurrentOpen: as many Sessions as the Descriptor allows open at once on a
// fresh agent, each under an id of its own, and each takes an input at once;
// a Session open in one Host is refused to another with session_in_use,
// which ends nothing of it.
func concurrentOpen(c *check) {
	if !c.sideBySide() {
		return
	}
	a := c.newAgent()
	n := c.desc.Sessions()
	ss, hs := c.openAll(a, n, "concurrent.open")
	seen := map[string]int{}
	for i, h := range hs {
		if j, dup := seen[h.id]; dup {
			c.fail("concurrent.ids", "Sessions %d and %d opened under one id, %q", j, i, h.id)
		}
		seen[h.id] = i
	}
	ins := c.sendAll(ss, func(i int) string { return fmt.Sprintf("PING %d", pingOf(i, 0)) }, "concurrent.open")
	for i, in := range ins {
		c.completes(hs[i], in, fmt.Sprintf("PONG %d", pingOf(i, 0)), "concurrent.open", "Session %d of %d opened at once", i, n)
	}
	if !c.has(contract.CapResume) {
		return
	}
	c.awaitPhase(ss[0], contract.PhaseIdle)
	again, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenReopen, hs[0].id, nil))
	if err == nil {
		ctx, cancel := c.ctx()
		_, err = again.Open(ctx)
		cancel()
		c.cleanups = append(c.cleanups, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = again.Close(ctx, contract.ClosePark, 0)
		})
	}
	var e *contract.Error
	if !errors.As(err, &e) || e.Code != contract.CodeOpenFailed || e.Reason != contract.OpenSessionInUse {
		c.fail("concurrent.one-host", "opening Session %q in a second Host while it runs: %v, want open_failed session_in_use", hs[0].id, err)
	}
	in := c.send(ss[0], fmt.Sprintf("PING %d", pingOf(0, 1)))
	c.completes(hs[0], in, fmt.Sprintf("PONG %d", pingOf(0, 1)), "concurrent.one-host", "the Session a second Host was refused, after")
}

// concurrentTurns: Sessions side by side take turns at once, each its own —
// a reply, a tool call, and text the harness streams — and nothing one does
// is delivered to another.
func concurrentTurns(c *check) {
	if !c.sideBySide() {
		return
	}
	a := c.newAgent()
	n := c.desc.Sessions()
	ss, hs := c.openAll(a, n, "concurrent.turns")
	rounds := []func(i int) (text, want string){
		func(i int) (string, string) {
			return fmt.Sprintf("PING %d", pingOf(i, 0)), fmt.Sprintf("PONG %d", pingOf(i, 0))
		},
		func(i int) (string, string) { return "TOOL echo " + sideMark(i) + "tool", "" },
		func(int) (string, string) { return "SLOW 20", "slow" },
	}
	sentTo := make([]map[string]bool, n)
	for i := range sentTo {
		sentTo[i] = map[string]bool{}
	}
	for _, round := range rounds {
		ins := c.sendAll(ss, func(i int) string { text, _ := round(i); return text }, "concurrent.turns")
		for i, in := range ins {
			sentTo[i][in] = true
			text, want := round(i)
			c.completes(hs[i], in, want, "concurrent.turns", "Session %d of %d, %q", i, n, text)
		}
		for _, s := range ss {
			c.awaitPhase(s, contract.PhaseIdle)
		}
	}
	time.Sleep(200 * time.Millisecond) // let late observations arrive
	for i, h := range hs {
		for _, o := range h.deliveries(func(contract.Observation) bool { return true }) {
			if o.InputID != "" && !sentTo[i][o.InputID] {
				c.fail("concurrent.isolated", "Session %d was delivered %s, of input %s: another Session's", i, o.ID, o.InputID)
				continue
			}
			for j := range hs {
				if j != i && (strings.Contains(string(o.Data), sideMark(j)) || strings.Contains(string(o.Data), fmt.Sprintf("PONG %d", pingOf(j, 0)))) {
					c.fail("concurrent.isolated", "Session %d was delivered %s, which tells of Session %d's work", i, o.ID, j)
				}
			}
		}
	}
}

// concurrentInterrupt: an interrupt of one Session's turn stops that turn
// alone, and the turn beside it completes.
func concurrentInterrupt(c *check) {
	if !c.sideBySide() {
		return
	}
	a := c.newAgent()
	ss, hs := c.openAll(a, 2, "concurrent.interrupt")
	ins := c.sendAll(ss, func(i int) string { return []string{"SLOW 200", "SLOW 40"}[i] }, "concurrent.interrupt")
	c.started(hs, ins, "concurrent.interrupt")
	time.Sleep(150 * time.Millisecond)
	ctx, cancel := c.ctx()
	defer cancel()
	if out, err := ss[0].Interrupt(ctx, contract.InterruptRequest{InputID: ins[0], DeadlineMS: 10000}); err != nil || out != contract.InterruptStopped {
		c.fail("concurrent.interrupt", "interrupting Session 0's turn: %v %v, want stopped", out, err)
	}
	if end, ok := hs[0].turnEnded(ins[0]); !ok || end.Outcome != contract.TurnInterrupted {
		c.fail("concurrent.interrupt", "Session 0's turn_ended %+v, want interrupted", end)
	}
	c.completes(hs[1], ins[1], "slow", "concurrent.interrupt", "Session 1's turn, beside the one interrupted")
}

// concurrentClose: closing one Session ends it alone: the turn running beside
// it completes, and that Session takes another.
func concurrentClose(c *check) {
	if !c.sideBySide() {
		return
	}
	a := c.newAgent()
	ss, hs := c.openAll(a, 2, "concurrent.close")
	in := c.send(ss[1], "SLOW 40")
	c.started(hs[1:], []string{in}, "concurrent.close")
	ctx, cancel := c.ctx()
	defer cancel()
	if res, err := ss[0].Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Stopped {
		c.fail("concurrent.close", "Close of Session 0 = %+v %v, want stopped", res, err)
	}
	c.completes(hs[1], in, "slow", "concurrent.close", "Session 1's turn, while Session 0 closed")
	c.awaitPhase(ss[1], contract.PhaseIdle)
	next := c.send(ss[1], fmt.Sprintf("PING %d", pingOf(1, 1)))
	c.completes(hs[1], next, fmt.Sprintf("PONG %d", pingOf(1, 1)), "concurrent.close", "Session 1, after Session 0 closed")
}

// concurrentCrash: one Session's harness crashing ends nothing beside it, and
// the crashed Session reopens while the other runs.
func concurrentCrash(c *check) {
	if !c.sideBySide() {
		return
	}
	a := c.newAgent()
	ss, hs := c.openAll(a, 2, "concurrent.crash")
	ins := c.sendAll(ss, func(i int) string { return []string{"STALL 30", "SLOW 40"}[i] }, "concurrent.crash")
	c.started(hs, ins, "concurrent.crash")
	c.f.Kill(c.t, ss[0])
	hs[0].halt()
	c.completes(hs[1], ins[1], "slow", "concurrent.crash", "Session 1's turn, while Session 0's harness crashed")
	c.awaitPhase(ss[1], contract.PhaseIdle)
	if !c.has(contract.CapResume) {
		return
	}
	busy := c.send(ss[1], "SLOW 40")
	s, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenReopen, hs[0].id, hs[0].checkpoint()))
	if err != nil {
		c.stop("concurrent.crash", "NewSession(reopen) of the crashed Session: %v", err)
	}
	s, h := c.openWatch(s, "concurrent.crash")
	in := c.send(s, fmt.Sprintf("PING %d", pingOf(0, 1)))
	c.completes(h, in, fmt.Sprintf("PONG %d", pingOf(0, 1)), "concurrent.crash", "the crashed Session, reopened while Session 1 runs")
	c.completes(hs[1], busy, "slow", "concurrent.crash", "Session 1's turn, while the crashed Session reopened")
}

// concurrentRecord: a record handle on one Session, read while another
// works, delivers its own Session's record alone and takes nothing of the
// other's, which observes its tool calls itself.
func concurrentRecord(c *check) {
	if !c.sideBySide() {
		return
	}
	a := c.newAgent()
	ss, hs := c.openAll(a, 2, "concurrent.record")
	mark := func(k int) string { return fmt.Sprintf("%srecord%d", sideMark(1), k) }
	var ins1 []string
	tools := func(in string, k int) {
		c.completes(hs[1], in, "", "concurrent.record", "Session 1's turn %d", k)
		c.awaitPhase(ss[1], contract.PhaseIdle)
		if !c.has(contract.CapToolsObserved) {
			return
		}
		for _, kind := range []contract.Kind{contract.KindToolStarted, contract.KindToolFinished} {
			if _, ok := hs[1].await(string(kind), func(o contract.Observation) bool {
				return o.Kind == kind && strings.Contains(string(o.Data), mark(k))
			}); !ok {
				c.fail("concurrent.record", "Session 1 observed no %s of its tool call %q", kind, mark(k))
			}
		}
	}
	// Both work, and then Session 0 closes and its record is read while
	// Session 1 works on.
	ins := c.sendAll(ss, func(i int) string { return []string{"TOOL echo " + sideMark(0) + "record", "TOOL echo " + mark(0)}[i] }, "concurrent.record")
	c.completes(hs[0], ins[0], "", "concurrent.record", "Session 0's turn")
	ins1 = append(ins1, ins[1])
	tools(ins[1], 0)
	ctx, cancel := c.ctx()
	defer cancel()
	if res, err := ss[0].Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.stop("concurrent.record", "Close of Session 0 = %+v %v, want drained", res, err)
	}
	hs[0].halt()
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: hs[0].id, OpenConfig: a.result.OpenConfig, Layout: a.layout})
	if err != nil {
		c.stop("concurrent.record", "OpenRecord of Session 0: %v", err)
	}
	defer func() { _ = r.Close() }()
	rh := c.watch(r)
	for k := 1; k < 3; k++ {
		in := c.send(ss[1], "TOOL echo "+mark(k))
		ins1 = append(ins1, in)
		tools(in, k)
	}
	if _, ok := rh.awaitKind(contract.KindUserInput, ins[0]); !ok {
		c.fail("concurrent.record", "the record handle on Session 0 delivered no user_input of its input")
	}
	time.Sleep(200 * time.Millisecond)
	for _, o := range rh.deliveries(func(contract.Observation) bool { return true }) {
		for _, in := range ins1 {
			if o.InputID == in {
				c.fail("concurrent.record", "the record handle on Session 0 delivered %s, of Session 1's input %s", o.ID, in)
			}
		}
		if strings.Contains(string(o.Data), sideMark(1)) {
			c.fail("concurrent.record", "the record handle on Session 0 delivered %s, which tells of Session 1's work", o.ID)
		}
	}
}

// concurrentLoad: an agent's Sessions, saved together, load together: each
// reopens, loaded, under its saved id while the other does, and its model is
// given its own conversation, never its sibling's.
func concurrentLoad(c *check) {
	if !c.sideBySide() {
		return
	}
	if !c.has(contract.CapSessionLoad) {
		c.t.Logf("no session_load: skipped")
		return
	}
	src := c.newAgent()
	ss, hs := c.openAll(src, 2, "concurrent.load")
	ins := c.sendAll(ss, func(i int) string { return fmt.Sprintf("PING %d", pingOf(i, 2)) }, "concurrent.load")
	ctx, cancel := c.ctx()
	defer cancel()
	for i, in := range ins {
		c.completes(hs[i], in, fmt.Sprintf("PONG %d", pingOf(i, 2)), "concurrent.load", "Session %d's turn to save", i)
	}
	for i, s := range ss {
		if res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
			c.stop("concurrent.load", "Close of Session %d = %+v %v, want drained", i, res, err)
		}
		hs[i].halt()
	}
	saved := c.save(src, "concurrent.load")
	dst := c.loadAgent(saved, true, "concurrent.load")
	cps := make([]*contract.Checkpoint, len(hs))
	for i, h := range hs {
		_, cps[i] = c.seed(dst, h.id, "concurrent.load")
	}
	ls, lhs := c.openAllWith(len(hs), "concurrent.load", func(i int) contract.OpenRequest {
		req := c.openRequest(dst, contract.OpenReopen, hs[i].id, cps[i])
		req.Loaded = true
		return req
	})
	for i, h := range lhs {
		if h.id != hs[i].id {
			c.fail("concurrent.load", "loaded Session %d opened as %q, want the saved %q", i, h.id, hs[i].id)
		}
	}
	for i, s := range ls {
		in := c.send(s, fmt.Sprintf("PING %d", pingOf(i, 3)))
		c.completes(lhs[i], in, fmt.Sprintf("PONG %d", pingOf(i, 3)), "concurrent.load", "loaded Session %d", i)
		text, ok := c.heard(s)
		if !ok {
			continue
		}
		if own := fmt.Sprintf("PING %d", pingOf(i, 2)); !strings.Contains(text, own) {
			c.fail("concurrent.load-history", "loaded Session %d's model was not given %q, its own saved conversation", i, own)
		}
		if other := fmt.Sprintf("PING %d", pingOf(1-i, 2)); strings.Contains(text, other) {
			c.fail("concurrent.load-history", "loaded Session %d's model was given %q, its sibling's conversation", i, other)
		}
		c.awaitPhase(s, contract.PhaseIdle)
	}
}
