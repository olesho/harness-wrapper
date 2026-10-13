package chatcore

import (
	"context"
	"fmt"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// maybeExtractSessionID opportunistically recovers the harness's own session
// ID, preferring a cheap screen scrape (turns.SessionIDExtractor) and falling
// back to an on-disk lookup keyed on the working directory and the launch
// time (turns.SessionIDLocator). The disk fallback exists because some
// harnesses (Codex 0.142+) stopped printing the "resume <uuid>" hint to the
// screen, so the scrape returns nothing and the only remaining anchor is the
// persisted session log. Once an ID is known we don't probe again — but while
// the Store has not got it, each call writes it again. No-op for adapters
// that implement neither capability.
func (c *Conversation) maybeExtractSessionID() {
	c.mu.Lock()
	known := c.session.HarnessID() != ""
	c.mu.Unlock()
	if known {
		c.saveHarnessID()
		return
	}

	id, ok := c.extractSessionID()
	if !ok {
		return
	}
	c.recordHarnessID(id)
}

// extractSessionID tries the screen scrape first, then the on-disk locator.
// Returns ("", false) when neither yields an ID. The locator looks where the
// harness files its sessions — the directory it runs in, as every transcript
// read does — for the one session started since the launch; it names none
// while that is ambiguous, and the next turn's end asks again.
func (c *Conversation) extractSessionID() (string, bool) {
	if ext, ok := c.adapter.(turns.SessionIDExtractor); ok {
		if id, ok := ext.ExtractSessionID(c.screen.Snapshot()); ok {
			return id, true
		}
	}
	if loc, ok := c.adapter.(turns.SessionIDLocator); ok {
		if id, ok := loc.LocateSessionID(c.transcriptDir(), c.launchedAt); ok {
			return id, true
		}
	}
	return "", false
}

// captureRawSessionID is the wrapper's durable line-tap callback (wired in Open
// only when the adapter implements turns.RawSessionIDExtractor). It runs
// synchronously in the PTY read goroutine — one call per raw output line, in
// order — and records the harness's own session ID the moment it appears in the
// stream (e.g. Claude Code's "claude --resume <uuid>" exit hint). Once captured
// we stop probing. Kept cheap so it does not back-pressure the read loop:
// a string compare short-circuits after capture, and the regex only runs while
// the ID is still unknown.
func (c *Conversation) captureRawSessionID(line string) {
	c.mu.Lock()
	already := c.session.HarnessID() != ""
	c.mu.Unlock()
	if already {
		return
	}

	ext, ok := c.adapter.(turns.RawSessionIDExtractor)
	if !ok {
		return
	}
	id, ok := ext.ExtractSessionIDFromLine(line)
	if !ok {
		return
	}
	c.recordHarnessID(id)
}

// recordHarnessID records id as the harness's own session id and saves it:
// the one write both capture paths share. First write wins — an id already
// recorded (assigned at launch, resumed, or captured by the other path while
// this one was still looking) is never replaced, in memory or in the Store.
// The check and the write share one hold of mu: a screen scrape or a disk
// lookup takes time, and a late one used to overwrite the id the line tap had
// captured meanwhile, sending History to another session's transcript.
func (c *Conversation) recordHarnessID(id string) {
	c.mu.Lock()
	if c.session.HarnessID() != "" {
		c.mu.Unlock()
		return
	}
	c.session.setHarnessID(id)
	c.harnessIDUnsaved = true
	c.mu.Unlock()
	c.saveHarnessID()
}

// saveHarnessID writes the session record while the harness session id in it
// has not reached the Store. A failed write keeps it unsaved — the running
// conversation still knows the id — so it is written again on the next call:
// at each turn's end (maybeExtractSessionID) and at exit (exitWith). Until
// then State().HarnessSessionIDErr carries the failure, and EventExited
// carries it if the id never reaches the Store.
func (c *Conversation) saveHarnessID() {
	c.mu.Lock()
	unsaved := c.harnessIDUnsaved
	c.mu.Unlock()
	if unsaved {
		_ = c.saveSession(context.Background())
	}
}

// saveSession writes the session record to the Store. The copy is taken under
// saveMu, when the write begins, so writes land in the order their copies were
// taken and the Store ends up with the newest. A write carrying the harness
// session id settles it: saved, or its error kept for the next attempt.
func (c *Conversation) saveSession(ctx context.Context) error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	rec := c.session.clone()
	c.mu.Unlock()
	err := c.store.UpdateSession(ctx, &rec)
	if rec.HarnessID() != "" {
		c.mu.Lock()
		switch {
		case err == nil:
			c.harnessIDUnsaved, c.harnessIDSaveErr = false, nil
		case c.harnessIDUnsaved:
			c.harnessIDSaveErr = err
		}
		c.mu.Unlock()
	}
	return err
}

// harnessIDSaveFailure returns the failure to save the harness session id:
// ErrHarnessSessionIDNotSaved wrapping the Store's error, or nil when the id
// is saved, unknown, or its write has not failed.
func (c *Conversation) harnessIDSaveFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.harnessIDUnsaved || c.harnessIDSaveErr == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrHarnessSessionIDNotSaved, c.session.HarnessID(), c.harnessIDSaveErr)
}
