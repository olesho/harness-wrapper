package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// firstReading replays frames from start and returns the first reading of
// prompt's turn that is not pending, with its frame index — asked only once the
// turn has been taken: a busy frame showing its echo. Before that the composer
// holds the prompt as typed, which is what a cancel leaves too (the chat layer
// asks from the same point). -1 when no frame reads anything.
func firstReading(frames []screen.Snapshot, start int, prompt string) (turns.InterruptOutcome, string, int) {
	a := New()
	taken := false
	for i := start; i < len(frames); i++ {
		snap := frames[i]
		if !taken {
			taken = a.Busy(snap) && strings.Contains(snap.Text, "❯ "+prompt)
			continue
		}
		if outcome, partial := a.InterruptOutcome(prompt, snap); outcome != turns.InterruptPending {
			return outcome, partial, i
		}
	}
	return turns.InterruptPending, "", -1
}

// TestInterruptRecordings replays claude 2.1.280 interrupted through
// Conversation.Interrupt (test/corpus/claude-code/interrupt-*): each turn reads
// pending until the harness answers, and then what it did.
func TestInterruptRecordings(t *testing.T) {
	t.Run("mid-reply", func(t *testing.T) {
		outcome, partial, _ := firstReading(frames(t, "interrupt-mid-reply"), 0, "HW_STREAM tell me a story")
		if outcome != turns.InterruptStopped || !strings.HasPrefix(partial, "word 1 word 2 word 3 word 4 word 5 word 6") {
			t.Fatalf("reading = %v %q, want stopped with the partial reply", outcome, partial)
		}
	})
	t.Run("mid-tool", func(t *testing.T) {
		outcome, partial, _ := firstReading(frames(t, "interrupt-mid-tool"), 0, "HW_TOOL run a command")
		if outcome != turns.InterruptStopped || partial != "" {
			t.Fatalf("reading = %v %q, want stopped with no reply: the tool call is not one", outcome, partial)
		}
	})
	t.Run("before-first-token", func(t *testing.T) {
		all := frames(t, "interrupt-before-first-token")
		outcome, _, at := firstReading(all, 0, "HW_DELAY answer slowly")
		if outcome != turns.InterruptCancelled {
			t.Fatalf("reading = %v, want cancelled", outcome)
		}
		// The next prompt, typed once chat cleared the composer, is answered.
		if outcome, _, _ := firstReading(all, at, "HW_QUICK after the cancel"); outcome != turns.InterruptFinished {
			t.Fatalf("next turn reads %v, want finished", outcome)
		}
	})
	t.Run("second-turn", func(t *testing.T) {
		all := frames(t, "interrupt-second-turn")
		outcome, _, at := firstReading(all, 0, "HW_STREAM first story")
		if outcome != turns.InterruptStopped {
			t.Fatalf("first turn reads %v, want stopped", outcome)
		}
		// The first marker stays on screen above the second turn, which reads
		// pending until its own marker lands.
		outcome, partial, _ := firstReading(all, at, "HW_STREAM second story")
		if outcome != turns.InterruptStopped || !strings.HasPrefix(partial, "word 1") {
			t.Fatalf("second turn reads %v %q, want stopped with its own partial reply", outcome, partial)
		}
	})
}

// TestInterruptRecording_2_1_270 reads the interrupted-mid-reply recording made
// on 2.1.270 with a terminal's Esc.
func TestInterruptRecording_2_1_270(t *testing.T) {
	snap := lastLiveFrame(t, "interrupted-mid-reply")
	const prompt = "tell me a long detailed story about a wizard navigating a haunted library. take at least 1500 words and include vivid descriptions of every room."
	outcome, partial := New().InterruptOutcome(prompt, snap)
	if outcome != turns.InterruptStopped || !strings.HasPrefix(partial, "The Wizard's Library") ||
		!strings.HasSuffix(partial, "mid-swing, and when") {
		t.Fatalf("reading = %v %q, want stopped with the partial story", outcome, partial)
	}
}

// OnScreen emits nothing for an interrupt: the marker is read per turn, by
// InterruptOutcome, never by its presence (an earlier turn's stays painted).
func TestOnScreenEmitsNoInterruptEvent(t *testing.T) {
	if evs := New().OnScreen(lastLiveFrame(t, "interrupted-mid-reply")); len(evs) != 0 {
		t.Fatalf("OnScreen emitted %+v for an interrupt marker", evs)
	}
}

// TestRewindPickerIsNoComposer: two Escs on an idle composer open claude's
// Rewind picker, whose "❯ (current)" row is not a composer.
func TestRewindPickerIsNoComposer(t *testing.T) {
	var picker screen.Snapshot
	for _, snap := range frames(t, "rewind-picker") {
		if strings.Contains(snap.Text, "Esc to cancel") {
			picker = snap
			break
		}
	}
	if picker.Text == "" {
		t.Fatal("no frame showed the Rewind picker; the recording changed")
	}
	a := New()
	if text, ok := a.ComposerText(picker); ok {
		t.Fatalf("ComposerText = %q, true on the Rewind picker", text)
	}
	if outcome, _ := a.InterruptOutcome("HW_QUICK say hi", picker); outcome != turns.InterruptPending {
		t.Fatalf("reading = %v on the Rewind picker, want pending", outcome)
	}
}

// box paints a claude frame: conversation rows, the composer box around
// composer rows, and the idle footer.
func box(convo []string, composer ...string) screen.Snapshot {
	rule := strings.Repeat("─", 100)
	lines := append(append([]string{}, convo...), rule)
	lines = append(lines, composer...)
	lines = append(lines, rule, "  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents")
	return screen.Snapshot{Text: strings.Join(lines, "\n")}
}

const marker = "  " + interruptMarker

func TestInterruptOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snap     screen.Snapshot
		want     turns.InterruptOutcome
		wantText string
	}{
		{
			"stopped below this turn's echo",
			box([]string{"❯ this prompt", "⏺ half a", "  reply", marker}, "❯ "),
			turns.InterruptStopped, "half a\nreply",
		},
		{
			"an earlier turn's marker above this turn's reply",
			box([]string{"❯ earlier", "⏺ cut", marker, "", "❯ this prompt", "⏺ the reply", "", "✻ Baked for 3s"}, "❯ "),
			turns.InterruptFinished, "",
		},
		{
			"an earlier turn's marker while this turn runs",
			box([]string{"❯ earlier", "⏺ cut", marker, "", "❯ this prompt", "⏺ the reply so far"}, "❯ "),
			turns.InterruptPending, "",
		},
		{
			"a reply quoting the marker, then the summary",
			box([]string{"❯ this prompt", "⏺ claude paints", marker, "  below a stopped reply", "", "✻ Baked for 3s"}, "❯ "),
			turns.InterruptFinished, "",
		},
		{
			"a stopped turn whose echo scrolled off",
			box([]string{"  of a long reply", "  cut short", marker}, "❯ "),
			turns.InterruptStopped, "",
		},
		{
			"the last echo is another prompt's",
			box([]string{"❯ earlier", "⏺ cut", marker}, "❯ "),
			turns.InterruptPending, "",
		},
		{
			"the prompt back in the composer",
			box([]string{"❯ earlier", "⏺ cut", marker}, "❯ this prompt"),
			turns.InterruptCancelled, "",
		},
		{
			"a multi-line prompt back in the composer",
			box(nil, "❯ line one of this prompt", "  line two"),
			turns.InterruptCancelled, "",
		},
		{
			"a short word someone typed that the prompt contains",
			box([]string{"❯ earlier", "⏺ cut", marker}, "❯ prompt"),
			turns.InterruptPending, "",
		},
		{
			"the marker while claude still works",
			screen.Snapshot{Text: box([]string{"❯ this prompt", "⏺ half", marker}, "❯ ").Text + " · esc to interrupt"},
			turns.InterruptPending, "",
		},
		{
			"no composer on screen",
			screen.Snapshot{Text: "❯ this prompt\n⏺ half\n" + marker},
			turns.InterruptPending, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := "this prompt"
			if strings.Contains(tc.name, "multi-line") {
				prompt = "line one of this prompt\nline two"
			}
			got, text := New().InterruptOutcome(prompt, tc.snap)
			if got != tc.want || text != tc.wantText {
				t.Fatalf("InterruptOutcome = %v %q, want %v %q\n%s", got, text, tc.want, tc.wantText, tc.snap.Text)
			}
		})
	}
}

// A long prompt put back fills the composer past the top of the screen, as a
// 42-line paste did on 2.1.280: the rows show only its tail, and read as it.
func TestInterruptOutcome_OverflowingComposer(t *testing.T) {
	var prompt strings.Builder
	prompt.WriteString("HW_DELAY big\n")
	var rows []string
	for i := range 50 {
		line := "filler line " + strings.Repeat("x", i%7) + " of the paste"
		prompt.WriteString(line + "\n")
		if i >= 12 {
			rows = append(rows, "  "+line)
		}
	}
	rows = append(rows, strings.Repeat("─", 100), "  paste again to expand")
	snap := screen.Snapshot{Text: strings.Join(rows, "\n")}
	a := New()
	if text, ok := a.ComposerText(snap); !ok || !strings.HasPrefix(text, "filler line") {
		t.Fatalf("ComposerText = %q, %v; want the visible tail", text, ok)
	}
	if outcome, _ := a.InterruptOutcome(prompt.String(), snap); outcome != turns.InterruptCancelled {
		t.Fatalf("reading = %v, want cancelled", outcome)
	}
}

func TestComposerText(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap screen.Snapshot
		text string
		ok   bool
	}{
		{"empty", box([]string{"⏺ hi"}, "❯ "), "", true},
		{"one line", box(nil, "❯ a draft"), "a draft", true},
		{"lines", box(nil, "❯ one", "  two", "  three"), "one\ntwo\nthree", true},
		{"no rules", screen.Snapshot{Text: "❯ a draft"}, "", false},
		{"a box that is not the composer", box(nil, "  Rewind", "  ❯ (current)"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if text, ok := New().ComposerText(tc.snap); text != tc.text || ok != tc.ok {
				t.Fatalf("ComposerText = %q, %v; want %q, %v", text, ok, tc.text, tc.ok)
			}
		})
	}
}

func TestClearComposerSequence(t *testing.T) {
	for n, composer := range []string{"", "one", "one\ntwo", "one\ntwo\nthree"} {
		lines := max(n, 1)
		got := string(New().ClearComposerSequence(composer))
		want := "\x05" + strings.Repeat("\x0b", 2*lines) + strings.Repeat("\x15", 2*lines+1)
		if got != want {
			t.Fatalf("ClearComposerSequence(%q) = %q, want %q", composer, got, want)
		}
	}
}

// The fake harness waits for exactly the bytes this adapter writes, and paints
// exactly the marker it reads.
func TestInterruptKeysMatchFakeharness(t *testing.T) {
	a := New()
	if got := string(a.InterruptSequence()); got != fakeharness.InterruptCSI27u {
		t.Fatalf("InterruptSequence = %q, fakeharness waits for %q", got, fakeharness.InterruptCSI27u)
	}
	for n := 1; n <= 4; n++ {
		composer := strings.Repeat("line\n", n-1) + "line"
		if got := string(a.ClearComposerSequence(composer)); got != fakeharness.ClearComposerKeys(n) {
			t.Fatalf("ClearComposerSequence(%d lines) = %q, fakeharness waits for %q", n, got, fakeharness.ClearComposerKeys(n))
		}
	}
	if got := strings.TrimSpace(fakeharness.InterruptMarkerLine()); got != interruptMarker {
		t.Fatalf("fakeharness paints %q, the adapter reads %q", got, interruptMarker)
	}
}
