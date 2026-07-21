package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/discovery/models"
)

// This file is the LIVE half of model discovery — the driver that feeds a real
// `/model` picker screen to models.ParseModelPicker (the pure parser lives in
// pkg/discovery/models). It is a faithful port of discoverModels from
// meta-harness's src/discovery/models.ts.
//
// Unlike Send, DiscoverModels is NOT a turn: the `/model` picker is a read-only
// TUI probe that never completes as a turn. So it drives stdin directly
// (AcquireControl + Wrapper().WriteStdin, the escape hatch send.go documents)
// rather than registering a currentTurn the picker could never satisfy.

// Sentinel errors for the DiscoverModels error contract. The auth-required case
// reuses the package-wide ErrAuthRequired (returned by waitReadyForSend), so the
// three distinct outcomes are: ErrPickerUnsupported, ErrAuthRequired, and
// ErrPickerTimeout.
var (
	// ErrPickerUnsupported is returned by DiscoverModels for a harness whose
	// `/model` picker is not recognized by the parser — anything but
	// claude-code/claude and codex (mirrors models.ParseModelPicker's header
	// gate, which yields [] for e.g. pi/opencode/generic). Call sites wrap it
	// with the harness name; errors.Is still matches.
	ErrPickerUnsupported = errors.New("chat: harness does not support /model discovery")

	// ErrPickerTimeout is returned by DiscoverModels when the `/model` picker
	// never rendered a parseable screen within the render budget
	// (DiscoverModelsOptions.RenderTimeout). This is the render-deadline arm,
	// layered on top of the readiness wait — distinct from the auth fast-fail.
	ErrPickerTimeout = errors.New("chat: /model picker did not render before deadline")
)

// defaultPickerRenderTimeout is the default budget for the picker to RENDER,
// mirroring the TS discoverModels 15000 ms default. It covers picker-render
// ONLY: it is layered on top of the readiness wait (which has its own
// dwell/timeout), so a slow CLI cold-start does not eat this window and cause a
// healthy picker to time out spuriously.
const defaultPickerRenderTimeout = 15 * time.Second

// pickerPollInterval is how often the render loop re-parses the screen while
// waiting for the picker, mirroring the TS 150 ms sleep.
const pickerPollInterval = 150 * time.Millisecond

// DiscoverModelsOptions configures DiscoverModels. It mirrors the chat/oneshot
// launch surface (the fields Open needs), plus a picker render budget.
type DiscoverModelsOptions struct {
	// Harness names the per-harness adapter. Only claude-code/claude and codex
	// expose a parseable `/model` picker; any other harness fast-fails with
	// ErrPickerUnsupported. Required.
	Harness string

	// BinaryPath is the harness executable. Required.
	BinaryPath string

	// WorkingDir is the harness's working directory. Defaults to the current
	// process's CWD.
	WorkingDir string

	// Env is the harness's environment. Defaults to the current process's
	// environment.
	Env []string

	// Cols, Rows configure the virtual PTY size. Defaults: 120x40 (see Open).
	Cols, Rows int

	// RenderTimeout bounds how long to wait for the picker to RENDER after
	// readiness, before giving up with ErrPickerTimeout. Zero uses
	// defaultPickerRenderTimeout (15s), mirroring the TS default. This budget is
	// layered on top of the readiness wait, not an end-to-end deadline.
	RenderTimeout time.Duration
}

// DiscoverModels launches the harness on an ephemeral memstore-backed session,
// probes its `/model` picker read-only, and returns the models it lists. It is
// read-only: it never selects a model — after writing `/model` the picker is
// left open, but because the session is memstore-backed and Close'd immediately
// in the defer, no Escape/cleanup keystroke is required.
//
// It gates on readiness up front (so an unauthenticated CLI fast-fails with
// ErrAuthRequired rather than hanging to the render deadline), then writes
// `/model` and polls the rendered screen against models.ParseModelPicker until
// it yields a non-empty list or the render budget elapses.
//
// Error contract — three distinct outcomes:
//   - ErrPickerUnsupported: the harness has no parseable picker (not
//     claude-code/claude or codex).
//   - ErrAuthRequired: the CLI is logged out / not onboarded (fast-fail).
//   - ErrPickerTimeout: the picker never rendered within RenderTimeout.
func DiscoverModels(ctx context.Context, opts DiscoverModelsOptions) ([]models.Info, error) {
	if !pickerSupported(opts.Harness) {
		return nil, fmt.Errorf("%w: %q", ErrPickerUnsupported, opts.Harness)
	}

	conv, err := Open(ctx, Options{
		Harness:    opts.Harness,
		BinaryPath: opts.BinaryPath,
		WorkingDir: opts.WorkingDir,
		Env:        opts.Env,
		Cols:       opts.Cols,
		Rows:       opts.Rows,
		Store:      newEphemeralStore(),
	})
	if err != nil {
		return nil, err
	}
	// Close on an independent, bounded context so a cancelled/expired ctx still
	// tears the ephemeral session down. Mirrors the TS 2s close deadline.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conv.Close(closeCtx)
	}()

	// Take control for the probe. The picker is read-only, but the token
	// serializes the WriteStdin with any other writer just as Send does.
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	// Gate on readiness up front. Because this driver uses the WriteStdin escape
	// hatch (not Send), it must itself surface the auth signal: on a logged-out /
	// not-onboarded CLI, waitReadyForSend returns ErrAuthRequired, which we
	// propagate immediately as a distinct fast-fail rather than writing `/model`
	// and hanging to the render deadline.
	if err := conv.waitReadyForSend(ctx); err != nil {
		if errors.Is(err, ErrAuthRequired) {
			return nil, ErrAuthRequired
		}
		return nil, err
	}

	// The submit key is screen-text-dependent (enhanced-keyboard vs plain CR),
	// so resolve it from the current screen — never hardcode "\r". Same helper
	// Send uses.
	submitKey := submitKeyForHarness(opts.Harness, conv.screen.Snapshot().Text)
	if _, err := conv.sess.WriteStdin(append([]byte("/model"), submitKey...)); err != nil {
		return nil, fmt.Errorf("chat: discover models: write /model: %w", err)
	}

	// Render budget layered on top of the readiness wait. Poll the settled screen
	// against the pure parser until it yields models or the deadline elapses.
	renderTimeout := opts.RenderTimeout
	if renderTimeout <= 0 {
		renderTimeout = defaultPickerRenderTimeout
	}
	deadline := time.NewTimer(renderTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pickerPollInterval)
	defer ticker.Stop()

	for {
		if found := models.ParseModelPicker(conv.screen.Snapshot().Text, opts.Harness); len(found) > 0 {
			return found, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-conv.closed:
			return nil, ErrClosed
		case <-deadline.C:
			return nil, ErrPickerTimeout
		case <-ticker.C:
		}
	}
}

// pickerSupported reports whether the harness exposes a parseable `/model`
// picker. It mirrors models.ParseModelPicker's header gate (which recognizes
// only claude/claude-code and codex); every other harness parses to [] and so
// is fast-failed with ErrPickerUnsupported before a session is even launched.
func pickerSupported(harness string) bool {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude", "claude-code", "codex":
		return true
	default:
		return false
	}
}

// ephemeralStore is a minimal in-memory Store for the throwaway DiscoverModels
// session. pkg/chat cannot import pkg/chat/memstore (memstore imports chat — an
// import cycle), so the driver ships its own tiny store rather than take a Store
// dependency in DiscoverModelsOptions: the picker probe registers no turns and
// its records are discarded on Close, so durability is irrelevant.
type ephemeralStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	turns    map[string][]Turn
}

func newEphemeralStore() *ephemeralStore {
	return &ephemeralStore{
		sessions: make(map[string]Session),
		turns:    make(map[string][]Turn),
	}
}

func (s *ephemeralStore) CreateSession(_ context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = *sess
	return nil
}

func (s *ephemeralStore) GetSession(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("chat: ephemeral store: session %s not found", id)
	}
	return &sess, nil
}

func (s *ephemeralStore) UpdateSession(_ context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = *sess
	return nil
}

func (s *ephemeralStore) AppendTurn(_ context.Context, t *Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns[t.SessionID] = append(s.turns[t.SessionID], *t)
	return nil
}

func (s *ephemeralStore) UpdateTurn(_ context.Context, t *Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.turns[t.SessionID]
	for i := range list {
		if list[i].ID == t.ID {
			list[i] = *t
			return nil
		}
	}
	s.turns[t.SessionID] = append(list, *t)
	return nil
}

func (s *ephemeralStore) ListTurns(_ context.Context, sessionID string) ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Turn(nil), s.turns[sessionID]...), nil
}
