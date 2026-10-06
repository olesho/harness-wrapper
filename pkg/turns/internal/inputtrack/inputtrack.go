// Package inputtrack turns per-frame blocking-dialog detections into balanced
// InputRequested / InputResolved transitions, shared by the screen-scraping
// turn adapters so they cannot drift apart on the edge cases.
package inputtrack

import "github.com/olesho/harness-wrapper/pkg/turns"

// Tracker remembers the blocking dialog currently on screen. The zero value
// is ready to use; it is not safe for concurrent use (adapters call it under
// their own lock).
type Tracker struct {
	lastID string
	last   *turns.InputRequest
}

// Observe records one frame's detection — req is the dialog on screen, or nil
// for none — and returns the transition events. prefix starts each event
// Reason (e.g. "codex: ").
//
// A different dialog replacing the tracked one without a dialog-free frame in
// between (e.g. one startup screen giving way to the next) first resolves the
// previous dialog, so every InputRequested is balanced by an InputResolved and
// a consumer's pending request is never silently overwritten with the
// replacement's identity and kind.
func (t *Tracker) Observe(req *turns.InputRequest, prefix string) []turns.Event {
	switch {
	case req != nil && req.ID == t.lastID:
		return nil
	case req != nil:
		out := t.resolve(prefix)
		t.lastID, t.last = req.ID, req
		return append(out, turns.Event{Kind: turns.InputRequested, Reason: prefix + req.Prompt, Input: req})
	default:
		return t.resolve(prefix)
	}
}

// LastID is the tracked dialog's request ID, or "" when none is on screen.
func (t *Tracker) LastID() string { return t.lastID }

func (t *Tracker) resolve(prefix string) []turns.Event {
	if t.lastID == "" {
		return nil
	}
	resolved := t.last
	if resolved == nil {
		resolved = &turns.InputRequest{ID: t.lastID}
	}
	t.lastID, t.last = "", nil
	return []turns.Event{{Kind: turns.InputResolved, Reason: prefix + "input resolved", Input: resolved}}
}
