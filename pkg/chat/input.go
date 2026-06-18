package chat

import (
	"context"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// InputRequest is the client-facing view of a blocking interactive prompt
// the harness is showing (e.g. Claude Code's folder-trust dialog). It mirrors
// turns.InputRequest but deliberately omits the per-option keystrokes: the
// client answers semantically by option ID or Alias and the chat layer owns
// the translation to keys.
type InputRequest struct {
	ID      string        `json:"id"`
	Kind    string        `json:"kind"`
	Prompt  string        `json:"prompt"`
	Options []InputOption `json:"options,omitempty"`
}

// InputOption is one selectable choice in an InputRequest.
type InputOption struct {
	ID    string `json:"id"`
	Alias string `json:"alias,omitempty"`
	Label string `json:"label"`
}

// InputAnswer is how a caller answers an InputRequest. Set OptionID (an
// option ID or Alias) for menu/confirm/trust prompts; set Text for free-text
// ("text_input") prompts.
type InputAnswer struct {
	OptionID string `json:"option_id,omitempty"`
	Text     string `json:"text,omitempty"`
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
// ErrUnknownOption.
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

// write sends keystrokes to the harness PTY (or the injected test sink).
func (c *Conversation) write(p []byte) error {
	if c.writeStdin != nil {
		_, err := c.writeStdin(p)
		return err
	}
	_, err := c.sess.WriteStdin(p)
	return err
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
func (c *Conversation) writeAnswer(req *turns.InputRequest, ans InputAnswer) error {
	if len(req.Options) == 0 {
		// Free-text prompt: send text + the harness submit key.
		submit := submitKeyForHarness(c.opts.Harness, c.screen.Snapshot().Text)
		return c.write(append([]byte(ans.Text), submit...))
	}
	opt := findOption(req, ans.OptionID)
	if opt == nil {
		return ErrUnknownOption
	}
	return c.write(opt.Keys)
}

func toClientInputRequest(req *turns.InputRequest) InputRequest {
	out := InputRequest{ID: req.ID, Kind: req.Kind, Prompt: req.Prompt}
	if len(req.Options) > 0 {
		out.Options = make([]InputOption, 0, len(req.Options))
		for _, o := range req.Options {
			out.Options = append(out.Options, InputOption{ID: o.ID, Alias: o.Alias, Label: o.Label})
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
