//go:build linux

package contain

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/containment"
	"golang.org/x/sys/unix"
)

// stateSchema versions lifecycle.json.
const stateSchema = 1

// Supervision and cleanup vocabulary recorded in lifecycle records and in the
// applied policy.
const (
	supervisionCgroup = containment.SupervisionCgroup
	supervisionNone   = containment.SupervisionNone
	cleanupComplete   = "complete"
)

// State is wrapper-managed private state: one session root, 0700, beneath a
// wrapper-controlled parent. The root itself is never granted to the harness;
// it holds lifecycle.json (the recorded cgroup of the last launch, which the
// next owner of the state kills before reusing or deleting it) and a lock
// file, and the granted directories — home/, tmp/ and the harness's own state
// root — are its children. Landlock rules bind to directory objects, so a
// descendant that survives one session can never reach another session's
// directories, even at a reused path.
type State struct {
	// ID names the state; it is what a stored conversation records.
	ID string
	// Persistent state outlives its launches (a stored chat conversation) and
	// is removed only by Remove.
	Persistent bool

	parent *pinned
	root   *pinned
	lockFD int
}

type lifecycleRecord struct {
	Schema     int           `json:"schema"`
	ID         string        `json:"id"`
	Persistent bool          `json:"persistent"`
	CreatedAt  time.Time     `json:"created_at"`
	Launch     *launchRecord `json:"launch,omitempty"`
}

type launchRecord struct {
	Supervision string     `json:"supervision"`
	Cgroup      string     `json:"cgroup,omitempty"`
	PID         int        `json:"pid,omitempty"`
	BootID      string     `json:"boot_id,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	Cleanup     string     `json:"cleanup,omitempty"`
}

// StateParent returns the directory beneath which managed state lives:
// $XDG_STATE_HOME/harness-wrapper/contain, defaulting to
// ~/.local/state/harness-wrapper/contain.
func StateParent() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate managed-state directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "harness-wrapper", "contain"), nil
}

// openStateParent creates (0700) and pins the managed-state parent, refusing
// one that another user owns or that others can write: a planted parent
// would let its owner reach every session's private state.
func openStateParent() (*pinned, error) {
	path, err := StateParent()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create managed-state directory: %w", err)
	}
	p, err := pinPath(path)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(p.fd, &st); err != nil {
		p.close()
		return nil, err
	}
	switch {
	case !p.isDir():
		p.close()
		return nil, fmt.Errorf("managed-state path %s is not a directory", p.canonical)
	case int(st.Uid) != os.Getuid():
		p.close()
		return nil, fmt.Errorf("managed-state directory %s is owned by uid %d, not %d", p.canonical, st.Uid, os.Getuid())
	case st.Mode&0o022 != 0:
		p.close()
		return nil, fmt.Errorf("managed-state directory %s is writable by other users (mode %#o)", p.canonical, st.Mode&0o777)
	}
	return p, nil
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b[:]), nil
}

func validID(id string) bool {
	if len(id) != 12 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !strings.ContainsRune(idAlphabet, rune(id[i])) {
			return false
		}
	}
	return true
}

// NewState allocates new managed state. Persistent state survives its
// launches until Remove; ephemeral state is deleted by the launch that
// created it once that launch's cgroup is empty.
func NewState(persistent bool) (*State, error) {
	parent, err := openStateParent()
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		id, err := newID()
		if err != nil {
			parent.close()
			return nil, err
		}
		if err := unix.Mkdirat(parent.fd, id, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			parent.close()
			return nil, fmt.Errorf("create managed state: %w", err)
		}
		root, err := pinAt(parent, id)
		if err != nil {
			parent.close()
			return nil, err
		}
		s := &State{ID: id, Persistent: persistent, parent: parent, root: root, lockFD: -1}
		// Locked from birth: a sweep for stale state skips anything it cannot
		// lock, and its creator keeps the lock until the state's first launch
		// ends (or it gives the state up).
		if err := s.lock(); err != nil {
			s.Close()
			return nil, err
		}
		rec := lifecycleRecord{Schema: stateSchema, ID: id, Persistent: persistent, CreatedAt: time.Now().UTC()}
		if err := s.writeLifecycle(&rec); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
	parent.close()
	return nil, errors.New("create managed state: could not allocate a unique id")
}

// OpenState opens existing managed state by id.
func OpenState(id string) (*State, error) {
	if !validID(id) {
		return nil, fmt.Errorf("managed state id %q is malformed", id)
	}
	parent, err := openStateParent()
	if err != nil {
		return nil, err
	}
	root, err := pinAt(parent, id)
	if err != nil {
		parent.close()
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrStateGone, id)
		}
		return nil, err
	}
	s := &State{ID: id, parent: parent, root: root, lockFD: -1}
	rec, err := s.readLifecycle()
	if err != nil {
		s.Close()
		return nil, err
	}
	if rec.Schema != stateSchema || rec.ID != id {
		s.Close()
		return nil, fmt.Errorf("managed state %s has an unsupported lifecycle record (schema %d)", id, rec.Schema)
	}
	s.Persistent = rec.Persistent
	return s, nil
}

// Root returns the session root's path (never granted to the harness).
func (s *State) Root() string { return s.root.canonical }

// errStateInUse: another launch, or removal, holds the state's lock.
var errStateInUse = errors.New("in use by another launch")

// lock takes the state's exclusive lock for the life of a launch (or a
// removal). A second owner is refused rather than queued: two launches into
// one state would share, and could tamper with, each other's files.
//
// The lock belongs to its open file description, and a spawn thread's
// private descriptor table refers to that description until the thread has
// exited, which can be just after Start returns.
func (s *State) lock() error {
	if s.lockFD >= 0 {
		return nil
	}
	fd, err := unix.Openat(s.root.fd, "lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("lock managed state %s: %w", s.ID, err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("managed state %s is %w", s.ID, errStateInUse)
		}
		return fmt.Errorf("lock managed state %s: %w", s.ID, err)
	}
	s.lockFD = fd
	return nil
}

func (s *State) unlock() {
	if s.lockFD >= 0 {
		_ = unix.Close(s.lockFD)
		s.lockFD = -1
	}
}

func (s *State) readLifecycle() (*lifecycleRecord, error) {
	fd, err := unix.Openat(s.root.fd, "lifecycle.json", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("read lifecycle of managed state %s: %w", s.ID, err)
	}
	f := os.NewFile(uintptr(fd), "lifecycle.json")
	defer func() { _ = f.Close() }()
	var rec lifecycleRecord
	if err := json.NewDecoder(f).Decode(&rec); err != nil {
		return nil, fmt.Errorf("read lifecycle of managed state %s: %w", s.ID, err)
	}
	return &rec, nil
}

// writeLifecycle replaces lifecycle.json atomically inside the session root.
func (s *State) writeLifecycle(rec *lifecycleRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := writeFileAt(s.root.fd, ".lifecycle.json.tmp", b, 0o600, true); err != nil {
		return fmt.Errorf("write lifecycle of managed state %s: %w", s.ID, err)
	}
	if err := unix.Renameat(s.root.fd, ".lifecycle.json.tmp", s.root.fd, "lifecycle.json"); err != nil {
		return fmt.Errorf("write lifecycle of managed state %s: %w", s.ID, err)
	}
	return nil
}

// recordLaunch writes the launch's supervision record before the harness
// starts, so a crash between spawn and teardown still leaves the next owner
// the cgroup to kill.
func (s *State) recordLaunch(l *launchRecord) error {
	rec, err := s.readLifecycle()
	if err != nil {
		return err
	}
	rec.Launch = l
	return s.writeLifecycle(rec)
}

// endLaunch marks the recorded launch finished, with its cleanup outcome.
func (s *State) endLaunch(cleanup string) {
	rec, err := s.readLifecycle()
	if err != nil || rec.Launch == nil {
		return
	}
	now := time.Now().UTC()
	rec.Launch.EndedAt = &now
	rec.Launch.Cleanup = cleanup
	_ = s.writeLifecycle(rec)
}

// recoverPrevious makes sure nothing from the state's previous launch is
// still running before the state is reused or removed: it kills the recorded
// cgroup, waits for it to empty and removes it, treating an absent cgroup as
// done. A previous launch that ran without supervision cannot be proven gone,
// so reuse and removal are refused.
func (s *State) recoverPrevious(ctx context.Context) error {
	rec, err := s.readLifecycle()
	if err != nil {
		return err
	}
	l := rec.Launch
	if l == nil {
		return nil
	}
	if l.EndedAt != nil && l.Cleanup == cleanupComplete {
		return nil
	}
	if l.Supervision != supervisionCgroup || l.Cgroup == "" {
		return fmt.Errorf("the previous launch of managed state %s ran without cgroup supervision, so its descendants cannot be shown to have exited", s.ID)
	}
	if err := recoverCgroup(ctx, l.Cgroup); err != nil {
		return fmt.Errorf("recover the previous launch of managed state %s: %w", s.ID, err)
	}
	now := time.Now().UTC()
	l.EndedAt = &now
	l.Cleanup = cleanupComplete
	return s.writeLifecycle(rec)
}

// Remove deletes the state after recovering its last launch's cgroup. It
// refuses while another launch holds the state.
func (s *State) Remove(ctx context.Context) error {
	if err := s.lock(); err != nil {
		return err
	}
	if err := s.recoverPrevious(ctx); err != nil {
		return err
	}
	return s.removeTree()
}

// removeTree deletes the session root. The caller holds the lock and has
// shown the state's cgroup empty.
func (s *State) removeTree() error {
	path := s.root.canonical
	if !strings.HasPrefix(path, s.parent.canonical+string(filepath.Separator)) {
		return fmt.Errorf("refusing to remove %s: not beneath %s", path, s.parent.canonical)
	}
	// Still locked while the tree goes: the lock file is inside it, and a
	// launch racing for the state must not win it half-deleted.
	err := os.RemoveAll(path)
	s.unlock()
	if err != nil {
		return fmt.Errorf("remove managed state %s: %w", s.ID, err)
	}
	return nil
}

// Close releases the state's descriptors and lock without deleting it.
func (s *State) Close() {
	if s == nil {
		return
	}
	s.unlock()
	s.root.close()
	s.parent.close()
}

// ensureDir creates name (0700) beneath dir if absent and pins it, refusing a
// symlink or non-directory in its place.
func ensureDir(dir *pinned, name string) (*pinned, error) {
	if err := unix.Mkdirat(dir.fd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("create %s: %w", filepath.Join(dir.canonical, name), err)
	}
	p, err := pinAt(dir, name)
	if err != nil {
		return nil, err
	}
	if !p.isDir() {
		p.close()
		return nil, fmt.Errorf("%s is not a directory", p.canonical)
	}
	return p, nil
}

// writeFileAt writes data to name beneath dirfd without following symlinks.
// truncate=false fails with EEXIST on an existing file (a seed is written
// once); truncate=true replaces its content (the wrapper's own records).
func writeFileAt(dirfd int, name string, data []byte, mode uint32, truncate bool) error {
	flags := unix.O_WRONLY | unix.O_CREAT | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if truncate {
		flags |= unix.O_TRUNC
	} else {
		flags |= unix.O_EXCL
	}
	fd, err := unix.Openat(dirfd, name, flags, mode)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// seedFile writes a seed unless the file already exists.
func seedFile(dir *pinned, name string, data []byte) error {
	err := writeFileAt(dir.fd, name, data, 0o600, false)
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("seed %s: %w", filepath.Join(dir.canonical, name), err)
	}
	return nil
}

func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
