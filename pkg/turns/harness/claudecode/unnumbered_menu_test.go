package claudecode

import (
	"bytes"
	"testing"
)

// The folder-trust dialog as Claude Code 2.1.261 renders it: no "N." prefixes,
// and the ❯ default sitting on "No, exit".
const trust2_1_261 = `Accessing workspace:

/Users/x/.loom/workspaces/ws/worktrees/repo/agent

Quick safety check: Is this a project you created or one you trust? (Like your own code, a
well-known open source project, or work from your team). If not, take a moment to review.

Claude Code'll be able to read, edit, and execute files here.

Security guide

❯ No, exit
  Yes, I trust this folder

Enter to confirm · Esc to cancel
`

// The pre-2.1.261 numbered layout must keep working.
const trustNumbered = `Do you trust the files in this folder?

❯ 1. Yes, I trust this folder
  2. No, exit
`

func TestDetectInput_Unnumbered_2_1_261(t *testing.T) {
	req, ok := DetectInput(trust2_1_261)
	if !ok {
		t.Fatal("dialog not detected — a trust_prompt policy would never be consulted and claude exits on the default 'No, exit'")
	}
	if req.Kind != "trust_prompt" {
		t.Fatalf("Kind = %q, want trust_prompt", req.Kind)
	}
	if len(req.Options) != 2 {
		t.Fatalf("got %d options, want 2: %+v", len(req.Options), req.Options)
	}
	var proceed *struct{ Keys []byte }
	for _, o := range req.Options {
		if o.Alias == "proceed" {
			proceed = &struct{ Keys []byte }{o.Keys}
			if o.Label != "Yes, I trust this folder" {
				t.Errorf("proceed label = %q", o.Label)
			}
		}
	}
	if proceed == nil {
		t.Fatalf("no option aliased 'proceed': %+v", req.Options)
	}
	// Marker is on "No, exit" (row 0); accepting means Down then Enter.
	want := append([]byte("\x1b[B"), '\r')
	if !bytes.Equal(proceed.Keys, want) {
		t.Fatalf("proceed keys = %q, want %q (must navigate, not type a digit)", proceed.Keys, want)
	}
}

func TestDetectInput_NumberedStillWorks(t *testing.T) {
	req, ok := DetectInput(trustNumbered)
	if !ok {
		t.Fatal("numbered dialog regressed")
	}
	if len(req.Options) != 2 {
		t.Fatalf("got %d options, want 2", len(req.Options))
	}
	for _, o := range req.Options {
		if !bytes.HasSuffix(o.Keys, []byte("\r")) || len(o.Keys) != 2 {
			t.Fatalf("numbered option %q keys = %q, want digit+Enter", o.Label, o.Keys)
		}
	}
}

// Prose with a stray "> " must not become a menu.
func TestDetectInput_NoFalseMenu(t *testing.T) {
	if _, ok := DetectInput("Is this a project you created or one you trust?\n\n> just one line\n"); ok {
		t.Fatal("a lone highlighted line was treated as a menu")
	}
}
