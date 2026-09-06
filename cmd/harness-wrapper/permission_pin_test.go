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

// TestInputHandling_UnattendedAutoAcceptsBypassAcceptance pins that
// inputHandling's unattended branch auto-accepts claude-code's
// skip-all-permissions acceptance screen — now via an EXPLICIT
// claudecode.KindBypassAcceptance entry rather than as a side effect of the
// folder-trust one. The screen's first option carries Alias "proceed" ("Yes, I
// accept"), which is what the entry resolves to.
//
// This is the SECOND, independent copy of that policy; pkg/oneshot.turnConfig
// holds the other (pinned in pkg/oneshot/permission_pin_test.go), and
// pkg/harness.TurnConfig.InputPolicy restates it as prose.
//
// BROKEN DELIBERATELY, AND FIXED, BY PUPPET-507 (child of PUPPET-495): this
// test used to assert Kind == "trust_prompt" for the bypass screen and existed
// to go red when the detector's Kind was split. It was split — the folder-trust
// dialog and the acceptance screen are separate kinds now, because every policy
// surface keys on Kind alone and "trust the folder but never silently accept a
// skip-all-permissions launch" was otherwise inexpressible. The decision the old
// comment demanded was made explicitly: harness-wrapper's own unattended
// behaviour stays byte-identical, so BOTH policy copies name the new kind, and
// this pin now asserts the new kind plus the same resolved option. The
// separation itself is guarded by
// TestPolicy_CanTrustFolderWithoutAcceptingBypass below.
func TestInputHandling_UnattendedAutoAcceptsBypassAcceptance(t *testing.T) {
	req, ok := claudecode.DetectInput(bypassAcceptancePinScreen)
	if !ok {
		t.Fatal("claudecode.DetectInput did not classify the bypass-permissions screen; this pin would be vacuous")
	}
	if req.Kind != claudecode.KindBypassAcceptance {
		t.Fatalf("Kind = %q, want %q — the bypass screen no longer carries its own kind; "+
			"check whether the unattended policy still covers it",
			req.Kind, claudecode.KindBypassAcceptance)
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

// folderTrustPinScreen is claude-code's folder-trust dialog, the counterpart to
// bypassAcceptancePinScreen above. Duplicated for the same reason: the fixture
// in pkg/turns/harness/claudecode/input_test.go is an unexported test const.
const folderTrustPinScreen = `╭─────────────────────────────────────────────────╮
│ Do you trust the files in this folder?            │
│                                                   │
│ /Users/oleh/Work/aether/harness-wrapper           │
│                                                   │
│ ❯ 1. Yes, proceed                                 │
│   2. No, exit                                     │
│                                                   │
│ Enter to confirm · Esc to exit                    │
╰─────────────────────────────────────────────────╯`

// TestPolicy_CanTrustFolderWithoutAcceptingBypass is the regression guard for
// the split PUPPET-495 asked for: a single ByKind policy must be able to say
// "yes" to folder trust and "no" to the skip-all-permissions acceptance screen
// AT THE SAME TIME. While both screens carried Kind "trust_prompt" that
// sentence was inexpressible — one map key covered both, so any policy that
// trusted a folder also accepted a bypass launch, silently.
//
// This asserts through the real resolution path (resolveUnderPolicy mirrors
// chat's policyOption/findOption), against both real detected requests, so it
// fails the moment the two kinds are merged back together.
func TestPolicy_CanTrustFolderWithoutAcceptingBypass(t *testing.T) {
	trust, ok := claudecode.DetectInput(folderTrustPinScreen)
	if !ok {
		t.Fatal("claudecode.DetectInput did not classify the folder-trust dialog; this guard would be vacuous")
	}
	bypass, ok := claudecode.DetectInput(bypassAcceptancePinScreen)
	if !ok {
		t.Fatal("claudecode.DetectInput did not classify the bypass-permissions screen; this guard would be vacuous")
	}

	policy := &chat.InputPolicy{ByKind: map[string]chat.Disposition{
		claudecode.KindTrustPrompt:      {Kind: chat.DispositionAnswer, OptionID: "proceed"},
		claudecode.KindBypassAcceptance: {Kind: chat.DispositionDeny},
	}}

	trustOpt := resolveUnderPolicy(policy, trust)
	if trustOpt == nil {
		t.Fatal("the folder-trust dialog resolved to nothing under a policy that answers trust_prompt")
	}
	if trustOpt.Label != "Yes, proceed" {
		t.Errorf("folder trust resolved to %q (alias %q), want %q", trustOpt.Label, trustOpt.Alias, "Yes, proceed")
	}

	bypassOpt := resolveUnderPolicy(policy, bypass)
	if bypassOpt == nil {
		t.Fatal("the bypass-acceptance screen resolved to nothing under a policy that denies bypass_acceptance")
	}
	if bypassOpt.Label != "No, exit" {
		t.Errorf("bypass acceptance resolved to %q (alias %q), want %q — the deny entry did not bind, "+
			"which means the trust entry is still covering both screens",
			bypassOpt.Label, bypassOpt.Alias, "No, exit")
	}
}
