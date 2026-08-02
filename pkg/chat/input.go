package chat

import (
	"context"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns"
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
	return c.writeAnswer(req, ans)
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
	c.mu.Unlock()

	if c.tryAutoDismissCodex(req) {
		// Built-in dismissal of a Codex startup interstitial. Keep currentInput
		// until the screen clears so the adapter's InputResolved drives the
		// clear + notify, same as a policy-resolved request.
		return
	}

	if c.tryResolveInput(req) {
		// Auto-answered server-side: keep currentInput until the dialog
		// clears so a slow/ineffective answer can be retried, and let the
		// adapter's InputResolved drive the clear + notify.
		return
	}

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
		_ = c.write(opt.Keys)
		return true
	}
	if c.opts.OnInputRequest != nil {
		if ans, ok := c.opts.OnInputRequest(toClientInputRequest(req)); ok {
			if err := c.writeAnswer(req, ans); err == nil {
				return true
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
func (c *Conversation) writeAnswer(req *turns.InputRequest, ans InputAnswer) error {
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
		// Free-text prompt: send text + the harness submit key.
		submit := submitKeyForHarness(c.opts.Harness, c.screen.Snapshot().Text)
		return c.write(append([]byte(ans.Text), submit...))
	}
	// Single-select (this also handles an empty OptionIDs on a MultiSelect
	// request: findOption(req, "") returns nil → ErrUnknownOption, the decided
	// "empty multi-select answer unsupported" contract).
	opt := findOption(req, ans.OptionID)
	if opt == nil {
		return ErrUnknownOption
	}
	return c.write(opt.Keys)
}

// writeMultiSelect resolves every id in ids to a distinct option, toggles each
// one in request order, then appends the commit sequence exactly once. All
// ids are resolved BEFORE any write, so an unknown id leaves the menu
// untouched. Duplicates are collapsed by resolved-option identity (not by the
// input string), so ["a","a"] or [id, matching-label] toggle the option once —
// toggling twice would net-deselect it on a toggle UI.
//
// The commit sequence is the request's own SubmitKeys when the producing
// adapter supplied one, and the harness's generic submit key otherwise. The
// distinction matters: a checkbox widget's commit control is usually NOT the
// composer's Enter (Claude Code's clarifying-question dialog commits on Tab —
// see turns.InputRequest.SubmitKeys), and sending Enter there would toggle the
// highlighted row instead of committing.
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
	submit := req.SubmitKeys
	if len(submit) == 0 {
		submit = submitKeyForHarness(c.opts.Harness, c.screen.Snapshot().Text)
	}
	keys = append(keys, submit...)
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
