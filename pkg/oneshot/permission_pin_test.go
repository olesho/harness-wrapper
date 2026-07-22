package oneshot

// Behavior pins for the launch-time permission knob (canonical rungs
// plan/manual/ask/auto/bypass -> claude --permission-mode, codex -s/-a).
//
// These tests document how much of that knob today's RUNTIME input machinery
// actually enforces on the unattended paths. They add no behavior; each one is
// written so a named follow-up fix breaks it DELIBERATELY rather than changing
// semantics silently.

import (
	"os"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
)

// bypassAcceptanceScreen is claude-code's --dangerously-skip-permissions
// acceptance screen, byte-identical in shape to the `bypassScreen` fixture that
// pkg/turns/harness/claudecode/input_test.go:24 already pins
// (TestDetectInput_BypassAcceptance). Duplicated rather than exported because
// the fixture is an unexported test const in another package.
const bypassAcceptanceScreen = `WARNING: Claude Code running in Bypass Permissions mode

By proceeding, you accept all risks.

❯ 1. Yes, I accept
  2. No, exit
`

// resolveUnderPolicy mirrors (*chat.Conversation).policyOption plus findOption
// (pkg/chat/input.go:256 and :347) — both unexported — so a pin can run the
// exact *chat.InputPolicy value the production code builds against a real
// detected request. Matching is by option ID, Alias, or case-insensitive Label,
// exactly as findOption does.
func resolveUnderPolicy(p *chat.InputPolicy, req *turns.InputRequest) *turns.InputOption {
	if p == nil {
		return nil
	}
	d, ok := p.ByKind[req.Kind]
	if !ok || d.Kind == "" {
		if p.Default == "" {
			return nil
		}
		d = chat.Disposition{Kind: p.Default}
	}
	switch d.Kind {
	case chat.DispositionAnswer:
		if d.OptionID == "" {
			return nil
		}
		ls := strings.ToLower(d.OptionID)
		for i := range req.Options {
			o := &req.Options[i]
			if o.ID == d.OptionID || strings.ToLower(o.Alias) == ls || strings.EqualFold(o.Label, d.OptionID) {
				return o
			}
		}
	case chat.DispositionDeny:
		for i := range req.Options {
			if req.Options[i].Alias == "deny" {
				return &req.Options[i]
			}
		}
	}
	return nil
}

// toClientRequest mirrors pkg/chat's unexported toClientInputRequest: the
// client-facing DTO is rebuilt field-by-field, so Keys/Highlighted never cross
// the boundary into an OnInputRequest callback.
func toClientRequest(req *turns.InputRequest) chat.InputRequest {
	cr := chat.InputRequest{ID: req.ID, Kind: req.Kind, Prompt: req.Prompt, Header: req.Header, MultiSelect: req.MultiSelect}
	for _, o := range req.Options {
		cr.Options = append(cr.Options, chat.InputOption{ID: o.ID, Alias: o.Alias, Label: o.Label, Description: o.Description})
	}
	return cr
}

func pinConfig() Config {
	return Config{
		Harness:    "claude",
		BinaryPath: "/usr/local/bin/claude",
		WorkingDir: "/work",
		Prompt:     "hello",
	}
}

// TestTurnConfig_BypassAcceptanceAutoAnswered pins the IMPLICIT COUPLING on the
// container path: turnConfig's single unattended `trust_prompt` policy entry —
// written for the FOLDER-TRUST dialog — also auto-accepts claude-code's
// skip-all-permissions acceptance screen, because claudecode.DetectInput emits
// bypassAnchor ("Bypass Permissions mode") under that same Kind
// (pkg/turns/harness/claudecode/claudecode.go:205) and the screen's first
// option carries Alias "proceed" ("Yes, I accept").
//
// turnConfig is the copy used by `structured-run` and therefore by
// pkg/env.RunStructuredTurn — the containerized guest path, where a reachable
// `bypass` rung matters most. cmd/harness-wrapper's inputHandling holds the
// second, independent copy of the same policy (pinned there).
//
// EXPECTED TO BREAK DELIBERATELY: the filed follow-up that splits the detector's
// Kind (e.g. a distinct `bypass_acceptance` instead of reusing `trust_prompt`)
// will make this resolve to nil. That is the point — when it goes red, decide
// explicitly whether the unattended policy should still accept the bypass
// screen, and add the new kind to the policy if so.
func TestTurnConfig_BypassAcceptanceAutoAnswered(t *testing.T) {
	req, ok := claudecode.DetectInput(bypassAcceptanceScreen)
	if !ok {
		t.Fatal("claudecode.DetectInput did not classify the bypass-permissions screen; this pin would be vacuous")
	}
	if req.Kind != "trust_prompt" {
		t.Fatalf("Kind = %q, want trust_prompt — the coupling this test pins is GONE; "+
			"check whether the unattended policy still covers the bypass screen", req.Kind)
	}

	cfg := turnConfig(pinConfig())
	opt := resolveUnderPolicy(cfg.InputPolicy, req)
	if opt == nil {
		t.Fatal("turnConfig's unattended policy no longer answers the bypass-acceptance " +
			"screen; an unattended structured-run would surface it instead of proceeding")
	}
	if opt.Alias != "proceed" {
		t.Errorf("resolved option alias = %q, want proceed (got label %q)", opt.Alias, opt.Label)
	}
	if opt.Label != "Yes, I accept" {
		t.Errorf("resolved option label = %q, want %q — the policy picked a different row", opt.Label, "Yes, I accept")
	}
}

// TestTurnConfig_CodexApprovalAutoAnswered pins the RUNTIME-ENFORCEMENT gap on
// codex's approval axis: turnConfig wires OnInputRequest = AutoAcceptAnswer
// UNCONDITIONALLY (oneshot.go:172), and AutoAcceptAnswer is a catch-all
// (affirmative option, else Options[0], for ANY kind). codex's KindApproval is
// deliberately excluded from auto-DISMISS, yet it is still auto-ANSWERED here,
// because detectApproval only returns a request when both a "proceed" and a
// "deny" alias are present — and AffirmativeOption matches "proceed".
//
// Consequence being pinned: `-a on-request` / `-a untrusted` restrict NOTHING
// under unattended `run` / `structured-run`. (The `-s` sandbox axis is enforced
// by codex itself, independent of this loop, and is unaffected.)
//
// This asserts through the WIRING (turnConfig().OnInputRequest), not through
// AutoAcceptAnswer directly, so making the callback conditional on an approval
// rung breaks it. EXPECTED TO BREAK DELIBERATELY: the follow-up that scopes
// OnInputRequest by permission mode (or teaches AutoAcceptAnswer to leave
// KindApproval unanswered) will turn this red.
func TestTurnConfig_CodexApprovalAutoAnswered(t *testing.T) {
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
	if req.Kind != codex.KindApproval {
		t.Fatalf("Kind = %q, want KindApproval", req.Kind)
	}

	handler := turnConfig(pinConfig()).OnInputRequest
	if handler == nil {
		t.Fatal("turnConfig no longer wires OnInputRequest; the unattended approval path changed")
	}
	ans, ok := handler(toClientRequest(req))
	if !ok {
		t.Fatal("the unattended handler declined a codex approval; the run would wedge on chat.ErrInputPending")
	}
	opt := findByID(req, ans.OptionID)
	if opt == nil {
		t.Fatalf("answer OptionID = %q matches no option in the request", ans.OptionID)
	}
	if opt.Alias != "proceed" {
		t.Errorf("unattended answer resolved to alias %q (label %q), want proceed — "+
			"codex approvals are no longer auto-approved; if that is the intended fix, "+
			"update this pin and the doc comments it references", opt.Alias, opt.Label)
	}
}

func findByID(req *turns.InputRequest, id string) *turns.InputOption {
	for i := range req.Options {
		if req.Options[i].ID == id {
			return &req.Options[i]
		}
	}
	return nil
}
