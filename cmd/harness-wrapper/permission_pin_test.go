package main

import (
	"context"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// bypassAcceptancePinScreen is claude-code's --dangerously-skip-permissions
// acceptance screen, in the same shape as the `bypassScreen` fixture that
// pkg/turns/harness/claudecode/input_test.go:24 already pins. Duplicated
// because that fixture is an unexported test const in another package.
const bypassAcceptancePinScreen = `WARNING: Claude Code running in Bypass Permissions mode

By proceeding, you accept all risks.

❯ 1. Yes, I accept
  2. No, exit
`

// resolveUnderPolicy mirrors (*chat.Conversation).policyOption plus findOption
// (pkg/chat/input.go:256 and :347) — both unexported — so this pin can run the
// exact *chat.InputPolicy value inputHandling builds against a real detected
// request. Matching is by option ID, Alias, or case-insensitive Label, exactly
// as findOption does.
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

// TestInputHandling_UnattendedAutoAcceptsBypassAcceptance pins the IMPLICIT
// COUPLING on the `run` path: inputHandling's unattended branch installs ONE
// `trust_prompt` policy entry — written for the FOLDER-TRUST dialog — and it
// also auto-accepts claude-code's skip-all-permissions acceptance screen,
// because claudecode.DetectInput emits bypassAnchor ("Bypass Permissions mode")
// under that same Kind (pkg/turns/harness/claudecode/claudecode.go:205) and the
// screen's first option carries Alias "proceed" ("Yes, I accept").
//
// This is the SECOND, independent copy of that policy; pkg/oneshot.turnConfig
// holds the other (pinned in pkg/oneshot/permission_pin_test.go), and
// pkg/harness.TurnConfig.InputPolicy restates it as prose.
//
// EXPECTED TO BREAK DELIBERATELY: the filed follow-up that splits the detector's
// Kind (e.g. a distinct `bypass_acceptance` instead of reusing `trust_prompt`)
// will make this resolve to nil. When it goes red, decide explicitly whether the
// unattended policy should still accept the bypass screen — and add the new kind
// to BOTH policy copies if so.
func TestInputHandling_UnattendedAutoAcceptsBypassAcceptance(t *testing.T) {
	req, ok := claudecode.DetectInput(bypassAcceptancePinScreen)
	if !ok {
		t.Fatal("claudecode.DetectInput did not classify the bypass-permissions screen; this pin would be vacuous")
	}
	if req.Kind != "trust_prompt" {
		t.Fatalf("Kind = %q, want trust_prompt — the coupling this test pins is GONE; "+
			"check whether the unattended policy still covers the bypass screen", req.Kind)
	}

	policy, handler := inputHandling(context.Background(), false, nil)
	if handler == nil {
		t.Fatal("inputHandling returned no unattended callback")
	}
	opt := resolveUnderPolicy(policy, req)
	if opt == nil {
		t.Fatal("the unattended policy no longer answers the bypass-acceptance screen; " +
			"an unattended `run` would surface it instead of proceeding")
	}
	if opt.Alias != "proceed" {
		t.Errorf("resolved option alias = %q, want proceed (got label %q)", opt.Alias, opt.Label)
	}
	if opt.Label != "Yes, I accept" {
		t.Errorf("resolved option label = %q, want %q — the policy picked a different row", opt.Label, "Yes, I accept")
	}
}

// TestInputHandling_InteractiveLeavesBypassToTheHuman is the control arm: the
// interactive branch returns a NIL policy precisely so nothing is pre-resolved
// behind the human's back — including the bypass screen. Pinned alongside the
// unattended arm so a fix that "solves" the coupling by adding a policy entry to
// the interactive branch shows up here too.
func TestInputHandling_InteractiveLeavesBypassToTheHuman(t *testing.T) {
	req, ok := claudecode.DetectInput(bypassAcceptancePinScreen)
	if !ok {
		t.Fatal("claudecode.DetectInput did not classify the bypass-permissions screen; this pin would be vacuous")
	}
	policy, _ := inputHandling(context.Background(), true, nil)
	if policy != nil {
		t.Fatalf("interactive InputPolicy = %+v, want nil (every kind must fall through to the tty callback)", policy)
	}
	if opt := resolveUnderPolicy(policy, req); opt != nil {
		t.Errorf("interactive mode pre-resolved the bypass screen to %q; it must reach the human", opt.Label)
	}
}
