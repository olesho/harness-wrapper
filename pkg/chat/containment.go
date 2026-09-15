package chat

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// ContainmentRecordVersion is the schema of SessionContainment. Readers
// refuse records of any other version rather than guess at their meaning.
const ContainmentRecordVersion = 1

// SessionContainment is the containment record of a contained conversation.
// It is persisted BEFORE the conversation's first launch, and every later
// launch — Reopen included — inherits it: a contained conversation can never
// continue uncontained or under a different policy.
//
// Downgrade guard: in a contained Session the legacy HarnessSessionID field
// stays empty for good, and the harness's own session id lives only here. An
// older harness-wrapper, which knows nothing of this record, therefore sees a
// session with no harness session id and refuses to resume it
// (ErrNoHarnessSession) instead of resuming it unrestricted — even when its
// decoder drops this field. Read the id through Session.HarnessID.
type SessionContainment struct {
	// Version is ContainmentRecordVersion.
	Version int `json:"version"`

	// Required is the normalized request every launch must enforce.
	Required *containment.Request `json:"required"`

	// Profile and ProfileVersion identify the harness profile selected when
	// the conversation was created; a resume refuses when that profile is no
	// longer available.
	Profile        string `json:"profile"`
	ProfileVersion int    `json:"profile_version"`

	// StateID names the wrapper-managed private state (HOME, TMPDIR and the
	// harness's own state root) every launch of the conversation reuses.
	StateID string `json:"state_id,omitempty"`

	// StateDeleted records that DeleteContainmentState removed the state;
	// the conversation can no longer be resumed.
	StateDeleted bool `json:"state_deleted,omitempty"`

	// Resumable is false for a conversation opened by a single-launch caller
	// (harness.RunTurn, harness-chatd, the structured runner), whose private
	// state is deleted when its only launch ends.
	Resumable bool `json:"resumable"`

	// HarnessSessionID is the harness's own session id; see the downgrade
	// guard above.
	HarnessSessionID string `json:"harness_session_id,omitempty"`

	// Targets maps each requested path (working directory, caller grants,
	// StateDir) to the canonical target it resolved to at creation. A resume
	// whose paths now resolve elsewhere is refused.
	Targets map[string]string `json:"targets,omitempty"`

	// LastLaunch is the applied policy of the most recent launch.
	LastLaunch *containment.Applied `json:"last_launch,omitempty"`
}

// Clone returns a deep copy (nil for nil).
func (r *SessionContainment) Clone() *SessionContainment {
	if r == nil {
		return nil
	}
	c := *r
	c.Required = r.Required.Clone()
	c.Targets = maps.Clone(r.Targets)
	c.LastLaunch = r.LastLaunch.Clone()
	return &c
}

// HarnessID returns the harness's own session id through the one
// containment-aware accessor: the containment record's for a contained
// session, the legacy HarnessSessionID for every other. Callers that opt into
// containment must read the id here, never from the legacy field.
func (s Session) HarnessID() string {
	if s.Containment != nil {
		return s.Containment.HarnessSessionID
	}
	return s.HarnessSessionID
}

// setHarnessID records id where HarnessID reads it. A published containment
// record is never mutated in place (copy-on-write), so a copy of the session
// handed to a Store or a caller cannot change under it. Only the one field
// changes: a Conversation's session ID is read without its lock.
func (s *Session) setHarnessID(id string) {
	if s.Containment != nil {
		rec := s.Containment.Clone()
		rec.HarnessSessionID = id
		s.Containment = rec
		return
	}
	s.HarnessSessionID = id
}

// withHarnessID returns s carrying id; see setHarnessID.
func (s Session) withHarnessID(id string) Session {
	s.setHarnessID(id)
	return s
}

// clone returns s with its containment record deep-copied.
func (s Session) clone() Session {
	s.Containment = s.Containment.Clone()
	return s
}

// validContainmentRecord checks the downgrade-guard invariants of a stored
// contained session before anything uses it.
func validContainmentRecord(s *Session) error {
	rec := s.Containment
	if rec.Version != ContainmentRecordVersion {
		return fmt.Errorf("%w: session %s has containment record version %d; this build reads version %d",
			ErrInvalidOptions, s.ID, rec.Version, ContainmentRecordVersion)
	}
	if s.HarnessSessionID != "" {
		return fmt.Errorf("%w: contained session %s carries a legacy harness session id, which the containment record forbids",
			ErrInvalidOptions, s.ID)
	}
	if rec.Required == nil {
		return fmt.Errorf("%w: contained session %s has no required policy", ErrInvalidOptions, s.ID)
	}
	return nil
}

// ContainmentStore is the optional Store extension a store implements to
// declare that it persists contained conversations faithfully. chat.Open and
// Reopen refuse containment on a Store that does not implement it, rather than
// trust a record the store may silently drop. chat.Store itself gains no
// method: callers implement it.
//
// By implementing it a store commits that CreateSession, UpdateSession and
// GetSession round-trip Session.Containment intact, and that it never
// populates the legacy HarnessSessionID of a session that has one.
type ContainmentStore interface {
	Store
	// StoresContainment is a marker; it is never called.
	StoresContainment()
}

// ErrContainmentUnavailable is returned when a contained conversation cannot
// be created or resumed: the Store does not implement ContainmentStore, the
// record is not resumable, its private state is gone, or its profile is no
// longer available. It wraps ErrInvalidOptions.
var ErrContainmentUnavailable = fmt.Errorf("%w: containment unavailable for this conversation", ErrInvalidOptions)

// Containment returns the effective policy of the conversation's current
// launch, or nil when it is not contained.
func (c *Conversation) Containment() *containment.Applied {
	if c.sess == nil {
		return nil
	}
	return c.sess.Containment()
}

// DeleteContainmentState removes the private state of a stored contained
// conversation: it first ends whatever the conversation's last launch left in
// its recorded cgroup, then deletes the directory and marks the record, which
// can no longer be resumed. Deleting state a launch still uses is refused.
func DeleteContainmentState(ctx context.Context, store Store, sessionID string) error {
	if _, ok := store.(ContainmentStore); !ok {
		return fmt.Errorf("%w: the store does not persist containment records", ErrContainmentUnavailable)
	}
	rec, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if rec.Containment == nil {
		return fmt.Errorf("%w: session %s is not contained", ErrInvalidOptions, sessionID)
	}
	if err := validContainmentRecord(rec); err != nil {
		return err
	}
	if rec.Containment.StateID != "" && !rec.Containment.StateDeleted {
		st, err := contain.OpenState(rec.Containment.StateID)
		switch {
		case errors.Is(err, contain.ErrStateGone):
			// Already gone (a single-launch conversation's state ends with it).
		case err != nil:
			return fmt.Errorf("chat: open private state of session %s: %w", sessionID, err)
		default:
			err = st.Remove(ctx)
			st.Close()
			if err != nil {
				return fmt.Errorf("chat: delete private state of session %s: %w", sessionID, err)
			}
		}
	}
	updated := rec.clone()
	updated.Containment.StateDeleted = true
	updated.Containment.Resumable = false
	return store.UpdateSession(ctx, &updated)
}

// containedLaunch is what openWithSession needs to start a contained
// conversation: the context carrying the lifecycle options for wrapper.Start,
// and the state a failed first launch must give back.
type containedLaunch struct {
	ctx      context.Context
	state    *contain.State
	ownState bool // allocated here for a new stored conversation
	launched bool // the conversation is running; nothing to give back
}

// prepareContainment validates a contained Open or Reopen, allocates private
// state for a new stored conversation and returns the record to persist
// before launch. session is the chat record being opened (fresh for Open,
// stored for Reopen).
func prepareContainment(ctx context.Context, opts Options, session *Session, fresh bool) (*containedLaunch, error) {
	if _, ok := opts.Store.(ContainmentStore); !ok {
		return nil, fmt.Errorf("%w: the store does not implement chat.ContainmentStore, so it may not persist the containment record", ErrContainmentUnavailable)
	}
	lo := contain.LaunchOptionsFrom(ctx)
	out := &containedLaunch{ctx: ctx}

	if !fresh {
		rec := session.Containment
		if err := validContainmentRecord(session); err != nil {
			return nil, err
		}
		if opts.Containment != nil && !containment.Equal(opts.Containment, rec.Required) {
			return nil, fmt.Errorf("%w: session %s was created with a different containment policy; policy changes need a new conversation",
				ErrInvalidOptions, session.ID)
		}
		switch {
		case !rec.Resumable:
			return nil, fmt.Errorf("%w: session %s was opened by a single-launch caller and cannot be resumed", ErrContainmentUnavailable, session.ID)
		case rec.StateDeleted || rec.StateID == "":
			return nil, fmt.Errorf("%w: the private state of session %s has been deleted", ErrContainmentUnavailable, session.ID)
		}
		profile, version, err := contain.ProfileID(opts.Harness)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrContainmentUnavailable, err)
		}
		if profile != rec.Profile || version != rec.ProfileVersion {
			return nil, fmt.Errorf("%w: session %s was created under profile %s (manifest %d), which this build no longer provides (%s, manifest %d)",
				ErrContainmentUnavailable, session.ID, rec.Profile, rec.ProfileVersion, profile, version)
		}
		st, err := contain.OpenState(rec.StateID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrContainmentUnavailable, err)
		}
		out.state = st
		out.ctx = contain.WithLaunchOptions(ctx, contain.LaunchOptions{
			State: st, RequireSupervision: true, ExpectTargets: rec.Targets,
		})
		return out, nil
	}

	required, err := containment.Normalize(opts.Containment)
	if err != nil {
		return nil, fmt.Errorf("%w: %w: %w", ErrInvalidOptions, wrapper.ErrContainmentRefused, err)
	}
	profile, version, err := contain.ProfileID(opts.Harness)
	if err != nil {
		if errors.Is(err, contain.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, wrapper.ErrContainmentUnsupported)
		}
		return nil, fmt.Errorf("%w: %w: %w", ErrInvalidOptions, wrapper.ErrContainmentRefused, err)
	}
	if opts.Resume != "" && required.StateDir == "" {
		return nil, fmt.Errorf("%w: Resume on a contained Open needs a caller-managed StateDir holding that session; a contained conversation resumes through Reopen, which reuses its private state", ErrInvalidOptions)
	}
	rec := &SessionContainment{
		Version:        ContainmentRecordVersion,
		Required:       required,
		Profile:        profile,
		ProfileVersion: version,
	}
	switch {
	case lo.SingleLaunch:
		// One launch, never reopened: the wrapper (or the caller that put
		// State on the context) owns the private state.
		if lo.State != nil {
			rec.StateID = lo.State.ID
		}
	default:
		st, err := contain.NewState(true)
		if err != nil {
			if errors.Is(err, contain.ErrUnsupported) {
				return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, wrapper.ErrContainmentUnsupported)
			}
			return nil, fmt.Errorf("chat: allocate private state: %w", err)
		}
		out.state, out.ownState = st, true
		rec.StateID = st.ID
		rec.Resumable = true
		out.ctx = contain.WithLaunchOptions(ctx, contain.LaunchOptions{State: st, RequireSupervision: true})
	}
	session.Containment = rec
	if opts.Resume != "" {
		session.setHarnessID(opts.Resume)
	}
	return out, nil
}

// abandon gives back private state allocated for a first launch that never
// ran; the persisted record is marked so nothing tries to resume it.
func (cl *containedLaunch) abandon(ctx context.Context, store Store, session Session) {
	if cl == nil || cl.state == nil {
		return
	}
	if cl.ownState {
		_ = cl.state.Remove(ctx)
		if session.Containment != nil {
			updated := session.clone()
			updated.Containment.StateDeleted = true
			updated.Containment.Resumable = false
			_ = store.UpdateSession(ctx, &updated)
		}
	}
	cl.state.Close()
}

// keepUntilEnd holds the conversation's state handle until its launch has
// fully ended: the wrapper borrows the state (and its lock) for the life of
// the launch and never closes a state it did not allocate.
func (cl *containedLaunch) keepUntilEnd(sess *wrapper.Session) {
	if cl.state == nil {
		return
	}
	st := cl.state
	go func() {
		_, _ = sess.Wait()
		st.Close()
	}()
}

// recordLaunch routes the adapter's readers to the launch's private layout
// and records the applied policy — and, on the first launch, the canonical
// targets a resume must still match — in the stored record.
func (c *Conversation) recordLaunch(ctx context.Context, cl *containedLaunch) error {
	applied := c.sess.Containment()
	if applied == nil {
		return fmt.Errorf("chat: contained launch reported no applied policy")
	}
	// Safe after Start: nothing reads the adapter's state roots before the
	// watcher starts, and the durable line tap does not use them.
	configureAdapterEnv(c.adapter, adapterEnvFor(applied))

	c.mu.Lock()
	rec := c.session.Containment.Clone()
	rec.LastLaunch = applied
	if rec.StateID == "" {
		rec.StateID = applied.State.ID
	}
	if len(rec.Targets) == 0 {
		rec.Targets = contain.Targets(applied)
	}
	c.session.Containment = rec
	updated := c.session.clone()
	c.mu.Unlock()
	if err := c.store.UpdateSession(ctx, &updated); err != nil {
		return fmt.Errorf("chat: record the applied containment policy: %w", err)
	}
	return nil
}

// adapterEnvFor returns the environment a contained session's adapter reads
// its state roots from: exactly the private (or caller-managed) layout the
// harness was given, never the wrapper user's own state. A profile without a
// harness state root yields a root that does not exist, so no reader falls
// back to the default.
func adapterEnvFor(a *containment.Applied) []string {
	if a == nil {
		return nil
	}
	root := a.State.HarnessState
	if root == "" {
		root = a.State.Home + string(os.PathSeparator) + ".harness-wrapper-no-state"
	}
	return []string{"CLAUDE_CONFIG_DIR=" + root, "CODEX_HOME=" + root, "HOME=" + a.State.Home}
}
