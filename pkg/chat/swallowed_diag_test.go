package chat

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// The three shapes behind "no assistant output was recoverable" used to share
// one diag line, so the operator reading a failed turn could not tell a harness
// that never persisted anything (nothing COULD be recovered) from a rollout
// that genuinely held no reply (nothing WAS). The 2026-09-18 release-check
// artefact was the first kind and had to be diagnosed by reading source.
// (PUPPET-671)

// transcriptOffScreen is the settled screen of a nested claude: the footer
// banner, width-truncated exactly as the captured artefact truncated it.
const transcriptOffScreen = "⚠ Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION marker\n" +
	"  · restart with CLAUDE_CODE_FORCE_SESSION_PE…\n❯ \n"

func TestSwallowedDiag_PersistenceOffNamesTheCause(t *testing.T) {
	// No rollout at all — the shape a non-persisting harness leaves behind.
	c := convWithTranscript(t, "sess-off", userLine("go"))
	c.store = newTestStore()
	c.eventCh = make(chan ConversationEvent, 4)

	turn := &Turn{ID: "t-off", State: TurnStatePending}
	c.applySwallowedPromptVerdict(turn, screen.Snapshot{Text: transcriptOffScreen})

	if turn.State != TurnStateErrored {
		t.Fatalf("State = %q, want errored — naming the cause must not rescue the turn", turn.State)
	}
	if !strings.Contains(turn.Reason, DiagTranscriptUnavailable) {
		t.Errorf("Reason = %q, want it to carry DiagTranscriptUnavailable", turn.Reason)
	}
	// The codes are a machine-matchable contract; this change only adds prose.
	if turn.Code != "" {
		t.Errorf("Code = %q, want it untouched — no new TurnCode was introduced", turn.Code)
	}
}

func TestSwallowedDiag_OrdinaryEmptyRolloutKeepsItsText(t *testing.T) {
	c := convWithTranscript(t, "sess-empty", userLine("go"))
	c.store = newTestStore()
	c.eventCh = make(chan ConversationEvent, 4)

	turn := &Turn{ID: "t-empty", State: TurnStatePending}
	c.applySwallowedPromptVerdict(turn, screen.Snapshot{Text: "❯ \n"})

	if turn.State != TurnStateErrored {
		t.Fatalf("State = %q, want errored", turn.State)
	}
	if strings.Contains(turn.Reason, DiagTranscriptUnavailable) {
		t.Errorf("Reason = %q, want the ordinary empty-rollout diag on a screen with no banner", turn.Reason)
	}
	if !strings.Contains(turn.Reason, "transcript has no assistant output") {
		t.Errorf("Reason = %q, want the unchanged empty-rollout wording", turn.Reason)
	}
}

// TestSwallowedDiag_RescueStillWins is the regression guard on the ORDER: the
// banner only ever LABELS a miss, so a rollout that holds an assistant reply
// still overturns the screen verdict — with or without the banner on screen.
func TestSwallowedDiag_RescueStillWins(t *testing.T) {
	c := convWithTranscript(t, "sess-rescue", userLine("go"), replyLine("Done."))
	c.store = newTestStore()
	c.eventCh = make(chan ConversationEvent, 4)

	turn := &Turn{ID: "t-rescue", State: TurnStatePending}
	c.applySwallowedPromptVerdict(turn, screen.Snapshot{Text: "❯ \n"})

	if turn.State != TurnStateComplete {
		t.Fatalf("State = %q, want complete — a rollout with assistant text still overturns the screen", turn.State)
	}
	if turn.Text != "Done." {
		t.Errorf("Text = %q, want the transcript reply", turn.Text)
	}

	// ...and the banner does not veto a rollout that really does hold a reply.
	b := convWithTranscript(t, "sess-rescue-banner", userLine("go"), replyLine("Done."))
	b.store = newTestStore()
	b.eventCh = make(chan ConversationEvent, 4)
	banner := &Turn{ID: "t-rescue-banner", State: TurnStatePending}
	b.applySwallowedPromptVerdict(banner, screen.Snapshot{Text: transcriptOffScreen})
	if banner.State != TurnStateComplete {
		t.Errorf("State = %q with the banner on screen, want complete — the banner labels a miss, it does not veto a proof", banner.State)
	}
}

// TestTranscriptSavingOff_NeedsTheBanner pins the detector's narrowness: it
// matches the truncated footer row and nothing that merely mentions claude.
func TestTranscriptSavingOff_NeedsTheBanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want bool
	}{
		{"the truncated banner", transcriptOffScreen, true},
		{"an ordinary ready screen", "❯ \n", false},
		{"a reply mentioning the marker env var", "⏺ Set CLAUDE_CODE_CHILD_SESSION=1 to nest.\n❯ \n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := transcriptSavingOff(tt.text); got != tt.want {
				t.Errorf("transcriptSavingOff() = %v, want %v", got, tt.want)
			}
		})
	}
}
