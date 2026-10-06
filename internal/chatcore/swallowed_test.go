package chatcore

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// TestTranscriptProof_IgnoresRepliesBeforeTheWatermark covers the stale-rescue
// bug: from the second turn on the transcript always holds an earlier reply,
// and accepting it as proof turned a swallowed prompt into a false success.
// Only entries at or beyond the watermark Send captured may prove this turn.
func TestTranscriptProof_IgnoresRepliesBeforeTheWatermark(t *testing.T) {
	lines := []string{userLine("first"), replyLine("first reply")}

	cases := []struct {
		name      string
		extra     []string
		watermark int
		wantProof string
		wantDiag  string
	}{
		{
			name:      "previous turn's reply only",
			watermark: 2,
			wantDiag:  "no assistant output",
		},
		{
			name:      "reply to the current prompt",
			extra:     []string{userLine("second"), replyLine("second reply")},
			watermark: 2,
			wantProof: "second reply",
		},
		{
			name:      "watermark unknown",
			extra:     []string{userLine("second"), replyLine("second reply")},
			watermark: watermarkUnknown,
			wantDiag:  "unknown",
		},
		{
			name:      "watermark beyond a shrunken transcript",
			watermark: 9,
			wantDiag:  "no assistant output",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := convWithTranscript(t, "sess-wm", append(append([]string{}, lines...), tc.extra...)...)
			c.sentTranscriptWatermark = tc.watermark
			v := c.transcriptProofOfCurrentTurn(screen.Snapshot{Text: "❯ \n"})
			if v.proofText != tc.wantProof {
				t.Fatalf("proofText = %q, want %q (diag %q)", v.proofText, tc.wantProof, v.diag)
			}
			if tc.wantDiag != "" && !strings.Contains(v.diag, tc.wantDiag) {
				t.Errorf("diag = %q, want it to mention %q", v.diag, tc.wantDiag)
			}
		})
	}
}
