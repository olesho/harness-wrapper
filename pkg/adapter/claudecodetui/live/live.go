// Package live is the Claude Code TUI profile's live hook channel: what the
// hooks claude fires report to the transport while it runs, and the binding
// of each prompt claude took to the input it was typed for.
//
// The hook helper (cmd/claude-code-hook, run as `claude-code-hook tui <arg>`)
// calls HandleHook; the transport reads what it wrote. Everything lives in
// one directory per Session (Dir), named to the helper by EnvDir:
//
//	events/   one file per hook fired, written atomically, which the
//	          transport reads in order and removes
//	prompts/  one file per prompt claude reported: the prompt's id (claude's
//	          promptId) names it, and it holds the native id of the input it
//	          was bound to, or nothing for a prompt of claude's own (a task
//	          notification, say); the record reader matches the
//	          transcript's prompt entries by it
//	pending   the input the transport is typing, which the UserPromptSubmit
//	          hook binds to the prompt claude reports
//
// The UserPromptSubmit hook binds the prompt before claude writes the prompt
// to its transcript: claude waits for the hook before it starts the turn
// (claude 2.1.283), so a record that holds the prompt always finds it bound.
package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// EnvDir names the variable that gives the hook helper the Session's live
// directory; the helper is inert without it.
const EnvDir = "HW_CLAUDE_TUI_DIR"

// Harness is the helper's first argument for the live hooks.
const Harness = "tui"

// The hooks the transport reads, and the argument each runs the helper with.
const (
	HookSessionStart = "SessionStart"
	HookUserPrompt   = "UserPromptSubmit"
	HookStop         = "Stop"
	HookStopFailure  = "StopFailure"
)

// Hooks maps each hook the transport reads to the helper's argument for it.
var Hooks = []struct{ Native, Arg string }{
	{HookSessionStart, "session-start"},
	{HookUserPrompt, "user-prompt-submit"},
	{HookStop, "stop"},
	{HookStopFailure, "stop-failure"},
}

// taskNotification begins the prompt of a turn claude starts itself to take
// up background work that ended: never an input's.
const taskNotification = "<task-notification>"

// Event is what one hook reported.
type Event struct {
	// Hook is the hook's name (hook_event_name).
	Hook      string `json:"hook"`
	SessionID string `json:"session_id,omitempty"`
	// PromptID is the turn's prompt id: absent before the first prompt. On
	// a prompt claude queued behind a running turn it is the running turn's
	// (claude 2.1.283), and Queued says so.
	PromptID string `json:"prompt_id,omitempty"`
	// Prompt is UserPromptSubmit's prompt text.
	Prompt string `json:"prompt,omitempty"`
	// Source is SessionStart's: startup, resume, ….
	Source string `json:"source,omitempty"`
	// Message is Stop's and StopFailure's last_assistant_message.
	Message string `json:"message,omitempty"`
	// Error is StopFailure's error tag: server_error, rate_limit, ….
	Error string `json:"error,omitempty"`
	// Native is the input UserPromptSubmit bound the prompt to, "" when it
	// bound none.
	Native string `json:"native,omitempty"`
	Queued bool   `json:"queued,omitempty"`
	// Background is the work claude runs in the background, as Stop and
	// StopFailure list it (background_tasks); Listed says the hook listed
	// it, even as none.
	Background []Task    `json:"background,omitempty"`
	Listed     bool      `json:"listed,omitempty"`
	At         time.Time `json:"at"`

	// Name is the event's file, for Remove.
	Name string `json:"-"`
}

// Task is one piece of work claude runs in the background, as a hook lists
// it (background_tasks).
type Task struct {
	ID string `json:"id"`
	// Type is claude's: shell, subagent, ….
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

// maxTasks bounds the background work an event keeps.
const maxTasks = 256

// TaskNotification reports whether prompt is one claude writes itself to
// take up background work that ended, and the task it names first ("" for
// none).
func TaskNotification(prompt string) (bool, string) {
	p := strings.TrimSpace(prompt)
	if !strings.HasPrefix(p, taskNotification) {
		return false, ""
	}
	_, rest, ok := strings.Cut(p, "<task-id>")
	if !ok {
		return true, ""
	}
	id, _, ok := strings.Cut(rest, "</task-id>")
	if !ok {
		return true, ""
	}
	return true, strings.TrimSpace(id)
}

// Dir is Session id's live directory under the agent's spool root.
func Dir(spoolRoot, id string) string { return filepath.Join(spoolRoot, "tui", id) }

const (
	eventsDir   = "events"
	promptsDir  = "prompts"
	pendingFile = "pending.json"
)

// validID keeps an id claude reported to a plain file name.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// pending is the input being typed.
type pending struct {
	Native string `json:"native"`
}

// Prepare makes dir ready for a launch: its directories made, and the events
// and the pending input of an earlier launch gone. Bindings stay: the record
// is read by them.
func Prepare(dir string) error {
	for _, d := range []string{eventsDir, promptsDir} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return err
		}
	}
	if err := ClearPending(dir); err != nil {
		return err
	}
	ents, err := os.ReadDir(filepath.Join(dir, eventsDir))
	if err != nil {
		return err
	}
	for _, e := range ents {
		_ = os.Remove(filepath.Join(dir, eventsDir, e.Name()))
	}
	return nil
}

// SetPending records, durably, that the input native is being typed: the
// next prompt claude takes is bound to it.
func SetPending(dir, native string) error {
	b, err := json.Marshal(pending{Native: native})
	if err != nil {
		return err
	}
	return writeAtomic(dir, pendingFile, b)
}

// ClearPending withdraws the input being typed.
func ClearPending(dir string) error {
	err := os.Remove(filepath.Join(dir, pendingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Bound is the native id of the input the prompt promptID was bound to.
func Bound(dir, promptID string) (string, bool) {
	if !validID.MatchString(promptID) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, promptsDir, promptID)) //nolint:gosec // a plain name in the Session's own directory
	if err != nil {
		return "", false
	}
	n := strings.TrimSpace(string(b))
	return n, n != ""
}

// HandleHook is the hook helper's work for a live hook: the payload claude
// handed the hook, read as an Event, the prompt bound when it is the input's,
// and the event written to the Session's directory. It does nothing outside a
// TUI Session (EnvDir unset).
func HandleHook(arg string, env []string, payload []byte) error {
	dir := lookup(env, EnvDir)
	if dir == "" {
		return nil
	}
	var p struct {
		Hook      string `json:"hook_event_name"`
		SessionID string `json:"session_id"`
		PromptID  string `json:"prompt_id"`
		Prompt    string `json:"prompt"`
		Source    string `json:"source"`
		Message   string `json:"last_assistant_message"`
		Error     string `json:"error"`
		// Background stays raw, so its shape never costs the event.
		Background json.RawMessage `json:"background_tasks"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("live hook %s: %w", arg, err)
	}
	ev := Event{
		Hook: p.Hook, SessionID: p.SessionID, PromptID: p.PromptID, Prompt: p.Prompt, Source: p.Source,
		Message: p.Message, Error: p.Error, At: time.Now().UTC(),
	}
	if ev.Hook == HookStop || ev.Hook == HookStopFailure {
		ev.Background, ev.Listed = tasks(p.Background)
	}
	if ev.Hook == HookUserPrompt {
		if err := bind(dir, &ev); err != nil {
			return err
		}
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if !validID.MatchString(arg) {
		return fmt.Errorf("live hook: %q is not a hook argument", arg)
	}
	name := fmt.Sprintf("%020d-%s-%d-%d.json", time.Now().UnixNano(), arg, os.Getpid(), seq.Add(1))
	return writeAtomic(filepath.Join(dir, eventsDir), name, b)
}

var seq atomic.Uint64

// tasks reads a hook's background_tasks: whether it listed them, and those
// with an id.
func tasks(raw json.RawMessage) ([]Task, bool) {
	var list []Task
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil {
		return nil, false
	}
	out := make([]Task, 0, min(len(list), maxTasks))
	for _, t := range list {
		if t.ID != "" && len(out) < maxTasks {
			out = append(out, t)
		}
	}
	return out, true
}

// bind binds the prompt ev reports to the input being typed. A prompt claude
// queued behind a running turn reports the running turn's prompt id, which
// is known already: it binds nothing. A prompt of claude's own (a task
// notification), or one with no input being typed, is not an input's: it is
// recorded as claude's own, so that a prompt queued behind its turn is not
// taken for an input's either.
func bind(dir string, ev *Event) error {
	if !validID.MatchString(ev.PromptID) {
		return nil
	}
	if known(dir, ev.PromptID) {
		ev.Queued = true
		return nil
	}
	own := func() error { return writeAtomic(filepath.Join(dir, promptsDir), ev.PromptID, nil) }
	if ok, _ := TaskNotification(ev.Prompt); ok {
		return own()
	}
	b, err := os.ReadFile(filepath.Join(dir, pendingFile)) //nolint:gosec // the Session's own directory
	if errors.Is(err, fs.ErrNotExist) {
		return own()
	}
	if err != nil {
		return err
	}
	var p pending
	if json.Unmarshal(b, &p) != nil || p.Native == "" {
		return own()
	}
	if err := writeAtomic(filepath.Join(dir, promptsDir), ev.PromptID, []byte(p.Native+"\n")); err != nil {
		return err
	}
	ev.Native = p.Native
	return ClearPending(dir)
}

// known reports whether the prompt promptID was reported before.
func known(dir, promptID string) bool {
	_, err := os.Stat(filepath.Join(dir, promptsDir, promptID))
	return err == nil
}

// Read returns the events written so far, oldest first.
func Read(dir string) ([]Event, error) {
	d := filepath.Join(dir, eventsDir)
	ents, err := os.ReadDir(d)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if n := e.Name(); e.Type().IsRegular() && strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	var out []Event
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(d, n)) //nolint:gosec // the Session's own directory
		if err != nil {
			continue
		}
		var ev Event
		if json.Unmarshal(b, &ev) != nil {
			_ = os.Remove(filepath.Join(d, n))
			continue
		}
		ev.Name = n
		out = append(out, ev)
	}
	return out, nil
}

// Remove removes an event Read returned, once the transport has taken it.
func Remove(dir string, ev Event) {
	if ev.Name != "" && !strings.ContainsAny(ev.Name, `/\`) {
		_ = os.Remove(filepath.Join(dir, eventsDir, ev.Name))
	}
}

// writeAtomic writes name in dir durably: a synced temporary file renamed
// into place, and the directory synced.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	d, err := os.Open(dir) //nolint:gosec // the Session's own directory
	if err != nil {
		return err
	}
	_ = d.Sync()
	return d.Close()
}

func lookup(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
