package claudecodetui

import (
	"time"

	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	tclaude "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// Record is the Claude Code profile's record — claude's transcript and the
// Session's hook spool — with one difference: a prompt entry's uuid is
// claude's own, not the input's native id, so the entry is matched to its
// input by its prompt id, which the UserPromptSubmit hook bound to the input
// (live.Bound) before claude wrote the entry. And the TUI may write a reply
// before its prompt, which the record waits for (promptLagFor).
func (Profile) Record(src adapter.RecordSource) (adapter.Reader, error) {
	dir := ""
	if cfg, err := claudecode.ParseOpenConfig(src.OpenConfig); err == nil && sessionid.IsUUID(src.SessionID) {
		dir = live.Dir(cfg.Spool, src.SessionID)
	}
	return claudecode.NewRecord(src, claudecode.RecordOptions{
		Native: func(e *tclaude.Entry) string {
			if e.PromptID == "" || dir == "" {
				return ""
			}
			n, _ := live.Bound(dir, e.PromptID)
			return n
		},
		PromptLag: promptLagFor,
	})
}

// promptLagFor is how long the record waits for a prompt claude writes after its
// reply: claude's TUI writes a fresh session's first prompt to the
// transcript after the reply, now and then, within a few hundred
// milliseconds of it (claude 2.1.283).
var promptLagFor = 3 * time.Second
