package live

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// At the gated posture claude's PermissionRequest hook asks the transport for
// each permission a tool needs, and blocks until the transport answers
// (probes/tui-hybrid, Phase 3: the hook's allow or deny decides). The hook
// helper (`claude-code-hook tui permission-request`) writes the ask as an
// event naming an id of its own, waits for answers/<id>, hands claude the
// decision it holds, and writes a HookDelivered event naming the ask. The
// transport answers each ask: the Session's answer, a withdrawal (the turn
// ended: the hook exits with no decision) or, for an ask on no input's turn,
// a denial.
//
// claude's TUI draws its own dialog while the hook blocks, and when the hook
// ends with no decision — it timed out, failed or was killed — that dialog
// waits for keystrokes. So the hook waits for as long as claude lets it
// (PermissionTimeout), with no deadline of its own: it outwaits any caller,
// and the dialog is never the answer path.

// The permission hook.
const (
	HookPermission = "PermissionRequest"
	// HookDelivered is not claude's: the helper writes it once it handed
	// claude the decision for an ask (Event.Ask names it).
	HookDelivered = "hw:permission-delivered"
	// PermissionArg is the helper's argument for HookPermission.
	PermissionArg = "permission-request"
	deliveredArg  = "permission-delivered"
)

// PermissionTimeout is the hook's timeout, in seconds, as claude's settings
// give it: 2 000 000 s, 23 days. claude counts a hook's timeout in
// milliseconds on a timer, which takes at most 2^31-1 ms (24.8 days); a longer
// timeout would overflow it and fire at once.
const PermissionTimeout = 2_000_000

// answerPoll is how often the hook looks for its answer.
const answerPoll = 25 * time.Millisecond

const answersDir = "answers"

// Ask is one permission claude asks for, as the hook reports it.
type Ask struct {
	// ID is the ask's, which the helper made: the answer's file name.
	ID          string          `json:"id"`
	Tool        string          `json:"tool,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Suggestions json.RawMessage `json:"suggestions,omitempty"`
	// PID is the helper's process, which waits for the answer.
	PID int `json:"pid,omitempty"`
}

// Answer is the transport's answer to an ask: claude's decision, or a
// withdrawal (Decision nil), which hands claude nothing.
type Answer struct {
	Decision json.RawMessage `json:"decision,omitempty"`
}

// WriteAnswer answers the ask id, durably.
func WriteAnswer(dir, id string, a Answer) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("live: %q is not an ask id", id)
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, answersDir), 0o700); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, answersDir), id, b)
}

// newAskID is a fresh ask id.
func newAskID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "ask-" + hex.EncodeToString(b[:])
}

// HandlePermission is the hook helper's work for PermissionRequest: the ask
// reported, then the transport's answer awaited for as long as it takes, and
// its decision written to out, claude's output from the hook. It ends with
// no decision when the answer withdraws the ask, and when the Session's
// directory is gone or the helper's parent with it. It does nothing outside
// a TUI Session (EnvDir unset).
func HandlePermission(env []string, payload []byte, out io.Writer) error {
	dir := lookup(env, EnvDir)
	if dir == "" {
		return nil
	}
	var p struct {
		SessionID   string          `json:"session_id"`
		PromptID    string          `json:"prompt_id"`
		Tool        string          `json:"tool_name"`
		Input       json.RawMessage `json:"tool_input"`
		Suggestions json.RawMessage `json:"permission_suggestions"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("live hook %s: %w", PermissionArg, err)
	}
	ask := &Ask{ID: newAskID(), Tool: p.Tool, Input: p.Input, Suggestions: p.Suggestions, PID: os.Getpid()}
	if err := writeEvent(dir, PermissionArg, Event{
		Hook: HookPermission, SessionID: p.SessionID, PromptID: p.PromptID, Ask: ask, At: time.Now().UTC(),
	}); err != nil {
		return err
	}
	a, ok := awaitAnswer(dir, ask.ID, os.Getppid())
	if !ok || len(a.Decision) == 0 {
		return nil
	}
	b, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": HookPermission, "decision": a.Decision,
	}})
	if err != nil {
		return err
	}
	if _, err := out.Write(append(b, '\n')); err != nil {
		return err
	}
	return writeEvent(dir, deliveredArg, Event{
		Hook: HookDelivered, SessionID: p.SessionID, PromptID: p.PromptID, Ask: &Ask{ID: ask.ID}, At: time.Now().UTC(),
	})
}

// awaitAnswer waits for the answer to ask id, with no deadline: until it
// comes, the Session's directory is gone, or the helper's parent, ppid, is.
func awaitAnswer(dir, id string, ppid int) (Answer, bool) {
	path := filepath.Join(dir, answersDir, id)
	for {
		b, err := os.ReadFile(path) //nolint:gosec // the Session's own directory
		if err == nil {
			_ = os.Remove(path)
			var a Answer
			if json.Unmarshal(b, &a) != nil {
				return Answer{}, false
			}
			return a, true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return Answer{}, false
		}
		if _, err := os.Stat(filepath.Join(dir, eventsDir)); err != nil {
			return Answer{}, false
		}
		if os.Getppid() != ppid {
			return Answer{}, false // the parent is gone: the helper was reparented
		}
		time.Sleep(answerPoll)
	}
}
