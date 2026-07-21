package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/chat"
)

func numberedReq() chat.InputRequest {
	return chat.InputRequest{
		ID:     "req-1",
		Kind:   "trust_prompt",
		Prompt: "Do you trust this folder?",
		Options: []chat.InputOption{
			{ID: "opt-yes", Alias: "trust", Label: "Yes, proceed"},
			{ID: "opt-no", Alias: "deny", Label: "No, exit"},
		},
	}
}

// TestSelectAnswer_NumberedValidSelection: a valid 1-based index returns the
// matching option's ID.
func TestSelectAnswer_NumberedValidSelection(t *testing.T) {
	var out bytes.Buffer
	ans, ok := selectAnswer(numberedReq(), strings.NewReader("2\n"), &out)
	if !ok {
		t.Fatalf("selectAnswer ok = false, want true")
	}
	if ans.OptionID != "opt-no" {
		t.Fatalf("OptionID = %q, want %q", ans.OptionID, "opt-no")
	}
}

// TestSelectAnswer_ReprompsThenSucceeds: an invalid choice re-prompts, then a
// valid one succeeds.
func TestSelectAnswer_ReprompsThenSucceeds(t *testing.T) {
	var out bytes.Buffer
	ans, ok := selectAnswer(numberedReq(), strings.NewReader("9\nx\n1\n"), &out)
	if !ok {
		t.Fatalf("selectAnswer ok = false, want true")
	}
	if ans.OptionID != "opt-yes" {
		t.Fatalf("OptionID = %q, want %q", ans.OptionID, "opt-yes")
	}
	if !strings.Contains(out.String(), "Invalid choice") {
		t.Fatalf("expected an invalid-choice notice, got:\n%s", out.String())
	}
}

// TestSelectAnswer_ExhaustedAttempts: invalid selections past the bound return
// (_, false) rather than spinning.
func TestSelectAnswer_ExhaustedAttempts(t *testing.T) {
	var out bytes.Buffer
	// A reader of endless newlines (empty choices) — never a valid index. bufio
	// keeps returning empty lines; the bound must stop it.
	garbage := strings.NewReader(strings.Repeat("0\n", 100))
	_, ok := selectAnswer(numberedReq(), garbage, &out)
	if ok {
		t.Fatalf("selectAnswer ok = true, want false after exhausting attempts")
	}
	// Bounded: it must not have printed more than selectInputMaxAttempts menus.
	if n := strings.Count(out.String(), "Select ["); n > selectInputMaxAttempts {
		t.Fatalf("printed %d menus, want <= %d (unbounded loop)", n, selectInputMaxAttempts)
	}
}

// TestSelectAnswer_EOFReturnsFalse: a closed/empty reader returns (_, false).
func TestSelectAnswer_EOFReturnsFalse(t *testing.T) {
	var out bytes.Buffer
	_, ok := selectAnswer(numberedReq(), strings.NewReader(""), &out)
	if ok {
		t.Fatalf("selectAnswer ok = true on EOF, want false")
	}
}

// TestSelectAnswer_FreeText: a zero-option prompt returns the typed line as Text.
func TestSelectAnswer_FreeText(t *testing.T) {
	var out bytes.Buffer
	req := chat.InputRequest{ID: "req-2", Kind: "text_input", Prompt: "Name?"}
	ans, ok := selectAnswer(req, strings.NewReader("Ada Lovelace\n"), &out)
	if !ok {
		t.Fatalf("selectAnswer ok = false, want true")
	}
	if ans.Text != "Ada Lovelace" {
		t.Fatalf("Text = %q, want %q", ans.Text, "Ada Lovelace")
	}
	if ans.OptionID != "" {
		t.Fatalf("OptionID = %q, want empty for free-text", ans.OptionID)
	}
	if !strings.Contains(out.String(), "Enter response:") {
		t.Fatalf("expected free-text prompt, got:\n%s", out.String())
	}
}

// TestSelectAnswer_FreeTextEOF: a closed reader with no input returns false.
func TestSelectAnswer_FreeTextEOF(t *testing.T) {
	var out bytes.Buffer
	req := chat.InputRequest{ID: "req-2", Kind: "text_input", Prompt: "Name?"}
	_, ok := selectAnswer(req, strings.NewReader(""), &out)
	if ok {
		t.Fatalf("selectAnswer ok = true on free-text EOF, want false")
	}
}

// TestSelectAnswer_RendersOptions asserts the prompt + numbered list formatting,
// including Alias display.
func TestSelectAnswer_RendersOptions(t *testing.T) {
	var out bytes.Buffer
	selectAnswer(numberedReq(), strings.NewReader("1\n"), &out)
	s := out.String()
	for _, want := range []string{
		"Do you trust this folder?",
		"1) Yes, proceed (trust)",
		"2) No, exit (deny)",
		"Select [1-2]:",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("rendered menu missing %q; got:\n%s", want, s)
		}
	}
}

// TestSelectAnswer_RendersOptionWithoutAlias: no parenthetical when Alias empty.
func TestSelectAnswer_RendersOptionWithoutAlias(t *testing.T) {
	var out bytes.Buffer
	req := chat.InputRequest{
		Prompt:  "Pick",
		Options: []chat.InputOption{{ID: "a", Label: "Alpha"}},
	}
	selectAnswer(req, strings.NewReader("1\n"), &out)
	if strings.Contains(out.String(), "(") {
		t.Fatalf("expected no alias parenthetical, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "1) Alpha") {
		t.Fatalf("expected '1) Alpha', got:\n%s", out.String())
	}
}

// TestInputHandling_UnattendedKeepsTrustPolicy: auto-accept mode preserves the
// trust_prompt policy entry (today's unattended behavior) and answers via
// autoAcceptAnswer.
func TestInputHandling_UnattendedKeepsTrustPolicy(t *testing.T) {
	policy, cb := inputHandling(context.Background(), false, nil)
	if policy == nil {
		t.Fatalf("unattended InputPolicy = nil, want trust_prompt entry")
	}
	d, ok := policy.ByKind["trust_prompt"]
	if !ok || d.Kind != chat.DispositionAnswer || d.OptionID != "proceed" {
		t.Fatalf("trust_prompt disposition = %+v (ok=%v), want answer/proceed", d, ok)
	}
	// The callback is the shared three-tier logic.
	ans, ok := cb(numberedReq())
	if !ok || ans.OptionID != "opt-yes" {
		t.Fatalf("callback = (%+v, %v), want opt-yes/true", ans, ok)
	}
}

// TestInputHandling_InteractiveDropsPolicy: interactive mode must NOT set a
// trust_prompt policy (nil InputPolicy), so trust_prompt reaches OnInputRequest.
func TestInputHandling_InteractiveDropsPolicy(t *testing.T) {
	policy, cb := inputHandling(context.Background(), true, nil)
	if policy != nil {
		t.Fatalf("interactive InputPolicy = %+v, want nil so trust_prompt reaches the callback", policy)
	}
	if cb == nil {
		t.Fatalf("interactive OnInputRequest = nil, want a callback")
	}
}

// TestInputHandling_InteractiveFallbackThreeTier: when the interactive read
// yields nothing (nil tty ⇒ interactiveSelect's read errors immediately), the
// callback must fall back to autoAcceptAnswer — a has-options-but-no-affirmative
// prompt resolves to the first option (true), NOT false (which would fail the
// run with ErrInputPending).
func TestInputHandling_InteractiveFallbackThreeTier(t *testing.T) {
	// A cancelled ctx guarantees interactiveSelect returns false immediately
	// (no real tty read), exercising the fallback deterministically.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cb := inputHandling(ctx, true, nil)

	noAffirmative := chat.InputRequest{Options: []chat.InputOption{
		{ID: "one", Label: "Option one"},
		{ID: "two", Label: "Option two"},
	}}
	ans, ok := cb(noAffirmative)
	if !ok {
		t.Fatalf("interactive fallback ok = false, want true (first option) to avoid ErrInputPending")
	}
	if ans.OptionID != "one" {
		t.Fatalf("fallback OptionID = %q, want first option %q", ans.OptionID, "one")
	}
}

// TestResolveInputMode_AutoAcceptWins: --auto-accept always yields auto-accept
// mode (no tty), regardless of whether a terminal is attached.
func TestResolveInputMode_AutoAcceptWins(t *testing.T) {
	tty, interactive := resolveInputMode(true)
	if interactive {
		t.Fatalf("interactive = true with --auto-accept, want false")
	}
	if tty != nil {
		_ = tty.Close()
		t.Fatalf("tty non-nil with --auto-accept, want nil")
	}
}

// TestResolveInputMode_NoTTY: in the test process os.Stdin/stdout are pipes and
// /dev/tty is typically unavailable, so this must fall back to auto-accept
// without opening a tty. (If a controlling tty happens to exist, we just close
// the handle — the assertion is only that no panic/leak occurs.)
func TestResolveInputMode_NonInteractiveFallback(t *testing.T) {
	tty, interactive := resolveInputMode(false)
	if tty != nil {
		defer func() { _ = tty.Close() }()
	}
	// interactive is environment-dependent; the invariant is the tty/flag
	// agreement: interactive iff a tty handle was returned.
	if interactive != (tty != nil) {
		t.Fatalf("interactive=%v but tty!=nil=%v — must agree", interactive, tty != nil)
	}
}
