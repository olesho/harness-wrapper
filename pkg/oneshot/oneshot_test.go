package oneshot_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/oneshot"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
)

// fakeBin builds the scriptable fake harness once per process, skipping when the
// Go toolchain is unavailable.
func fakeBin(t *testing.T) string {
	t.Helper()
	p, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	return p
}

// scriptEnv marshals a script to a temp file and returns the process env that
// points the fake at it — passed to RunOneShot via oneshot.Config.Env.
func scriptEnv(t *testing.T, s fakeharness.Script) []string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	p := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return append(os.Environ(), fakeharness.EnvVar+"="+p)
}

// TestRunOneShotDetailed_Matrix is the acceptance matrix: each fakeharness
// scenario drives pkg/oneshot end-to-end and asserts the STATUS and (where it
// distinguishes an arm) the REASON, plus the exit code derived via
// turnproto.ExitCode(status) — several arms share exit 1 but differ in reason.
func TestRunOneShotDetailed_Matrix(t *testing.T) {
	const prompt = "ship the turn API"
	bin := fakeBin(t)

	t.Run("completed", func(t *testing.T) {
		// Exercises the err == nil + Turn.State == TurnStateComplete arm: a clean
		// completion maps to StatusCompleted, exit 0, reply populated.
		env := scriptEnv(t, fakeharness.New("claude-code").
			Idle().
			AwaitSubmit().
			Working(30, "Working").
			Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
			StayAliveUntilStopped().
			Build())

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		out, err := oneshot.RunOneShotDetailed(ctx, cfg(t, bin, env, prompt))
		if err != nil {
			t.Fatalf("RunOneShotDetailed returned a non-nil error for a classified outcome: %v", err)
		}
		if out.Status != turnproto.StatusCompleted {
			t.Fatalf("status = %q, want %q", out.Status, turnproto.StatusCompleted)
		}
		if got := turnproto.ExitCode(out.Status); got != turnproto.ExitOK {
			t.Errorf("ExitCode = %d, want %d", got, turnproto.ExitOK)
		}
		if !strings.Contains(out.Reply, "assistant reply: "+prompt) {
			t.Errorf("reply = %q, want it to contain %q", out.Reply, "assistant reply: "+prompt)
		}
	})

	t.Run("errored drives the ErrTurnErrored arm", func(t *testing.T) {
		// An "API Error: 429 …" frame: the wrapper's classifier turns it into a
		// Blocked event → TurnStateErrored → RunTurn returns harness.ErrTurnErrored.
		// Classify must land on the ErrTurnErrored arm (status errored, reason =
		// the turn's Reason), NOT the chat.ErrClosed or default mid-turn arm — all
		// three exit 1 but carry different reasons.
		env := scriptEnv(t, fakeharness.New("claude-code").
			Idle().
			AwaitSubmit().
			Raw(0, "API Error: 429 Too Many Requests. Retry after 30 seconds.").
			Build())

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		out, err := oneshot.RunOneShotDetailed(ctx, cfg(t, bin, env, prompt))
		if err != nil {
			t.Fatalf("RunOneShotDetailed returned a non-nil error for a classified outcome: %v", err)
		}
		if out.Status != turnproto.StatusErrored {
			t.Fatalf("status = %q, want %q", out.Status, turnproto.StatusErrored)
		}
		if got := turnproto.ExitCode(out.Status); got != turnproto.ExitError {
			t.Errorf("ExitCode = %d, want %d", got, turnproto.ExitError)
		}
		// The ErrTurnErrored arm carries the turn's own Reason (the api-error
		// detail) — the default/ErrClosed arms would carry a chat error string.
		if !strings.Contains(strings.ToLower(out.Reason), "api error 429") {
			t.Errorf("reason = %q, want the turn's api-error reason (ErrTurnErrored arm)", out.Reason)
		}
		if out.Reply != "" {
			t.Errorf("reply = %q, want empty on a non-completed status", out.Reply)
		}
	})

	t.Run("startup_error", func(t *testing.T) {
		// A pre-turn failure: an unresolved harness binary. RunTurn fails before a
		// chat session is opened, so res.Session.ID == "" drives the startup arm.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		badBin := filepath.Join(t.TempDir(), "does-not-exist-harness")
		out, err := oneshot.RunOneShotDetailed(ctx, cfg(t, badBin, os.Environ(), prompt))
		if err != nil {
			t.Fatalf("RunOneShotDetailed returned a non-nil error for a classified outcome: %v", err)
		}
		if out.Status != turnproto.StatusStartupError {
			t.Fatalf("status = %q, want %q", out.Status, turnproto.StatusStartupError)
		}
		if got := turnproto.ExitCode(out.Status); got != turnproto.ExitError {
			t.Errorf("ExitCode = %d, want %d", got, turnproto.ExitError)
		}
		if out.HarnessSessionID != "" {
			t.Errorf("HarnessSessionID = %q, want empty on a startup error", out.HarnessSessionID)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		// The library owns no timeout: the CALLER supplies a short-deadline ctx.
		// The fake reaches its prompt and holds (StayAlive) without ever replying,
		// so the turn never completes and ctx.Err() == DeadlineExceeded drives the
		// deadline arm (exit 124). This row must let a real short timeout elapse to
		// stay hermetic — hence the deliberately small, but spawn-tolerant, budget.
		env := scriptEnv(t, fakeharness.New("claude-code").
			Idle().
			AwaitSubmit().
			StayAliveUntilStopped().
			Build())

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		status, err := oneshot.RunOneShot(ctx, cfg(t, bin, env, prompt))
		if err != nil {
			t.Fatalf("RunOneShot returned a non-nil error for a classified outcome: %v", err)
		}
		if status != turnproto.StatusDeadline {
			t.Fatalf("status = %q, want %q", status, turnproto.StatusDeadline)
		}
		if got := turnproto.ExitCode(status); got != turnproto.ExitDeadline {
			t.Errorf("ExitCode = %d, want %d", got, turnproto.ExitDeadline)
		}
	})
}

// cfg builds a headless oneshot.Config for the fakeharness-driven matrix.
func cfg(t *testing.T, bin string, env []string, prompt string) oneshot.Config {
	t.Helper()
	return oneshot.Config{
		Harness:    "claude",
		BinaryPath: bin,
		WorkingDir: t.TempDir(),
		Env:        env,
		Prompt:     prompt,
	}
}

// TestClassify_Arms pins EACH branch of Classify on synthetic (res, err) pairs —
// unambiguous where an end-to-end fake cannot cheaply disambiguate the arms that
// key purely on the returned error (ErrClosed vs default vs ErrTurnErrored). It
// is the guard that the (TurnResult, error) → status/reason core is preserved
// VERBATIM, including the err == nil defensive fallback.
func TestClassify_Arms(t *testing.T) {
	tests := []struct {
		name       string
		res        harness.TurnResult
		err        error
		wantStatus turnproto.TurnStatus
		wantReason string
	}{
		{
			name:       "nil err + complete state → completed",
			res:        harness.TurnResult{Turn: chat.Turn{State: chat.TurnStateComplete}},
			err:        nil,
			wantStatus: turnproto.StatusCompleted,
			wantReason: "",
		},
		{
			name:       "nil err + unexpected state → defensive errored fallback",
			res:        harness.TurnResult{Turn: chat.Turn{State: chat.TurnStateErrored}},
			err:        nil,
			wantStatus: turnproto.StatusErrored,
			wantReason: "turn ended in unexpected state",
		},
		{
			name:       "deadline exceeded → deadline",
			res:        harness.TurnResult{},
			err:        context.DeadlineExceeded,
			wantStatus: turnproto.StatusDeadline,
			wantReason: "",
		},
		{
			name:       "ErrTurnErrored with reason → errored, carries reason",
			res:        harness.TurnResult{Turn: chat.Turn{Reason: "api error 429: Too Many Requests"}},
			err:        harness.ErrTurnErrored,
			wantStatus: turnproto.StatusErrored,
			wantReason: "api error 429: Too Many Requests",
		},
		{
			name:       "ErrTurnErrored with empty reason → errored, default reason",
			res:        harness.TurnResult{},
			err:        harness.ErrTurnErrored,
			wantStatus: turnproto.StatusErrored,
			wantReason: "turn errored",
		},
		{
			name:       "ErrClosed → errored, carries the error text",
			res:        harness.TurnResult{},
			err:        chat.ErrClosed,
			wantStatus: turnproto.StatusErrored,
			wantReason: chat.ErrClosed.Error(),
		},
		{
			name:       "default arm with a session opened → mid-turn errored",
			res:        harness.TurnResult{Session: chat.Session{ID: "sess-1"}},
			err:        errors.New("mid-turn boom"),
			wantStatus: turnproto.StatusErrored,
			wantReason: "mid-turn boom",
		},
		{
			name:       "default arm with no session → startup_error",
			res:        harness.TurnResult{},
			err:        errors.New("spawn failed"),
			wantStatus: turnproto.StatusStartupError,
			wantReason: "spawn failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason := oneshot.Classify(tt.res, tt.err)
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
			// Exit is derived ONLY via turnproto.ExitCode — the single table.
			if got, want := turnproto.ExitCode(status), wantExit(tt.wantStatus); got != want {
				t.Errorf("ExitCode(%q) = %d, want %d", status, got, want)
			}
		})
	}
}

func wantExit(s turnproto.TurnStatus) int {
	switch s {
	case turnproto.StatusCompleted:
		return turnproto.ExitOK
	case turnproto.StatusDeadline:
		return turnproto.ExitDeadline
	default:
		return turnproto.ExitError
	}
}

// TestRunOneShot_ErrorContract asserts the parity-defining error contract: a
// non-nil error appears ONLY for an unclassifiable/infra failure (an invalid
// Config), never for a classified outcome. It mirrors pkg/env.RunStructuredTurn.
func TestRunOneShot_ErrorContract(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  oneshot.Config
	}{
		{"empty harness", oneshot.Config{BinaryPath: "/bin/true", Prompt: "hi"}},
		{"empty binary path", oneshot.Config{Harness: "claude", Prompt: "hi"}},
		{"empty prompt", oneshot.Config{Harness: "claude", BinaryPath: "/bin/true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := oneshot.RunOneShot(ctx, tc.cfg)
			if err == nil {
				t.Fatalf("RunOneShot(%s) err = nil, want a non-nil infra error", tc.name)
			}
			if status != "" {
				t.Errorf("status = %q, want empty (no usable outcome) on an infra error", status)
			}
		})
	}
}

// TestReply_LabelsBySourceNotPresence is the regression relocated from the run
// command: the store fallback also returns a non-empty History, so Reply must key
// the source on res.HistorySource, not on len(History).
func TestReply_LabelsBySourceNotPresence(t *testing.T) {
	transcriptTurns := []chat.Turn{
		{Role: chat.RoleUser, Text: "hi"},
		{Role: chat.RoleAssistant, Text: "full transcript reply\nline 2"},
	}
	storeTurns := []chat.Turn{
		{Role: chat.RoleUser, Text: "hi"},
		{Role: chat.RoleAssistant, Text: "truncated screen tail"},
	}

	tests := []struct {
		name string
		res  harness.TurnResult
		want string
	}{
		{
			name: "transcript-backed history returns transcript text",
			res: harness.TurnResult{
				History:       transcriptTurns,
				HistorySource: chat.HistorySourceTranscript,
				Turn:          chat.Turn{Text: "screen tail"},
			},
			want: "full transcript reply\nline 2",
		},
		{
			name: "store-backed history falls through to screen extract",
			res: harness.TurnResult{
				History:       storeTurns,
				HistorySource: chat.HistorySourceStore,
				Turn:          chat.Turn{Text: "clean screen reply"},
			},
			want: "clean screen reply",
		},
		{
			name: "store-backed with empty Turn.Text falls back to recorded assistant turn",
			res: harness.TurnResult{
				History:       storeTurns,
				HistorySource: chat.HistorySourceStore,
				Turn:          chat.Turn{Text: ""},
			},
			want: "truncated screen tail",
		},
		{
			name: "transcript source but no assistant text falls through to screen",
			res: harness.TurnResult{
				History:       []chat.Turn{{Role: chat.RoleUser, Text: "hi"}},
				HistorySource: chat.HistorySourceTranscript,
				Turn:          chat.Turn{Text: "screen reply"},
			},
			want: "screen reply",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := oneshot.Reply(tt.res); got != tt.want {
				t.Fatalf("Reply = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAutoAcceptAnswer covers the shared three-tier fallback (relocated from the
// run command): affirmative option, else first option, else decline.
func TestAutoAcceptAnswer(t *testing.T) {
	tests := []struct {
		name       string
		req        chat.InputRequest
		wantOK     bool
		wantOption string
	}{
		{
			name: "affirmative option chosen when present",
			req: chat.InputRequest{Options: []chat.InputOption{
				{ID: "cancel", Label: "Cancel"},
				{ID: "go", Label: "Yes, proceed"},
			}},
			wantOK:     true,
			wantOption: "go",
		},
		{
			name: "first option when no affirmative match",
			req: chat.InputRequest{Options: []chat.InputOption{
				{ID: "one", Label: "Option one"},
				{ID: "two", Label: "Option two"},
			}},
			wantOK:     true,
			wantOption: "one",
		},
		{
			name:   "zero-option prompt declines",
			req:    chat.InputRequest{Prompt: "free text?"},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ans, ok := oneshot.AutoAcceptAnswer(tt.req)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && ans.OptionID != tt.wantOption {
				t.Fatalf("OptionID = %q, want %q", ans.OptionID, tt.wantOption)
			}
		})
	}
}

// TestAutoAcceptAnswer_CodexApproval pins the CURRENT unattended policy for
// codex's genuine command / apply-patch approval dialog, which HARNESS-WRAPPER-75
// made reachable for the first time: before the KindApproval port the dialog was
// never surfaced as a structured request (the prompt text was typed blindly into
// it instead), so AutoAcceptAnswer never saw one.
//
// The behavior pinned here — selecting "Yes, proceed" — is the existing designed
// unattended policy (it already applies to claude-code's trust_prompt), NOT a
// decision made by this change. The pin exists so any future move to a
// deny-by-default unattended stance is deliberate and shows up as a failing test
// rather than a silent behavioral drift.
func TestAutoAcceptAnswer_CodexApproval(t *testing.T) {
	data, err := os.ReadFile("../../test/corpus/codex/approval-command/bytes.raw")
	if err != nil {
		t.Fatalf("test/corpus/codex/approval-command/bytes.raw is REQUIRED: %v", err)
	}
	scr := screen.New(120, 40)
	_, _ = scr.Write(data)
	req, ok := codex.DetectInput(scr.Snapshot().Text)
	if !ok {
		t.Fatal("the live approval dialog did not classify; this pin would be vacuous")
	}

	// Mirrors pkg/chat's toClientInputRequest (unexported): the client-facing DTO
	// is built field-by-field, so Keys/Highlighted never cross this boundary.
	cr := chat.InputRequest{ID: req.ID, Kind: req.Kind, Prompt: req.Prompt}
	for _, o := range req.Options {
		cr.Options = append(cr.Options, chat.InputOption{ID: o.ID, Alias: o.Alias, Label: o.Label})
	}

	ans, ok := oneshot.AutoAcceptAnswer(cr)
	if !ok {
		t.Fatal("AutoAcceptAnswer declined a codex approval; the unattended path would " +
			"wedge on chat.ErrInputPending")
	}
	if ans.OptionID != "1" {
		t.Errorf("OptionID = %q, want %q (the 'Yes, proceed' row) — if this changed "+
			"deliberately, update the comment above; unattended runs now answer codex "+
			"approvals differently", ans.OptionID, "1")
	}
}

// TestAutoAcceptAnswer_SelectorTrustDialog pins the unattended path against
// claude 2.1.251's UNNUMBERED folder-trust dialog. The stakes differ from the
// numbered case: the rows are in the order claude paints them, "No, exit"
// FIRST, so AutoAcceptAnswer's second tier (fall through to Options[0]) would
// answer "exit" and quit the CLI. Only the affirmative tier gets this right,
// and this pin is what keeps it wired.
func TestAutoAcceptAnswer_SelectorTrustDialog(t *testing.T) {
	const frame = "Accessing workspace:\n" +
		"/private/tmp/trustrepo\n" +
		"Quick safety check: Is this a project you created or one you trust? \u2026\n" +
		"Claude Code'll be able to read, edit, and execute files here.\n" +
		"Security guide\n" +
		" \u276f No, exit\n" +
		"   Yes, I trust this folder\n" +
		"Enter to confirm \u00b7 Esc to cancel\n"

	req, ok := claudecode.DetectInput(frame)
	if !ok {
		t.Fatal("the live 2.1.251 trust dialog did not classify; this pin would be vacuous")
	}
	// Mirrors pkg/chat's toClientInputRequest (unexported): the client-facing
	// DTO is built field-by-field, so Keys/Highlighted never cross this boundary
	// — which is precisely why the unattended answer must be right by ALIAS.
	cr := chat.InputRequest{ID: req.ID, Kind: req.Kind, Prompt: req.Prompt}
	for _, o := range req.Options {
		cr.Options = append(cr.Options, chat.InputOption{ID: o.ID, Alias: o.Alias, Label: o.Label})
	}
	if cr.Options[0].Alias != "deny" {
		t.Fatalf("fixture drifted: Options[0] is %q/%q, but the danger this pins is that claude "+
			"lists the DENY row first", cr.Options[0].Alias, cr.Options[0].Label)
	}

	ans, ok := oneshot.AutoAcceptAnswer(cr)
	if !ok {
		t.Fatal("AutoAcceptAnswer declined the folder-trust dialog; an unattended run would wedge")
	}
	if ans.OptionID != "1" {
		t.Errorf("OptionID = %q, want %q (\"Yes, I trust this folder\") — the unattended path is "+
			"answering the folder-trust dialog by QUITTING claude", ans.OptionID, "1")
	}
}
