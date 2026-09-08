package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// gracefulQuitWait bounds how long RunTurn waits for the harness to exit on its
// own after the quit sequence before conv.Close escalates to a signal.
const gracefulQuitWait = 3 * time.Second

// gracefulQuit asks the harness to exit cleanly via Conversation.Quit (its
// turns.Quitter sequence — for Claude Code, the "/quit" slash command) so it can
// flush/persist its transcript before termination, and waits briefly for the
// process to go away. Returns true if the quit was sent: the caller may then
// re-read a freshly flushed transcript AND the harness session id the durable
// line tap captured from the exit output. A harness with no quit sequence is a
// no-op returning false — conv.Close then stops it with a signal as before.
//
// Quit (not the old wrapper.AcquireWriter dance) is what makes this work: the
// chat Conversation holds the PTY writer lock for its whole life, so an external
// AcquireWriter here always failed and the quit keys were never sent — the
// harness only ever died on Close's SIGTERM.
func gracefulQuit(conv *chat.Conversation) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gracefulQuitWait)
	defer cancel()
	if err := conv.Quit(ctx); err != nil {
		return false
	}

	done := make(chan struct{})
	go func() { _, _ = conv.Wrapper().Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(gracefulQuitWait):
	}
	return true
}

// ErrTurnErrored is returned by RunTurn when the harness reports that the
// submitted assistant turn ended in an errored state. The returned TurnResult
// carries the errored turn and any retry metadata surfaced by the adapter.
var ErrTurnErrored = errors.New("harness: turn errored")

// TurnConfig configures RunTurn, the one-shot interactive-turn entrypoint.
//
// RunTurn starts an interactive harness, sends Prompt through the PTY, waits
// for the adapter to report that the assistant turn completed, and then either
// stops the harness process or returns the live Conversation to the caller.
//
// This is intentionally turn-lifecycle based. It is the API shape callers use
// when they want normal interactive harness behavior to look like a single
// non-interactive job without relying on print/headless modes.
type TurnConfig struct {
	// Harness names the wrapper harness/profile ("claude", "codex",
	// ...). It is also used to pick the chat adapter unless
	// TurnHarness is set.
	Harness string

	// TurnHarness overrides the chat adapter name. Most callers leave this
	// empty. It exists for naming mismatches such as wrapper harness "claude"
	// using the chat adapter "claude-code".
	TurnHarness string

	// BinaryPath is the harness executable. Required.
	BinaryPath string

	// Args are passed verbatim to the harness. For Claude Code this is the
	// normal interactive arg set, e.g. {"--dangerously-skip-permissions"},
	// not print/headless args.
	Args []string

	// WorkingDir and Env are passed through to the harness process.
	WorkingDir string
	Env        []string

	// Effort, Model and PermissionMode are execution-mode knobs forwarded to
	// chat.Options → wrapper.Config (Claude Code --effort/--model, Codex config
	// overrides). Empty leaves the harness default.
	Effort string
	Model  string

	// PermissionMode is the launch-time permission rung forwarded to
	// chat.Options → wrapper.Config, which translates it into the harness's
	// native flag (claude --permission-mode, codex -s/-a). The canonical rungs
	// are "plan", "manual", "ask", "auto" and "bypass"; per-harness native
	// spellings are also accepted. Empty leaves the harness default. Values are
	// validated by wrapper.Config, not here.
	//
	// Restrictive rungs (`plan`, `manual`, `ask`) are fully enforced only when
	// a human is at the TUI (passthrough, or `run` from a terminal for codex).
	// Under `structured-run` and unattended `run`, claude's permission dialogs
	// are not detected (the turn stalls to the deadline) and codex's approval
	// prompts are auto-approved (only the `-s` sandbox axis still binds).
	PermissionMode string

	// Prompt is submitted as one user message.
	Prompt string

	// ExitAfterTurn stops the interactive harness after the submitted turn
	// completes or errors. When false, the returned TurnResult.Conversation is
	// live and the caller owns closing it.
	ExitAfterTurn bool

	// Cols and Rows configure the virtual terminal used by the chat layer.
	// Zero values use chat.Open defaults.
	Cols, Rows int

	// EventBuffer sizes the chat event channel. Zero uses chat.Open default.
	EventBuffer int

	// InputPolicy pre-resolves blocking interactive prompts (e.g. the
	// folder-trust dialog) so a one-shot run proceeds unattended. Without it,
	// an unanswerable prompt fails the run with chat.ErrInputPending rather
	// than hanging to the deadline. For an untrusted worktree, set
	// {ByKind: {"trust_prompt": {Kind: "answer", OptionID: "proceed"}}}.
	//
	// claude-code emits two distinct kinds here, and the entry above covers only
	// the first:
	//   - "trust_prompt" (claudecode.KindTrustPrompt) — the folder-trust dialog,
	//     in either phrasing;
	//   - "bypass_acceptance" (claudecode.KindBypassAcceptance) — the
	//     --dangerously-skip-permissions ("Bypass Permissions mode") acceptance
	//     screen, which needs its OWN entry. A folder-trust entry no longer
	//     accepts a skip-all-permissions launch by accident.
	// A policy alone also cannot gate what is never surfaced: claude's per-tool
	// permission dialog is not detected, so it reaches no policy at all and
	// stalls the turn to the deadline.
	InputPolicy *chat.InputPolicy

	// OnInputRequest is an in-process resolver for prompts the policy didn't
	// answer. Go-only (not exposed over transports).
	OnInputRequest func(chat.InputRequest) (chat.InputAnswer, bool)

	// AutoSkipCodexUpdateNotice auto-Skips Codex's "Update available!" menu
	// instead of surfacing it. A one-shot run has no live client to answer the
	// menu, so leaving it false would wedge the run on the pending prompt; set
	// true for unattended/headless callers. See chat.Options for the details.
	AutoSkipCodexUpdateNotice bool

	// Output, when non-nil, receives a best-effort copy of PTY output observed
	// after RunTurn opens the conversation. This is diagnostic/display output;
	// turn completion is driven by the screen adapter, not this writer.
	Output io.Writer
}

// TurnResult is the outcome of a RunTurn call.
type TurnResult struct {
	// Turn is the assistant turn that completed or errored.
	Turn chat.Turn

	// Session is the chat-level session record after the turn. If the adapter
	// extracted a native harness session ID, it is stored in
	// Session.HarnessSessionID.
	Session chat.Session

	// History is Conversation.History after the turn. It may be backed by the
	// harness transcript when the adapter supports transcript reading and a
	// harness session ID was captured; otherwise it is the chat store fallback.
	History []chat.Turn

	// HistorySource reports which of those two paths produced History. The
	// presence of turns alone can't distinguish them — the store fallback also
	// returns non-empty slices — so callers that care about fidelity (e.g. the
	// run command's debug logging) must consult this field, not len(History).
	HistorySource chat.HistorySource

	// WrapperResult is populated when ExitAfterTurn is true and the harness
	// process was stopped before returning. This is the raw process-level
	// outcome from the lower-level wrapper; after RunTurn intentionally stops a
	// still-live interactive harness following a successful turn, either
	// StatusIdle (the harness honored the graceful quit and exited cleanly) or
	// StatusInterrupted (it ignored the quit and was terminated) is expected —
	// which one appears is a property of the harness build, not of the turn.
	// Callers should use Turn.State / ErrTurnErrored for the
	// turn-level outcome.
	WrapperResult wrapper.Result

	// ProcessStoppedAfterTurn reports that RunTurn intentionally stopped the
	// interactive harness after the submitted turn reached a terminal state.
	// This distinguishes expected process interruption from turn failure.
	ProcessStoppedAfterTurn bool

	// Conversation is non-nil only when ExitAfterTurn is false. The caller owns
	// the live interactive process and must eventually call Close.
	Conversation *chat.Conversation
}

// RunTurn runs one interactive harness turn and returns when that turn reaches
// a completed or errored state.
func RunTurn(ctx context.Context, cfg TurnConfig) (TurnResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Harness == "" {
		return TurnResult{}, fmt.Errorf("%w: Harness is required", chat.ErrInvalidOptions)
	}
	if cfg.BinaryPath == "" {
		return TurnResult{}, fmt.Errorf("%w: BinaryPath is required", chat.ErrInvalidOptions)
	}

	store := memstore.New()
	conv, err := chat.Open(ctx, chat.Options{
		Harness:        turnHarnessName(cfg),
		BinaryPath:     cfg.BinaryPath,
		Args:           cfg.Args,
		WorkingDir:     cfg.WorkingDir,
		Env:            cfg.Env,
		Effort:         cfg.Effort,
		Model:          cfg.Model,
		PermissionMode: cfg.PermissionMode,
		Cols:           cfg.Cols,
		Rows:           cfg.Rows,
		Store:          store,
		EventBuffer:    cfg.EventBuffer,
		InputPolicy:    cfg.InputPolicy,
		OnInputRequest: cfg.OnInputRequest,

		AutoSkipCodexUpdateNotice: cfg.AutoSkipCodexUpdateNotice,
	})
	if err != nil {
		return TurnResult{}, err
	}

	var detach func()
	if cfg.Output != nil {
		detach = conv.Wrapper().AttachOutput(cfg.Output)
	}

	result, err := runConversationTurn(ctx, conv, store, cfg.Prompt)
	if err != nil {
		if detach != nil {
			detach()
		}
		_ = conv.Close(context.Background())
		return result, err
	}

	if cfg.ExitAfterTurn {
		return stopHarnessAfterTurn(conv, store, result, detach)
	}

	result.Conversation = conv
	return result, nil
}

// stopHarnessAfterTurn handles the ExitAfterTurn path: refresh the result from a
// graceful quit, then stop the process and capture its raw wrapper outcome.
func stopHarnessAfterTurn(conv *chat.Conversation, store chat.Store, result TurnResult, detach func()) (TurnResult, error) {
	result.ProcessStoppedAfterTurn = true
	refreshResultAfterQuit(conv, store, &result)
	if err := conv.Close(context.Background()); err != nil {
		if detach != nil {
			detach()
		}
		return result, err
	}
	wres, werr := conv.Wrapper().Wait()
	if detach != nil {
		detach()
	}
	result.WrapperResult = wres
	return result, werr
}

// refreshResultAfterQuit asks the harness to exit gracefully first (so it can
// flush/persist). The graceful exit prints the harness session id, which the
// durable line tap captures into the stored Session — so after the quit we
// refresh result.Session (now carrying HarnessSessionID) and re-read History,
// which is transcript-backed once that id is known. conv.Close still guarantees
// termination if the harness ignores the quit.
func refreshResultAfterQuit(conv *chat.Conversation, store chat.Store, result *TurnResult) {
	if !gracefulQuit(conv) {
		return
	}
	if s, serr := store.GetSession(context.Background(), conv.SessionID()); serr == nil && s != nil {
		result.Session = *s
	}
	if h, src, herr := conv.HistoryWithSource(context.Background()); herr == nil && len(h) > 0 {
		result.History = h
		result.HistorySource = src
	}
}

func runConversationTurn(ctx context.Context, conv *chat.Conversation, store chat.Store, prompt string) (TurnResult, error) {
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		return TurnResult{}, err
	}
	defer release()

	turnID, err := conv.Send(ctx, prompt)
	if err != nil {
		return TurnResult{}, err
	}

	for {
		select {
		case <-ctx.Done():
			return snapshotTurnResult(context.Background(), conv, store, chat.Turn{}), ctx.Err()
		case ev, ok := <-conv.Events():
			if !ok {
				return snapshotTurnResult(context.Background(), conv, store, chat.Turn{}), chat.ErrClosed
			}
			if ev.Err != nil {
				return snapshotTurnResult(context.Background(), conv, store, ev.Turn), ev.Err
			}
			if ev.Turn.ID != turnID {
				continue
			}
			switch ev.Turn.State {
			case chat.TurnStateComplete:
				return snapshotTurnResult(ctx, conv, store, ev.Turn), nil
			case chat.TurnStateErrored:
				return snapshotTurnResult(ctx, conv, store, ev.Turn), ErrTurnErrored
			}
		}
	}
}

func snapshotTurnResult(ctx context.Context, conv *chat.Conversation, store chat.Store, turn chat.Turn) TurnResult {
	session, _ := store.GetSession(ctx, conv.SessionID())
	history, source, err := conv.HistoryWithSource(ctx)
	if err != nil {
		history, _ = store.ListTurns(ctx, conv.SessionID())
		source = chat.HistorySourceStore
	}

	var sess chat.Session
	if session != nil {
		sess = *session
	}
	return TurnResult{Turn: turn, Session: sess, History: history, HistorySource: source}
}

func turnHarnessName(cfg TurnConfig) string {
	if cfg.TurnHarness != "" {
		return cfg.TurnHarness
	}
	switch cfg.Harness {
	case "claude":
		return "claude-code"
	default:
		return cfg.Harness
	}
}
