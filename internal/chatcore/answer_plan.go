package chatcore

import (
	"context"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// answerOptionIDs resolves an answer's options — each named by ID, Alias or
// Label — to option IDs, once each. An answer that names no option but
// carries text goes to the request's "other" option, the one a question
// takes free text through; without one, the text alone is the answer.
func answerOptionIDs(req *turns.InputRequest, ans InputAnswer) ([]string, error) {
	names := ans.OptionIDs
	if ans.OptionID != "" {
		names = []string{ans.OptionID}
	}
	if len(names) == 0 && ans.Text != "" {
		if o := findOptionByAlias(req, "other"); o != nil {
			return []string{o.ID}, nil
		}
		return nil, nil
	}
	ids := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		o := findOption(req, n)
		if o == nil {
			return nil, ErrUnknownOption
		}
		if !seen[o.ID] {
			seen[o.ID] = true
			ids = append(ids, o.ID)
		}
	}
	return ids, nil
}

// runAnswerPlan writes an answer the adapter planned (turns.AnswerPlanner)
// one step at a time, each no sooner than its After since the request was
// raised. After each write it waits for the step's evidence, read
// back through the dialog's own parse, before writing the next: the rules
// answerAndConfirm follows for one key, applied to every key of a longer
// answer, and writes nothing unless the dialog on screen is the request's own.
// It never re-sends a step. An answer whose dialog or evidence does not
// appear within the render budget ends with a bounded *InputUnresolvedError,
// whose Attempts counts the steps written.
//
// Without a screen to read, or a DialogReader to read it with, there is
// nothing to wait for, and the steps are written back to back.
func (c *Conversation) runAnswerPlan(ctx context.Context, req *turns.InputRequest, steps []turns.AnswerStep) error {
	dr, ok := c.adapter.(turns.DialogReader)
	if c.screen == nil || !ok {
		for _, s := range steps {
			if err := c.write(s.Keys); err != nil {
				return err
			}
		}
		return nil
	}
	for i, s := range steps {
		if err := c.waitAfterRaised(ctx, req, s.After); err != nil {
			return err
		}
		// Keys go only to the dialog the plan was made for. A request whose
		// pane is not on screen — answered meanwhile, or read from a frame
		// claude was still painting, which mixes a pane with the one before —
		// gets none; a frame mid-paint only delays the write.
		on, seen, err := c.awaitScreen(ctx, func(text string) bool {
			live, ok := dr.ReadDialog(text)
			return ok && live.ID == req.ID
		})
		if err != nil {
			return err
		}
		if !on {
			return c.unresolvedInput(req, seen, i)
		}
		if err := c.write(s.Keys); err != nil {
			return err
		}
		held, seen, err := c.awaitScreen(ctx, func(text string) bool {
			return evidenceHolds(dr, text, req, s.Until)
		})
		if err != nil {
			return err
		}
		if !held {
			return c.unresolvedInput(req, seen, i+1)
		}
	}
	return nil
}

// waitAfterRaised returns once req has been up for d, counted from when it was
// raised — at once if it already has. A request this Conversation did not
// raise counts from now.
func (c *Conversation) waitAfterRaised(ctx context.Context, req *turns.InputRequest, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	c.mu.Lock()
	raised := time.Now()
	if c.currentInput != nil && c.currentInput.ID == req.ID && !c.currentInputAt.IsZero() {
		raised = c.currentInputAt
	}
	c.mu.Unlock()
	wait := time.Until(raised.Add(d))
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	case <-t.C:
		return nil
	}
}

// evidenceHolds reports whether text shows what ev waits for in req's dialog.
//
// The dialog is identified by its request ID, through a fresh parse of the
// whole screen, so its ID must not move while it is being answered: the
// adapter keeps checkbox states, the highlight and typed text out of it.
//
// A dialog is gone when another one parses in its place, or when nothing
// parses and no dialog anchor is painted. Its prompt is no evidence either
// way: claude prints an answered question into the conversation, and its
// review pane lists the questions answered.
func evidenceHolds(dr turns.DialogReader, text string, req *turns.InputRequest, ev turns.AnswerEvidence) bool {
	live, ok := dr.ReadDialog(text)
	if ev.Kind == turns.EvidenceGone {
		if ok {
			return live.ID != req.ID
		}
		return !dr.DialogAnchorPresent(text)
	}
	if !ok || live.ID != req.ID {
		return false
	}
	for _, o := range live.Options {
		if o.ID != ev.OptionID {
			continue
		}
		switch ev.Kind {
		case turns.EvidenceHighlighted:
			return o.Highlighted
		case turns.EvidenceUnhighlighted:
			return !o.Highlighted
		case turns.EvidenceChecked:
			return o.Checked
		case turns.EvidenceTyped:
			return o.Typed == ev.Text
		}
	}
	return false
}
