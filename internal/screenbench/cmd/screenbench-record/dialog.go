//go:build screenbench

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// defaultAnswerWithin bounds an answer_dialog step that sets no "within".
const defaultAnswerWithin = 30 * time.Second

// answerRenderBudget bounds each wait inside an answer: for the highlight to
// reach the target row, and for the dialog to leave the screen.
const answerRenderBudget = 5 * time.Second

// answerDialog handles a blocking dialog the harness MAY show — claude's
// folder-trust dialog, which paints only in a directory claude has not trusted
// before. It waits on the rendered screen until one of:
//
//   - a dialog is detected: it is answered with the option whose label matches
//     label, and the step returns once the dialog has left the screen;
//   - the harness has printed something and then gone quiet for the settle
//     window (idleTimeout) with no dialog up: there is nothing to answer, which
//     is the trusted-directory start, and the step is a no-op;
//   - within passes, likewise a no-op.
//
// The keys are never hard-coded. They come from the production parser
// (claudecode.DetectInput) against the live screen, so they follow claude's
// option order and default highlight, both of which moved between releases.
// And the answer mirrors production's safety rule: navigation is written
// first, and the confirm key only once the highlight sits on the target row,
// so a lost or late arrow can never turn into Enter on "No, exit".
//
// A dialog that offers no such option, a highlight that never lands, and a
// dialog that does not clear are errors: an unattended rebake must stop rather
// than bake a recording of the wrong screen.
func (d *scriptDriver) answerDialog(ctx context.Context, label string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if req, ok := claudecode.DetectInput(d.screenText()); ok {
			opt := optionByLabel(req, label)
			if opt == nil {
				return fmt.Errorf("answer_dialog: dialog %q offers no option %q (options: %s)", req.Prompt, label, optionLabels(req))
			}
			return d.answer(ctx, req, label, opt)
		}
		if d.settled() || time.Now().After(deadline) {
			return nil
		}
		if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
}

// maxAnswers bounds the answers written to a dialog that keeps resetting —
// the same count production uses (pkg/chat answerAttempts).
const maxAnswers = 3

// answer writes the keys for label into req's dialog, confirming navigation
// before the confirm key, then waits for the dialog to leave the screen. It
// mirrors production's evidence rules (pkg/chat answerAndConfirm): the one
// state that licenses a second answer is the same dialog repainted with its
// highlight OFF the row just confirmed — claude resetting the dialog, which
// 2.1.261 and 2.1.270 both do to the first Enter after the dialog paints —
// and the re-answer is computed from that live screen. A highlight that never
// lands, or a dialog that stays exactly as it was, gets no further keys.
func (d *scriptDriver) answer(ctx context.Context, req *turns.InputRequest, label string, opt *turns.InputOption) error {
	keys := opt.Keys
	for answered := 0; ; {
		nav, confirm, split := splitConfirm(keys)
		if split {
			if _, err := d.stdin.WriteStdin(nav); err != nil {
				return err
			}
			landed, err := d.awaitScreen(ctx, func(text string) bool { return highlightedOn(text, req.Prompt, label) })
			if err != nil {
				return err
			}
			if !landed {
				return fmt.Errorf("answer_dialog: the highlight never reached %q; not pressing Enter on another row", label)
			}
			keys = confirm
		}
		if _, err := d.stdin.WriteStdin(keys); err != nil {
			return err
		}
		answered++
		var reset bool
		cleared, err := d.awaitScreen(ctx, func(text string) bool {
			if !dialogShowing(text, req.Prompt) {
				return true
			}
			reset = split && highlightedElsewhere(text, req.Prompt, label)
			return reset
		})
		if err != nil {
			return err
		}
		switch {
		case cleared && !reset:
			return nil
		case !reset || answered >= maxAnswers:
			return fmt.Errorf("answer_dialog: dialog %q still up after %d answer(s) with %q", req.Prompt, answered, label)
		}
		live, ok := claudecode.DetectInput(d.screenText())
		if !ok || live.Prompt != req.Prompt {
			return nil // it cleared while we looked
		}
		next := optionByLabel(live, label)
		if next == nil {
			return fmt.Errorf("answer_dialog: dialog %q no longer offers %q", req.Prompt, label)
		}
		keys = next.Keys
	}
}

// highlightedElsewhere reports whether the dialog with prompt is on screen
// with its highlight on a row other than label — the reset evidence.
func highlightedElsewhere(text, prompt, label string) bool {
	req, ok := claudecode.DetectInput(text)
	if !ok || req.Prompt != prompt {
		return false
	}
	opt := optionByLabel(req, label)
	return opt != nil && string(opt.Keys) != "\r"
}

// splitConfirm splits a selector answer (arrows + CR) into its navigation and
// its confirm key. A numbered answer ("2\r") has no navigation — the digit
// selects its row wherever the highlight is — and stays one write.
func splitConfirm(keys []byte) (nav, confirm []byte, ok bool) {
	if len(keys) < 2 || keys[len(keys)-1] != '\r' || !bytes.Contains(keys, []byte{0x1b}) {
		return nil, nil, false
	}
	return keys[:len(keys)-1], keys[len(keys)-1:], true
}

// highlightedOn reports whether the dialog with prompt is on screen with its
// highlight on label. The parser derives each option's keys from the current
// highlight, so the highlighted option is the one whose keys are a bare
// confirm — this reads the highlight through the same parse the answer used.
func highlightedOn(text, prompt, label string) bool {
	req, ok := claudecode.DetectInput(text)
	if !ok || req.Prompt != prompt {
		return false
	}
	opt := optionByLabel(req, label)
	return opt != nil && string(opt.Keys) == "\r"
}

// dialogShowing reports whether the dialog with prompt is still painted.
func dialogShowing(text, prompt string) bool {
	if prompt != "" {
		return strings.Contains(text, prompt)
	}
	_, ok := claudecode.DetectInput(text)
	return ok
}

// optionByLabel finds the option whose label matches label, ignoring case.
func optionByLabel(req *turns.InputRequest, label string) *turns.InputOption {
	for i := range req.Options {
		if strings.EqualFold(strings.TrimSpace(req.Options[i].Label), strings.TrimSpace(label)) {
			return &req.Options[i]
		}
	}
	return nil
}

func optionLabels(req *turns.InputRequest) string {
	labels := make([]string, 0, len(req.Options))
	for _, o := range req.Options {
		labels = append(labels, fmt.Sprintf("%q", o.Label))
	}
	return strings.Join(labels, ", ")
}

// awaitScreen polls the rendered screen until want reports true or
// answerRenderBudget passes.
func (d *scriptDriver) awaitScreen(ctx context.Context, want func(string) bool) (bool, error) {
	deadline := time.Now().Add(answerRenderBudget)
	for {
		if want(d.screenText()) {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
			return false, err
		}
	}
}

// settled reports whether the harness has printed something and then been
// quiet for the settle window — the same "screen settled" notion wait_for's
// idle fallback uses.
func (d *scriptDriver) settled() bool {
	d.mu.Lock()
	last := d.lastOut
	d.mu.Unlock()
	return !last.IsZero() && time.Since(last) >= d.idleTimeout
}

func sleepCtx(ctx context.Context, dur time.Duration) error {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
