package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// codex keeps two things of a thread outside its rollout: its name, in
// session_index.jsonl, and its goal, in goals_1.sqlite and that database's
// write-ahead log. A thread copied without them resumes with its whole
// conversation and neither, and codex says nothing: a goal whose row is still
// in the log, copied without it, is simply gone. The rollout cannot tell,
// either: it logs a goal a client set, and not one the model made, changed
// or finished.
//
// So the profile keeps what codex last said of them in a file of its own —
// the thread's native state — beneath the scratch root, and names that file
// history: it is saved with the thread. When a loaded thread is first opened
// in its new environment, the profile asks codex what it has of the thread
// there, before the thread resumes, and refuses the open when that is not
// what was saved.

// Where the native state is kept, and the files of codex's it vouches for,
// each relative to its root.
const (
	nativeDir    = "native" // beneath scratch: <thread>.json
	indexFile    = "session_index.jsonl"
	goalsFile    = "goals_1.sqlite"
	goalsLogFile = "goals_1.sqlite-wal"
)

// goalActive is the status of a goal codex works on by itself.
const goalActive = "active"

// nativeState is what codex keeps of a thread outside its rollout.
type nativeState struct {
	Thread string `json:"thread"`
	Name   string `json:"name,omitempty"`
	Goal   *goal  `json:"goal,omitempty"`
	// Home is the CODEX_HOME codex said so in: a thread whose state names
	// another has not been opened here yet.
	Home string `json:"home"`
}

// goal is a thread's goal: what it is, and where it stands (active, paused,
// blocked, usageLimited, budgetLimited, complete).
type goal struct {
	Objective string `json:"objective"`
	Status    string `json:"status"`
}

func (g *goal) active() bool { return g != nil && g.Status == goalActive }

func sameGoal(a, b *goal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// same reports whether codex holds of the thread what s says: its name and
// its goal.
func (s nativeState) same(o nativeState) bool { return s.Name == o.Name && sameGoal(s.Goal, o.Goal) }

func (s nativeState) String() string {
	g := "no goal"
	if s.Goal != nil {
		g = fmt.Sprintf("goal %q (%s)", s.Goal.Objective, s.Goal.Status)
	}
	if s.Name == "" {
		return "no name, " + g
	}
	return fmt.Sprintf("name %q, %s", s.Name, g)
}

func nativePath(scratch, thread string) (string, error) {
	if !contract.ValidID(thread) {
		return "", fmt.Errorf("codex: %q is not a thread id", thread)
	}
	return filepath.Join(scratch, nativeDir, thread+".json"), nil
}

// readNative reads a thread's saved native state; fs.ErrNotExist when none
// was kept.
func readNative(scratch, thread string) (nativeState, error) {
	p, err := nativePath(scratch, thread)
	if err != nil {
		return nativeState{}, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // beneath the scratch root, named for a valid id
	if err != nil {
		return nativeState{}, err
	}
	var s nativeState
	if err := json.Unmarshal(b, &s); err != nil {
		return nativeState{}, fmt.Errorf("codex: the thread's native state: %w", err)
	}
	return s, nil
}

// writeNative keeps a thread's native state: written whole, synced, and moved
// into place, so a reader finds the old state or the new.
func writeNative(scratch string, s nativeState) error {
	p, err := nativePath(scratch, s.Thread)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".native-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// checkLoaded is what opening a loaded thread requires of its native state:
// got is what codex holds of the thread here, before it resumes. A thread
// already opened in this CODEX_HOME passes: the check is of the load, once.
func checkLoaded(scratch, home string, got nativeState) error {
	saved, err := readNative(scratch, got.Thread)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("thread %s was saved with no account of its name and goal: an adapter older than the one that loads it saved it, so what it lost cannot be told", got.Thread)
	case err != nil:
		return err
	case saved.Home == home:
		return nil
	case !saved.same(got):
		return fmt.Errorf("thread %s was saved with %s; codex holds %s here", got.Thread, saved, got)
	}
	return nil
}
