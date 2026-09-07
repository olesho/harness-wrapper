package claudecode

import (
	"bytes"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// Two live Claude Code 2.1.261 recordings of the folder-trust dialog. The rest
// of this package's trust coverage is hand-written strings, which pin what we
// BELIEVE the dialog looks like — these pin what it actually emitted, so the
// next upstream re-layout fails here instead of in production.
const (
	trustDialogCorpus = "trust-dialog-unnumbered" // the dialog alone
	// The dialog plus the frames produced by answering it with a single
	// "\x1b[B\r" — the exact keys this adapter reports for the proceed option.
	trustConfirmedCorpus = "trust-dialog-confirmed"
)

// trustReplayChunk is the byte granularity of the replays below.
//
// It is not arbitrary, and the extremes both hide the thing being tested.
// Measured on these two recordings:
//
//   - >= 1 KiB: the trust-dialog-confirmed answer lands in the same write as
//     the dialog, so the emulator never renders a frame carrying the dialog and
//     NOTHING is emitted at all.
//   - <= 64 B: a write splits an option row mid-paint, the adapter parses the
//     truncated label ("Yes, I trust this"), and the completed row then hashes
//     to a different input ID — a second InputRequested for one dialog.
//
// 128 bytes is fine enough to see every intermediate frame the way production
// does, and coarse enough that each row lands whole.
const trustReplayChunk = 128

// replayCorpus feeds a recording through the screen emulator incrementally, as
// the wrapper does in production, and returns every event plus the final frame.
func replayCorpus(t *testing.T, scenario string) ([]turns.Event, screen.Snapshot) {
	t.Helper()
	raw := corpusBytes(t, scenario)

	scr := screen.New(120, 40)
	a := New()
	var evs []turns.Event
	var last screen.Snapshot
	for i := 0; i < len(raw); i += trustReplayChunk {
		end := i + trustReplayChunk
		if end > len(raw) {
			end = len(raw)
		}
		_, _ = scr.Write(raw[i:end])
		last = scr.Snapshot()
		evs = append(evs, a.OnScreen(last)...)
	}
	return evs, last
}

func eventsOfKind(evs []turns.Event, kind turns.Kind) []turns.Event {
	var out []turns.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// assertTrustRequest holds a request from either recording against the live
// dialog: two options, the destructive one FIRST and highlighted, and the
// accepting one reachable only by navigating down. The keys are asserted, not
// just the aliases — a regression to a digit keystroke would type "2" into the
// dialog and select nothing, and the labels alone would not catch it.
func assertTrustRequest(t *testing.T, req *turns.InputRequest) {
	t.Helper()
	if req.Kind != "trust_prompt" {
		t.Errorf("Kind = %q, want trust_prompt", req.Kind)
	}
	if req.Prompt != trustAnchorAlt {
		t.Errorf("Prompt = %q, want %q", req.Prompt, trustAnchorAlt)
	}
	if len(req.Options) != 2 {
		t.Fatalf("got %d options, want exactly 2: %+v", len(req.Options), req.Options)
	}
	byAlias := map[string]turns.InputOption{}
	for _, o := range req.Options {
		byAlias[o.Alias] = o
	}
	proceed, ok := byAlias["proceed"]
	if !ok {
		t.Fatalf("no option aliased proceed: %+v", req.Options)
	}
	if proceed.Label != "Yes, I trust this folder" {
		t.Errorf("proceed label = %q, want %q", proceed.Label, "Yes, I trust this folder")
	}
	if want := []byte("\x1b[B\r"); !bytes.Equal(proceed.Keys, want) {
		t.Errorf("proceed keys = %q, want %q (navigate, never a digit)", proceed.Keys, want)
	}
	deny, ok := byAlias["deny"]
	if !ok {
		t.Fatalf("no option aliased deny: %+v", req.Options)
	}
	if deny.Label != "No, exit" {
		t.Errorf("deny label = %q, want %q", deny.Label, "No, exit")
	}
	// The default really is the destructive one: bare Enter exits.
	if want := []byte("\r"); !bytes.Equal(deny.Keys, want) {
		t.Errorf("deny keys = %q, want %q (the highlight starts on No, exit)", deny.Keys, want)
	}
}

// The live dialog must be detected exactly once across the whole replay — one
// InputRequested, not one per repaint.
func TestTrustDialogCorpus_UnnumberedDialogDetectedOnce(t *testing.T) {
	evs, final := replayCorpus(t, trustDialogCorpus)

	reqs := eventsOfKind(evs, turns.InputRequested)
	if len(reqs) != 1 {
		t.Fatalf("InputRequested count = %d, want exactly 1; events: %+v", len(reqs), evs)
	}
	assertTrustRequest(t, reqs[0].Input)

	// Guard the recording itself: it must really be the UNNUMBERED layout, or
	// the numbered parser would be what is under test here.
	if !strings.Contains(final.Text, "❯ No, exit") {
		t.Error("recording no longer shows the highlight on \"No, exit\"; re-read meta.json before touching the parser")
	}
	if strings.Contains(final.Text, "1. Yes") || strings.Contains(final.Text, "2. No") {
		t.Error("recording carries numbered options; it no longer pins the unnumbered layout")
	}
}

// Answering the dialog with the proceed keys clears it: the anchor leaves the
// screen and the adapter reports InputResolved rather than holding a request
// that can never be answered again.
func TestTrustDialogCorpus_ConfirmedDialogResolves(t *testing.T) {
	evs, final := replayCorpus(t, trustConfirmedCorpus)

	reqs := eventsOfKind(evs, turns.InputRequested)
	if len(reqs) != 1 {
		t.Fatalf("InputRequested count = %d, want exactly 1; events: %+v", len(reqs), evs)
	}
	assertTrustRequest(t, reqs[0].Input)

	res := eventsOfKind(evs, turns.InputResolved)
	if len(res) != 1 {
		t.Fatalf("InputResolved count = %d, want exactly 1; events: %+v", len(res), evs)
	}
	// The resolution must carry the dialog it resolves, so a consumer can tell
	// WHICH request stopped blocking.
	if res[0].Input == nil || res[0].Input.ID != reqs[0].Input.ID {
		t.Errorf("InputResolved carries %+v, want the id of the request it resolves (%s)", res[0].Input, reqs[0].Input.ID)
	}
	// Ordering: the request has to come first, or "resolved" means nothing.
	var sawReq bool
	for _, ev := range evs {
		switch ev.Kind {
		case turns.InputRequested:
			sawReq = true
		case turns.InputResolved:
			if !sawReq {
				t.Fatal("InputResolved was emitted before any InputRequested")
			}
		}
	}

	if strings.Contains(final.Text, trustAnchorAlt) {
		t.Errorf("the anchor is still on the final frame after \\x1b[B\\r; the dialog was not actually answered:\n%s", final.Text)
	}
	// The post-answer frames are the session proper — that is what makes this
	// recording the pin for "the answer was accepted", not just "the screen
	// changed".
	if !strings.Contains(final.Text, "Claude Code v2.1.261") {
		t.Errorf("final frame does not show the started session:\n%s", final.Text)
	}
}
