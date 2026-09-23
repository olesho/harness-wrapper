package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// frames replays a recording in small writes and returns every distinct
// snapshot, in order.
func frames(t *testing.T, scenario string) []screen.Snapshot {
	t.Helper()
	raw := corpusBytes(t, scenario)
	scr := screen.New(120, 40)
	var out []screen.Snapshot
	last := ""
	for i := 0; i < len(raw); i += 64 {
		_, _ = scr.Write(raw[i:min(i+64, len(raw))])
		if snap := scr.Snapshot(); snap.Text != last {
			out = append(out, snap)
			last = snap.Text
		}
	}
	return out
}

// TestBusy_RetryBackoffIsBusy replays claude 2.1.280 retrying a failed API
// call (recorded against a local API answering 529). Through the backoff its
// status line reads "✻ API error · Retrying in Ns · attempt N/10", and every
// such frame must read busy — whatever the footer shows while it repaints.
func TestBusy_RetryBackoffIsBusy(t *testing.T) {
	a := New()
	for _, scenario := range []string{"api-error-retry-recovers", "api-error-retry-gives-up"} {
		t.Run(scenario, func(t *testing.T) {
			backoff := 0
			for _, snap := range frames(t, scenario) {
				status, _, ok := statusRegion(snap.Text)
				if !ok || !strings.Contains(status, "Retrying in") {
					continue
				}
				backoff++
				if !a.Busy(snap) {
					t.Fatalf("Busy = false during the retry backoff:\n%s", snap.Text)
				}
			}
			if backoff == 0 {
				t.Fatal("no frame showed the retry backoff; the recording changed")
			}
		})
	}
}

// TestBusy_RetryBackoffWithoutTheFooterHint is a backoff frame from a live
// run of claude 2.1.280 whose footer showed "← 1 agent": there the footer
// dropped "esc to interrupt" for the whole backoff, so the whole-screen markers
// read Claude idle while it was about to retry. The status line still says it
// is working.
func TestBusy_RetryBackoffWithoutTheFooterHint(t *testing.T) {
	frame := strings.Join([]string{
		"❯ reply with exactly RECOVERED_REPLY_OK",
		"✻ API error · Retrying in 1s · attempt 1/10",
		"                                                                                                     ◐ medium · /effort",
		strings.Repeat("─", 120),
		"❯ ",
		strings.Repeat("─", 119),
		"  ⏵⏵ auto mode on (shift+tab to cycle) · ← 1 agent",
	}, "\n")
	if strings.Contains(frame, busyMarker) || workingRE.MatchString(frame) {
		t.Fatal("the frame carries a whole-screen marker; it no longer shows the gap")
	}
	if !New().Busy(screen.Snapshot{Text: frame}) {
		t.Fatal("Busy = false during a retry backoff with no footer hint")
	}
}

// TestRetryRecordings_EndSettled: each retry recording ends on a settled
// frame — not busy, with the end-of-turn summary — carrying what the turn
// produced: the reply, or the error claude gave up with.
func TestRetryRecordings_EndSettled(t *testing.T) {
	for _, tc := range []struct{ scenario, reply string }{
		{"api-error-retry-recovers", "RECOVERED_REPLY_OK"},
		{"api-error-retry-gives-up", "API Error: Repeated 529 Overloaded errors."},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			a := New()
			var settled screen.Snapshot
			for _, snap := range frames(t, tc.scenario) {
				for _, ev := range a.OnScreen(snap) {
					if ev.Kind == turns.TurnComplete {
						settled = snap
					}
				}
			}
			if settled.Text == "" {
				t.Fatal("no end-of-turn marker fired")
			}
			if a.Busy(settled) {
				t.Fatalf("the settled frame reads busy:\n%s", settled.Text)
			}
			if msg, ok := a.ExtractMessage(settled); !ok || !strings.HasPrefix(msg, tc.reply) {
				t.Fatalf("ExtractMessage = %q, want it to start %q", msg, tc.reply)
			}
		})
	}
}

// TestBusy_QuotedMarkersAreNotBusy is the adversarial row: a settled reply that
// quotes the footer hint and a spinner line. They sit in the conversation, above
// the status region, so Claude is not busy — the whole-screen reading called
// this frame busy, and the turn could never complete.
func TestBusy_QuotedMarkersAreNotBusy(t *testing.T) {
	a := New()
	settled := strings.Join([]string{
		"❯ what does claude show while it works?",
		"",
		"⏺ While a turn runs the footer says \"esc to interrupt\", and the status line",
		"  shows a spinner such as \"✶ Cerebrating… (57s · ↓ 4.8k tokens)\" or, while it",
		"  backs off, \"✻ API error · Retrying in 1s · attempt 1/10\".",
		"",
		"✻ Baked for 3s · done 11:42 AM",
		"                                                              ◐ medium · /effort",
		strings.Repeat("─", 100),
		"❯ ",
		strings.Repeat("─", 100),
		"  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents",
	}, "\n")
	if a.Busy(screen.Snapshot{Text: settled}) {
		t.Fatal("a reply quoting the busy markers made a settled Claude read busy")
	}
	working := strings.Replace(settled, "✻ Baked for 3s · done 11:42 AM", "✶ Cerebrating… (57s · ↓ 4.8k tokens)", 1)
	if !a.Busy(screen.Snapshot{Text: working}) {
		t.Fatal("the spinner in the status line did not read busy")
	}
	footer := strings.Replace(settled, "· ← for agents", "· esc to interrupt · ← for agents", 1)
	if !a.Busy(screen.Snapshot{Text: footer}) {
		t.Fatal("the footer's esc to interrupt did not read busy")
	}
}
