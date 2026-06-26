package main

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

// TestCleanReply_LabelsBySourceNotPresence is the regression for the misleading
// HARNESS_WRAPPER_RUN_DEBUG "reply source" label: the store fallback also
// returns a non-empty History, so cleanReply must key the source on
// res.HistorySource, not on len(History).
func TestCleanReply_LabelsBySourceNotPresence(t *testing.T) {
	transcriptTurns := []chat.Turn{
		{Role: chat.RoleUser, Text: "hi"},
		{Role: chat.RoleAssistant, Text: "full transcript reply\nline 2"},
	}
	// The store fallback path: History is non-empty (screen-derived turns) but
	// the source is the lossy store, NOT the transcript.
	storeTurns := []chat.Turn{
		{Role: chat.RoleUser, Text: "hi"},
		{Role: chat.RoleAssistant, Text: "truncated screen tail"},
	}

	tests := []struct {
		name string
		res  harness.TurnResult
		want string
	}{
		{
			name: "transcript-backed history returns transcript text",
			res: harness.TurnResult{
				History:       transcriptTurns,
				HistorySource: chat.HistorySourceTranscript,
				Turn:          chat.Turn{Text: "screen tail"},
			},
			want: "full transcript reply\nline 2",
		},
		{
			name: "store-backed history falls through to screen extract",
			res: harness.TurnResult{
				History:       storeTurns, // non-empty, but NOT authoritative
				HistorySource: chat.HistorySourceStore,
				Turn:          chat.Turn{Text: "clean screen reply"},
			},
			want: "clean screen reply",
		},
		{
			name: "store-backed with empty Turn.Text falls back to recorded assistant turn",
			res: harness.TurnResult{
				History:       storeTurns,
				HistorySource: chat.HistorySourceStore,
				Turn:          chat.Turn{Text: ""},
			},
			want: "truncated screen tail",
		},
		{
			name: "transcript source but no assistant text falls through to screen",
			res: harness.TurnResult{
				History:       []chat.Turn{{Role: chat.RoleUser, Text: "hi"}},
				HistorySource: chat.HistorySourceTranscript,
				Turn:          chat.Turn{Text: "screen reply"},
			},
			want: "screen reply",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanReply(tt.res); got != tt.want {
				t.Fatalf("cleanReply = %q, want %q", got, tt.want)
			}
		})
	}
}
