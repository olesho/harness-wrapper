package chat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
)

// InputRequest is the client-facing view of a blocking interactive prompt
// the harness is showing (e.g. Claude Code's folder-trust dialog). It mirrors
// turns.InputRequest but deliberately omits the per-option keystrokes: the
// client answers semantically by option ID or Alias and the chat layer owns
// the translation to keys.
type InputRequest struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	Prompt      string        `json:"prompt"`
	Header      string        `json:"header,omitempty"`
	MultiSelect bool          `json:"multi_select,omitempty"`
	Options     []InputOption `json:"options,omitempty"`
}

// InputOption is one selectable choice in an InputRequest.
type InputOption struct {
	ID          string `json:"id"`
	Alias       string `json:"alias,omitempty"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// InputAnswer is how a caller answers an InputRequest. Set OptionID (an
// option ID or Alias) for single-select menu/confirm/trust prompts; set Text
// for free-text ("text_input") prompts; set OptionIDs (each an option ID or
// Alias) to select one or more options on a MultiSelect request.
//
// OptionID and OptionIDs are mutually exclusive: setting both returns
// ErrConflictingAnswer. OptionIDs on a request whose MultiSelect is false
// returns ErrNotMultiSelect.
type InputAnswer struct {
	OptionID  string   `json:"option_id,omitempty"`
	OptionIDs []string `json:"option_ids,omitempty"`
	Text      string   `json:"text,omitempty"`
}

// DispositionKind is how a policy disposes of a matched InputRequest.
type DispositionKind string

const (
	// DispositionAsk surfaces the request to the client (the default).
	DispositionAsk DispositionKind = "ask"
	// DispositionAnswer auto-answers with Disposition.OptionID / Text.
	DispositionAnswer DispositionKind = "answer"
	// DispositionDeny auto-answers with the request's "deny" option
	// (declining the prompt; for a trust dialog this exits the harness).
	DispositionDeny DispositionKind = "deny"
)

// Disposition is the action a policy takes for a matched request kind.
type Disposition struct {
	Kind     DispositionKind `json:"kind"`
	OptionID string          `json:"option_id,omitempty"` // for DispositionAnswer: an option ID or Alias
	Text     string          `json:"text,omitempty"`      // for DispositionAnswer on text_input
}

// InputPolicy pre-configures how interactive prompts are resolved without a
// live client in the loop. It is JSON-serializable so it can be supplied at
// open time over a transport (harness-chatd). When a request matches no rule
// (or the matched disposition is "ask"), the request is surfaced on Events().
type InputPolicy struct {
	// Default applies when ByKind has no entry for the request's kind.
	// Empty means "ask".
	Default DispositionKind `json:"default,omitempty"`
	// ByKind maps an InputRequest.Kind ("trust_prompt", …) to its action.
	ByKind map[string]Disposition `json:"by_kind,omitempty"`
}

func (p *InputPolicy) resolve(kind string) (Disposition, bool) {
	if p == nil {
		return Disposition{}, false
	}
	if d, ok := p.ByKind[kind]; ok && d.Kind != "" {
		return d, true
	}
	if p.Default != "" {
		return Disposition{Kind: p.Default}, true
	}
	return Disposition{}, false
}

// Answer responds to the interactive prompt currently awaiting an answer.
//
// Preconditions, mirroring Send:
//   - The caller must hold the control token (AcquireControl); otherwise
//     Answer returns ErrNoControl.
//
// requestID must match the pending request's ID (pass "" to target whatever
// is currently pending). Errors: ErrNoInputPending, ErrStaleInputRequest,
// ErrUnknownOption, ErrNotMultiSelect, ErrConflictingAnswer.
func (c *Conversation) Answer(ctx context.Context, requestID string, ans InputAnswer) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	if !c.queue.Held() {
		return ErrNoControl
	}
	c.mu.Lock()
	req := c.currentInput
	c.mu.Unlock()
	if req == nil {
		return ErrNoInputPending
	}
	if requestID != "" && requestID != req.ID {
		return ErrStaleInputRequest
	}
	return c.writeAnswer(ctx, req, ans)
}

// handleInputRequested records the pending request and tries to resolve it
// from the configured policy/handler; if neither resolves it, it surfaces the
// request to the client on Events(). Called from the watcher pump goroutine.
func (c *Conversation) handleInputRequested(req *turns.InputRequest) {
	if req == nil {
		return
	}
	c.mu.Lock()
	c.currentInput = req
	c.inputSurfaced = false
	c.inputUnresolved = nil
	c.mu.Unlock()

	if c.tryAutoDismissCodex(req) {
		// Built-in dismissal of a Codex startup interstitial. Keep currentInput
		// until the screen clears so the adapter's InputResolved drives the
		// clear + notify, same as a policy-resolved request.
		return
	}

	if c.tryResolveInput(req) {
		// Auto-answered server-side AND confirmed: answerAndConfirm has already
		// watched the dialog leave the screen, retrying from the live screen if
		// it did not. Keep currentInput until the adapter's InputResolved drives
		// the clear + notify, so the two agree on when the prompt is gone.
		return
	}
	// Not resolved. Either nothing wanted to answer it, or an answer was
	// written and the dialog would not take it — in which case
	// recordUnresolvedInput has latched the evidence and surfacing it below
	// also gives a live client the chance to answer it by hand.

	c.mu.Lock()
	c.inputSurfaced = true
	c.mu.Unlock()
	c.signalInputState()
	cr := toClientInputRequest(req)
	c.emit(ConversationEvent{Type: EventInputRequest, Input: &cr})
}

// handleInputResolved clears the pending request and notifies the client.
func (c *Conversation) handleInputResolved(_ *turns.InputRequest) {
	c.mu.Lock()
	had := c.currentInput
	c.currentInput = nil
	c.inputSurfaced = false
	c.inputUnresolved = nil
	c.mu.Unlock()
	c.signalInputState()
	if had == nil {
		return
	}
	cr := toClientInputRequest(had)
	c.emit(ConversationEvent{Type: EventInputResolved, Input: &cr})
}

// signalInputState wakes a Send blocked in waitReadyForSend so it re-checks
// input state. Non-blocking and nil-safe.
func (c *Conversation) signalInputState() {
	select {
	case c.inputStateCh <- struct{}{}:
	default:
	}
}

// inputAwaitingClient reports whether a prompt is pending that no policy or
// handler is resolving — i.e. Send should fail fast rather than block.
func (c *Conversation) inputAwaitingClient() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentInput != nil && c.inputSurfaced
}

// PendingInput returns the client-facing view of the interactive prompt
// currently awaiting the client, or nil when nothing is. It gates on the SAME
// condition as inputAwaitingClient (currentInput != nil && inputSurfaced) so
// the two never disagree: a request that is set but not yet surfaced — while it
// is being auto-dismissed or policy-resolved server-side — reports nil here,
// exactly as inputAwaitingClient reports false. PendingInput answers "what is
// awaiting me?"; inputAwaitingClient answers "is anything?".
func (c *Conversation) PendingInput() *InputRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentInput == nil || !c.inputSurfaced {
		return nil
	}
	cr := toClientInputRequest(c.currentInput)
	return &cr
}

// write sends keystrokes to the harness PTY (or the injected test sink).
func (c *Conversation) write(p []byte) error {
	if c.writeStdin != nil {
		_, err := c.writeStdin(p)
		return err
	}
	_, err := c.sess.WriteStdin(p)
	return err
}

// tryAutoDismissCodex clears a Codex startup interstitial (update notice,
// model migration) by writing its safe-dismiss keystrokes, unless the caller
// disabled auto-dismiss. Returns true when it wrote a dismissal. It matches
// only the interstitial request kinds, so Codex's real approval prompts fall
// through to the normal policy/handler/surface path.
func (c *Conversation) tryAutoDismissCodex(req *turns.InputRequest) bool {
	if c.opts.Harness != "codex" || c.opts.DisableCodexAutoDismiss {
		return false
	}
	// The update menu is surfaced to the client by default so it can choose
	// Update / Skip; only auto-Skip it when the caller opted in. The other
	// interstitials (model migration, menu-less notice) have no user choice and
	// stay auto-dismissed.
	if req.Kind == codex.KindUpdateNotice && !c.opts.AutoSkipCodexUpdateNotice {
		return false
	}
	keys, ok := codex.AutoDismissKeys(req)
	if !ok {
		return false
	}
	_ = c.write(keys)
	return true
}

// tryResolveInput auto-answers req from the policy or the in-process handler.
// Returns true if it wrote an answer.
func (c *Conversation) tryResolveInput(req *turns.InputRequest) bool {
	if opt := c.policyOption(req); opt != nil {
		// context.Background() for the same reason the handler branch below
		// uses it: this runs on the watcher pump goroutine, which has no caller
		// context. answerAndConfirm is bounded by its own render budget and
		// selects on c.closed, so Close still unblocks it.
		if err := c.answerAndConfirm(context.Background(), req, opt); err != nil {
			c.recordUnresolvedInput(err)
			return false
		}
		return true
	}
	if c.opts.OnInputRequest != nil {
		if ans, ok := c.opts.OnInputRequest(toClientInputRequest(req)); ok {
			// context.Background() rather than a threaded ctx: this runs on the
			// watcher pump goroutine (handleInputRequested), which has no caller
			// context to thread — Options.OnInputRequest is a callback, not a
			// request. Nothing is lost: the only ctx consumer downstream is
			// awaitComposerEcho, which already selects on c.closed, so Close
			// still unblocks it, and its own deadline bounds the rest.
			if err := c.writeAnswer(context.Background(), req, ans); err == nil {
				return true
			} else {
				c.recordUnresolvedInput(err)
			}
		}
	}
	return false
}

func (c *Conversation) policyOption(req *turns.InputRequest) *turns.InputOption {
	d, ok := c.opts.InputPolicy.resolve(req.Kind)
	if !ok {
		return nil
	}
	switch d.Kind {
	case DispositionAnswer:
		return findOption(req, d.OptionID)
	case DispositionDeny:
		return findOptionByAlias(req, "deny")
	default:
		return nil
	}
}

// writeAnswer translates an InputAnswer into keystrokes and writes them.
//
// Channel selection runs FIRST — before the free-text early-return — so the
// OptionIDs guards apply even to a single-select free-text prompt (which has
// zero options and would otherwise fall into the free-text branch and return
// before any validation). Precedence, evaluated top to bottom:
//  1. OptionIDs set AND OptionID set          → ErrConflictingAnswer
//  2. OptionIDs set AND req is single-select   → ErrNotMultiSelect
//  3. OptionIDs set AND req is MultiSelect      → multi-select toggle path
//  4. OptionIDs empty                          → single-select / free-text path
func (c *Conversation) writeAnswer(ctx context.Context, req *turns.InputRequest, ans InputAnswer) error {
	if len(ans.OptionIDs) > 0 {
		if ans.OptionID != "" {
			return ErrConflictingAnswer
		}
		if !req.MultiSelect {
			return ErrNotMultiSelect
		}
		return c.writeMultiSelect(req, ans.OptionIDs)
	}

	if len(req.Options) == 0 {
		// Free-text prompt: the answer is composer TEXT, so it loses to the same
		// paste-collapse Send does — text and submit key in one write can be taken
		// as pasted content and never acted on. Two writes with the composer echo
		// awaited in between (submit.go), mirroring the TypeScript twin at
		// meta-harness/src/chat/conversation.ts:2478-2483: ONE pre-write snapshot
		// feeds both the submit-key choice and the echo baseline.
		//
		// The option branches below are deliberately left as single writes: they
		// send short key sequences, not composer text, so there is nothing to
		// collapse and nothing to echo.
		preWriteScreen := c.screen.Snapshot().Text
		submit := submitKeyForHarness(c.opts.Harness, preWriteScreen)
		return c.writeMessageAndSubmit(ctx, ans.Text, preWriteScreen, submit)
	}
	// Single-select (this also handles an empty OptionIDs on a MultiSelect
	// request: findOption(req, "") returns nil → ErrUnknownOption, the decided
	// "empty multi-select answer unsupported" contract).
	opt := findOption(req, ans.OptionID)
	if opt == nil {
		return ErrUnknownOption
	}
	return c.answerAndConfirm(ctx, req, opt)
}

// writeMultiSelect resolves every id in ids to a distinct option, toggles each
// one in request order, then appends the harness submit key exactly once. All
// ids are resolved BEFORE any write, so an unknown id leaves the menu
// untouched. Duplicates are collapsed by resolved-option identity (not by the
// input string), so ["a","a"] or [id, matching-label] toggle the option once —
// toggling twice would net-deselect it on a toggle UI.
//
// KNOWN LATENT DEFECT, deliberately not fixed here: an UNNUMBERED option's Keys
// already end in the confirm key (claudecode.parseUnnumberedMenuOptions), so
// concatenating them and appending a submit key double-submits. splitNavKeys
// now makes the remedy a one-liner — drop each option's trailing confirm — but
// no menu this layer meets is both unnumbered and MultiSelect (trust_prompt
// never is), so the change would be untestable against a real screen and is
// left to whoever first meets one.
func (c *Conversation) writeMultiSelect(req *turns.InputRequest, ids []string) error {
	seen := make(map[string]bool, len(ids))
	var opts []*turns.InputOption
	for _, id := range ids {
		opt := findOption(req, id)
		if opt == nil {
			return ErrUnknownOption
		}
		if seen[opt.ID] {
			continue
		}
		seen[opt.ID] = true
		opts = append(opts, opt)
	}
	var keys []byte
	for _, opt := range opts {
		keys = append(keys, opt.Keys...)
	}
	keys = append(keys, submitKeyForHarness(c.opts.Harness, c.screen.Snapshot().Text)...)
	return c.write(keys)
}

func toClientInputRequest(req *turns.InputRequest) InputRequest {
	out := InputRequest{ID: req.ID, Kind: req.Kind, Prompt: req.Prompt, Header: req.Header, MultiSelect: req.MultiSelect}
	if len(req.Options) > 0 {
		out.Options = make([]InputOption, 0, len(req.Options))
		for _, o := range req.Options {
			out.Options = append(out.Options, InputOption{ID: o.ID, Alias: o.Alias, Label: o.Label, Description: o.Description})
		}
	}
	return out
}

// findOption matches by option ID, Alias, or (case-insensitively) Label.
func findOption(req *turns.InputRequest, s string) *turns.InputOption {
	if s == "" {
		return nil
	}
	ls := strings.ToLower(s)
	for i := range req.Options {
		o := &req.Options[i]
		if o.ID == s || strings.ToLower(o.Alias) == ls || strings.EqualFold(o.Label, s) {
			return o
		}
	}
	return nil
}

func findOptionByAlias(req *turns.InputRequest, alias string) *turns.InputOption {
	for i := range req.Options {
		if req.Options[i].Alias == alias {
			return &req.Options[i]
		}
	}
	return nil
}

// The answer used to be ONE write, fired ONCE, and never checked.
//
// Measured on claude-code 2.1.261: the folder-trust dialog's highlight moved to
// "Yes, I trust this folder" and then moved BACK to the default "No, exit" on
// the next redraw. The dialog stayed up, and because Adapter.OnScreen emits
// InputRequested only when the request ID CHANGES — and the ID hashes
// kind+prompt+labels, which the moving highlight does not alter — nothing ever
// fired again. The turn then ran to loom's 43-minute run deadline with an
// unanswered modal on screen. The keys were never wrong: replaying the exact
// bytes by hand into the same pane dismissed the dialog.
//
// So this file now does to an interactive ANSWER what submit.go does to a
// prompt: it OBSERVES the screen instead of trusting a write. Fixed delays are
// the wrong instrument — "the two writes race and the environment decides the
// winner" (submit.go) — and a 60 ms settle between the arrows and the Enter was
// measured to change nothing.
//
// Note the direction of each check, which differs from submit.go's on purpose:
//
//   - NAVIGATION is confirmed BEFORE the confirm key. An Enter pressed while
//     the marker sits on the wrong row selects that row, and on a trust dialog
//     the wrong row is "No, exit" — it kills the session. So a marker that
//     never lands means NO Enter at all, never a hopeful one.
//   - THE ANSWER is confirmed AFTER the fact, and retried. Re-pressing Enter on
//     a menu that already took it is not the same hazard as double-submitting a
//     prompt: the dialog is gone, so the retry is skipped entirely. What makes
//     the retry safe is that the keys are RECOMPUTED from the live screen —
//     replaying the original "\x1b[B" after the highlight has moved would walk
//     past the target row.
const (
	// answerAttempts bounds re-answers of a dialog that will not clear. Three
	// covers a repaint that ate one answer without turning a wedged dialog into
	// an unbounded keystroke loop.
	answerAttempts = 3
)

// errNavigationMissed is internal: the highlight never reached the target row,
// so the confirm key was deliberately not written. It is never returned to a
// caller — answerAndConfirm turns it into another attempt or, at the end, into
// an *InputUnresolvedError.
var errNavigationMissed = errors.New("chat: menu highlight never reached the target option")

// answerAndConfirm writes opt's keystrokes and confirms the harness acted on
// them, re-answering from the CURRENT screen up to answerAttempts times before
// giving up with an *InputUnresolvedError.
//
// Harnesses other than claude-code, and any Conversation with no screen (the
// unit tests that inject only a write sink), keep the historical single write:
// there is nothing to observe, and a check that cannot run must not become a
// check that always fails.
func (c *Conversation) answerAndConfirm(ctx context.Context, req *turns.InputRequest, opt *turns.InputOption) error {
	if c.screen == nil || c.opts.Harness != chatClaudeCode {
		return c.write(opt.Keys)
	}

	label := opt.Label
	keys := opt.Keys
	observed := c.screen.Snapshot().Text

	for attempt := 1; ; attempt++ {
		cur, err := c.writeAnswerKeys(ctx, label, keys)
		if cur != "" {
			observed = cur
		}
		switch {
		case err == nil:
			// The answer is delivered; did the dialog take it?
			cleared, seen, err := c.awaitScreen(ctx, func(s string) bool {
				return dialogCleared(req, s)
			})
			if seen != "" {
				observed = seen
			}
			if err != nil {
				return err
			}
			if cleared {
				return nil
			}
		case errors.Is(err, errNavigationMissed):
			// No Enter was written. Fall through to another attempt rather than
			// pressing into whatever row the marker is actually on.
		default:
			return err
		}

		if attempt >= answerAttempts {
			break
		}
		if err := c.answerBackoff(ctx, attempt); err != nil {
			return err
		}

		observed = c.screen.Snapshot().Text
		if dialogCleared(req, observed) {
			// It cleared during the backoff.
			return nil
		}
		next, ok := recomputeKeys(observed, label)
		if !ok {
			// The anchor is up but the menu no longer parses, so there is no
			// honest set of keys to send. Stop rather than replay stale ones.
			break
		}
		keys = next
	}

	return &InputUnresolvedError{
		Request:  toClientInputRequest(req),
		Observed: observed,
		Attempts: answerAttempts,
	}
}

// writeAnswerKeys delivers one answer, splitting navigation from the confirm
// key when there is navigation to confirm. It returns the last screen it read
// (for the error's evidence) and errNavigationMissed when it declined to press
// Enter.
func (c *Conversation) writeAnswerKeys(ctx context.Context, label string, keys []byte) (string, error) {
	nav, confirm, split := splitNavKeys(keys)
	if !split {
		return "", c.write(keys)
	}
	if err := c.write(nav); err != nil {
		return "", err
	}
	landed, seen, err := c.awaitScreen(ctx, func(s string) bool {
		return markerOnLabel(s, label)
	})
	if err != nil {
		return seen, err
	}
	if !landed {
		return seen, errNavigationMissed
	}
	return seen, c.write(confirm)
}

// splitNavKeys splits an option's keystrokes into the navigation prefix that
// moves the highlight and the trailing confirm key.
//
// It splits ONLY when there is navigation worth confirming. A numbered menu's
// "2\r" selects by digit regardless of where the highlight sits, so there is no
// marker to watch and no wrong row to land on — it stays one write, exactly as
// before. An unnumbered menu's keys are arrows plus Enter (see
// claudecode.parseUnnumberedMenuOptions), and those are the ones that need the
// split.
func splitNavKeys(keys []byte) (nav, confirm []byte, ok bool) {
	if len(keys) < 2 || keys[len(keys)-1] != '\r' {
		return nil, nil, false
	}
	nav = keys[:len(keys)-1]
	if !bytes.Contains(nav, []byte{0x1b}) {
		return nil, nil, false
	}
	return nav, keys[len(keys)-1:], true
}

// markerOnLabel reports whether the menu marker currently sits on label.
// Matching is by LABEL and never by index: claude-code 2.1.261 inverted the
// folder-trust option order, so the row that used to mean "proceed" now means
// "exit".
func markerOnLabel(text, label string) bool {
	got, ok := claudecode.HighlightedLabel(text)
	return ok && strings.EqualFold(got, label)
}

// dialogCleared reports whether THIS request's dialog has left the screen.
//
// It asks for the request's own anchor to be gone, not for DetectInput to fail:
// a dialog whose menu is mid-paint fails DetectInput while still being very
// much up, so DetectInput would read a repaint as success. And it is scoped to
// this request rather than to "any dialog" (claudecode.AnchorPresent) because a
// DIFFERENT dialog replacing this one is a resolution of this one — the adapter
// hashes a new ID for it and emits its own InputRequested, which starts its own
// answer. Retrying this request's keys into someone else's menu is exactly the
// wrong-row press the navigation check exists to prevent.
func dialogCleared(req *turns.InputRequest, text string) bool {
	if req.Prompt != "" {
		return !strings.Contains(text, req.Prompt)
	}
	return !claudecode.AnchorPresent(text)
}

// recomputeKeys re-derives the keystrokes for label from the screen as it looks
// NOW. This is what makes a retry safe: the highlight has usually moved by the
// time the first answer failed, and replaying the original arrows would walk
// past the target row instead of onto it.
func recomputeKeys(text, label string) ([]byte, bool) {
	req, ok := claudecode.DetectInput(text)
	if !ok {
		return nil, false
	}
	opt := findOption(req, label)
	if opt == nil || len(opt.Keys) == 0 {
		return nil, false
	}
	return opt.Keys, true
}

// answerBackoff waits between attempts, growing with the attempt number so a
// harness that is merely slow gets more room on the second try than the first.
// It returns early — with an error — on cancellation or Close, so a wedged
// dialog never holds a shutdown open.
func (c *Conversation) answerBackoff(ctx context.Context, attempt int) error {
	d := time.Duration(attempt) * c.permModeRenderBudget() / 4
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
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

// awaitScreen polls the live screen until want reports true or the render
// budget expires, and returns what it last read.
//
// It reuses permModeRenderBudget rather than inventing a timing idiom: it is
// the same question ("has the TUI repainted yet?"), the same order of
// magnitude, and the same unexported override the hermetic tests already shrink.
//
// The select's arms carry NO returns — they only WAKE the loop, and every
// decision is taken in the body, in order: want first (so work that lands on
// the final tick is still honoured), then ctx and Close (an aborted wait must
// never be reported as a quiet expiry), then the budget. See closedNow for the
// measured bugs that shape costs when it is written the other way round.
func (c *Conversation) awaitScreen(ctx context.Context, want func(string) bool) (bool, string, error) {
	budget := c.permModeRenderBudget()
	expiry := time.Now().Add(budget)
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	ticker := time.NewTicker(permModePollInterval(budget))
	defer ticker.Stop()

	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()

	for {
		cur := c.screen.Snapshot().Text
		if want(cur) {
			return true, cur, nil
		}
		if ctx.Err() != nil {
			return false, cur, ctx.Err()
		}
		if closedNow(c.closed) {
			return false, cur, ErrClosed
		}
		if time.Now().After(expiry) {
			return false, cur, nil
		}
		select {
		case <-ctx.Done():
		case <-c.closed:
		case <-deadline.C:
		case <-notifyCh:
		case <-ticker.C:
		}
	}
}
