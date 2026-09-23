package wrapper_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Config.OnEvent (ADR-008): every event, in order, the Terminated one last,
// with a slow consumer; and a stalled one never holds Stop.

func onEventSession(t *testing.T, script string, onEvent func(wrapper.SessionEvent), limits wrapper.QueueLimits) *wrapper.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	sess, err := wrapper.Start(ctx, wrapper.Config{
		BinaryPath:                "/bin/sh",
		Args:                      []string{"-c", script},
		Stdout:                    io.Discard,
		Harness:                   "claude",
		IdleQuiet:                 kaQuiet,
		IdleClassify:              kaClassify,
		KeepAliveOnClassification: true,
		OnEvent:                   onEvent,
		EventQueue:                limits,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = sess.Stop(stopCtx)
	})
	return sess
}

// A slow consumer still gets every event, in order, ending on Terminated —
// where Events() would have dropped what it could not hold.
func TestOnEvent_EveryEventInOrder(t *testing.T) {
	var mu sync.Mutex
	var got []wrapper.SessionEvent
	done := make(chan struct{})
	onEvent := func(ev wrapper.SessionEvent) {
		time.Sleep(20 * time.Millisecond) // a slow consumer
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
		if ev.Terminated {
			close(done)
		}
	}
	// A wall, output past it (the status clears silently), the wall again,
	// then an exit: two verdicts and the final event.
	script := printLines(t, banner) + "; sleep 0.6; echo working; sleep 0.3; " + printLines(t, banner) + "; sleep 0.6; exit 3"
	sess := onEventSession(t, script, onEvent, wrapper.QueueLimits{})
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("OnEvent never saw the Terminated event")
	}
	_, _ = sess.Wait()
	mu.Lock()
	defer mu.Unlock()
	if n := len(got); n < 3 || !got[n-1].Terminated {
		t.Fatalf("events = %+v, want the verdicts then Terminated last", got)
	}
	for i, ev := range got[:len(got)-1] {
		if ev.Terminated || ev.Status != wrapper.StatusBlockedByCost {
			t.Fatalf("event %d = %+v, want a blocked_by_cost verdict", i, ev)
		}
		if i > 0 && ev.At.Before(got[i-1].At) {
			t.Fatalf("event %d is older than the one before it", i)
		}
	}
}

// A consumer that never returns cannot hold the session: Stop returns, and
// Wait with it, however full its queue.
func TestOnEvent_StalledConsumerNeverHoldsStop(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	onEvent := func(wrapper.SessionEvent) { <-block }
	script := printLines(t, banner) + "; sleep 0.4; echo a; sleep 0.4; " + printLines(t, banner) + "; exec sleep 30"
	sess := onEventSession(t, script, onEvent, wrapper.QueueLimits{Events: 1})
	eventually(t, "a verdict", func() bool { return sess.Snapshot().Status == wrapper.StatusBlockedByCost })
	time.Sleep(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.Stop(ctx); err != nil {
		t.Fatalf("Stop with a stalled OnEvent: %v", err)
	}
	if res, _ := sess.Wait(); res.Status != wrapper.StatusInterrupted {
		t.Fatalf("Result.Status = %q, want interrupted", res.Status)
	}
}

func TestSnapshot_ClassifiedAt(t *testing.T) {
	before := time.Now()
	sess := scriptSession(t, printLines(t, banner)+"; exec sleep 30", true)
	eventually(t, "the verdict", func() bool { return sess.Snapshot().Status == wrapper.StatusBlockedByCost })
	if at := sess.Snapshot().ClassifiedAt; at.Before(before) || at.After(time.Now()) {
		t.Fatalf("ClassifiedAt = %v, want the verdict's time", at)
	}
}

func TestConfig_NegativeEventQueueRefused(t *testing.T) {
	for _, limits := range []wrapper.QueueLimits{{Events: -1}, {Bytes: -1}} {
		_, err := wrapper.Start(context.Background(), wrapper.Config{
			BinaryPath: "/bin/sh", Stdout: io.Discard, EventQueue: limits,
		})
		if !errors.Is(err, wrapper.ErrInvalidConfig) {
			t.Fatalf("EventQueue %+v: err = %v, want ErrInvalidConfig", limits, err)
		}
	}
}
