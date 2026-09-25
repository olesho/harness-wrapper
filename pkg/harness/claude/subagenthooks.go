package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// claude's subagent hooks (ADR-011). SubagentStart fires when a subagent
// starts, with its agent_id and agent_type; SubagentStop fires when it has
// finished, adding the path of its transcript (agent_transcript_path) and its
// last reply (last_assistant_message). Both carry the parent's session_id and
// transcript_path. claude added SubagentStart in 2.0.43; SubagentStop's
// agent_type and last_assistant_message came later, and every field here is
// in 2.1.250 through 2.1.282 (measured live on 2.1.281 and 2.1.282).
//
// SubagentStop fires when the subagent has finished, whether its tool call
// waited for it or launched it in the background. A background subagent's
// PostToolUse (post-task) fires when it is launched, before it has done
// anything, so SubagentStop is the only hook that reads its transcript.
type subagentHookPayload struct {
	AgentID              string `json:"agent_id"`
	AgentType            string `json:"agent_type"`
	AgentTranscriptPath  string `json:"agent_transcript_path"`  // SubagentStop
	LastAssistantMessage string `json:"last_assistant_message"` // SubagentStop
}

// maxAgentTypeBytes bounds the subagent type a start or stop event carries.
const maxAgentTypeBytes = 256

// lastReplyWait bounds how long subagent-stop waits for claude to write the
// subagent's last reply to its transcript (readSubagentStop), looking again
// every lastReplyPoll.
var (
	lastReplyWait = time.Second
	lastReplyPoll = 20 * time.Millisecond
)

// readSubagentHook turns SubagentStart into the subagent's start marker, and
// SubagentStop into the subagent's transcript — read from the path claude
// hands over once it is validated (readSubagentStop), tagged as post-task
// tags it — followed by its stop marker. A marker is a SourceHook event
// tagged with the subagent's session under its parent's, stamped when the
// hook fired; the stop's carries the subagent's last reply, cut to
// harness.MaxToolHookBytes. Its native id, hook:<arg>:<agent_id>, is the same
// on every replay of the hook, so a replay dedups. The stop marker is
// recorded even when the transcript is not on disk: the subagent finished
// either way.
func readSubagentHook(ctx harness.HookContext, event string, p claudeHookPayload, stdin []byte) ([]transcript.ParsedEvent, error) {
	var s subagentHookPayload
	if err := json.Unmarshal(stdin, &s); err != nil {
		return nil, fmt.Errorf("claude hook %q: parse subagent payload: %w", event, err)
	}
	if !validSubagentID(s.AgentID) {
		return nil, fmt.Errorf("claude hook %q: invalid subagent id %q", event, s.AgentID)
	}
	typ, text := transcript.EventSubagentStart, ""
	var out []transcript.ParsedEvent
	if event == argSubagentStop {
		typ, text = transcript.EventSubagentStop, boundToolText(s.LastAssistantMessage)
		if s.AgentTranscriptPath != "" {
			var err error
			if out, err = readSubagentStop(ctx, p.SessionID, s, time.Now().Add(lastReplyWait)); err != nil {
				return nil, err
			}
		}
	}
	return append(out, transcript.ParsedEvent{
		HarnessSessionID: s.AgentID,
		ParentSessionID:  p.SessionID,
		Event: transcript.Event{
			Timestamp: time.Now().UTC(),
			Role:      transcript.RoleSystem,
			Type:      typ,
			Text:      text,
			AgentType: capBytes(s.AgentType, maxAgentTypeBytes),
			Source:    transcript.SourceHook,
			NativeID:  "hook:" + event + ":" + s.AgentID,
		},
	}), nil
}

// readSubagentStop reads the subagent's transcript at SubagentStop. claude
// writes a record to the transcript a moment after it makes it, and
// SubagentStop can fire in between: measured on 2.1.281 and 2.1.282, the
// subagent's last reply reached the disk up to a tenth of a second after the
// hook fired, whether its tool call waited for it or not. So while the file
// does not end with the reply claude handed over, it reads the file again
// each time it has grown, until deadline, and keeps what it read last.
func readSubagentStop(ctx harness.HookContext, parentSessionID string, s subagentHookPayload, deadline time.Time) ([]transcript.ParsedEvent, error) {
	reply := strings.TrimSpace(s.LastAssistantMessage)
	var out []transcript.ParsedEvent
	size := -1
	for {
		data, err := readSubagentFile(ctx, parentSessionID, s.AgentID, s.AgentTranscriptPath)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			size = len(data)
			if out, err = subagentEvents(data, s.AgentID, parentSessionID); err != nil {
				return nil, fmt.Errorf("claude hook %s: %w", argSubagentStop, err)
			}
			if reply == "" || endsWithReply(out, reply) {
				return out, nil
			}
		}
		if !time.Now().Before(deadline) {
			return out, nil
		}
		time.Sleep(lastReplyPoll)
	}
}

// endsWithReply reports whether the last assistant entry among evs holds
// reply. claude builds last_assistant_message from the last assistant entry,
// joining the text of its blocks with newlines and trimming the result; each
// block is an event here, and the entry's events share its UUID.
func endsWithReply(evs []transcript.ParsedEvent, reply string) bool {
	var parts []string
	entry := ""
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i].Event
		if e.Role != transcript.RoleAssistant || e.Type != transcript.EventText {
			continue
		}
		if entry != "" && e.UUID != entry {
			break
		}
		entry = e.UUID
		parts = append(parts, e.Text)
	}
	slices.Reverse(parts)
	return entry != "" && strings.TrimSpace(strings.Join(parts, "\n")) == reply
}

// readSubagentFile reads the subagent transcript at path (resolved against
// the run's working dir, as every handed-over path is), or returns nil when
// it is not on disk. The path must name agent-<agentID>.jsonl in a
// subagents directory of the parent session, in one of claude's two layouts
// (subagentTranscriptPaths):
//
//	<projects>/<encoded-cwd>/<session>/subagents/agent-<id>.jsonl
//	<projects>/<encoded-cwd>/subagents/agent-<id>.jsonl
//
// Anything else is refused. The file is opened through an os.Root at the
// transcript root, so a symlink cannot lead the read outside it.
func readSubagentFile(ctx harness.HookContext, parentSessionID, agentID, path string) ([]byte, error) {
	root := transcriptRoot(ctx)
	rel, err := subagentTranscriptRel(root, parentSessionID, agentID, transcript.ResolveHarnessPath(path, ctx.Cwd))
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // no transcript root yet — best-effort, like post-task
	}
	if err != nil {
		return nil, fmt.Errorf("claude hook %s: open transcript root: %w", argSubagentStop, err)
	}
	defer func() { _ = r.Close() }()
	data, err := r.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claude hook %s: read subagent transcript: %w", argSubagentStop, err)
	}
	return data, nil
}

// subagentTranscriptRel checks path's shape (see readSubagentFile) and
// returns it relative to root.
func subagentTranscriptRel(root, parentSessionID, agentID, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("claude hook %s: empty agent_transcript_path", argSubagentStop)
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("claude hook %s: agent_transcript_path %q not under transcript root %q", argSubagentStop, clean, root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	want := "agent-" + agentID + ".jsonl"
	switch {
	case len(parts) == 4 && parts[1] == parentSessionID && parts[2] == "subagents" && parts[3] == want:
	case len(parts) == 3 && parts[1] == "subagents" && parts[2] == want:
	default:
		return "", fmt.Errorf("claude hook %s: agent_transcript_path %q is not %s in a subagents dir of session %q",
			argSubagentStop, clean, want, parentSessionID)
	}
	return rel, nil
}

// transcriptRoot is claude's transcript root: <config dir>/projects, the
// config dir being HW_HARNESS_CONFIG_DIR (resolved against the run's working
// dir) or <home>/.claude.
func transcriptRoot(ctx harness.HookContext) string {
	configDir := transcript.ResolveHarnessPath(ctx.ConfigDir, ctx.Cwd)
	if configDir == "" {
		configDir = filepath.Join(ctx.Home, ".claude")
	}
	return filepath.Join(configDir, "projects")
}

// capBytes cuts s to at most n bytes at a rune boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
