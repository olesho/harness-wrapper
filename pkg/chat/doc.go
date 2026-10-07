// Package chat is the Go-level chat-conversation API on top of
// harness-wrapper. A Conversation owns one PTY-supervised harness
// process (Codex, Claude Code, …) and exposes a small interface:
// acquire exclusive control, send a user message, observe turn-level
// state transitions.
//
// The package is the substrate that transport layers (HTTP, gRPC, …)
// import. Transport concerns — framing, streaming protocol, auth — are
// not part of this package and live in separate cmd/ binaries.
//
// Lifecycle:
//
//	conv, err := chat.Open(ctx, chat.Options{
//	    Harness:    "codex",
//	    BinaryPath: "/usr/local/bin/codex",
//	})
//	defer conv.Close(context.Background())
//
//	release, err := conv.AcquireControl(ctx)
//	defer release()
//
//	turnID, err := conv.Send(ctx, "hello")
//	for ev := range conv.Events() {
//	    if ev.Type == EventTurn && ev.Turn.ID == turnID && ev.Turn.State == TurnStateComplete {
//	        break
//	    }
//	}
//
// Concurrency: all Conversation methods are safe for concurrent use.
// Send specifically requires that the caller's goroutine has previously
// acquired control via AcquireControl; otherwise it returns
// ErrNoControl.
//
// The implementation lives in internal/chatcore, which names no harness; every
// exported identifier here is the identical one there. This package registers
// the built-in harness adapters — codex, claude-code, opencode and generic —
// so Options.Harness resolves exactly the names it always has.
package chat

//go:generate go run ../../internal/facadegen -core ../../internal/chatcore -import github.com/olesho/harness-wrapper/internal/chatcore -alias chatcore -pkg chat -skip RegisterAdapter -o forward.go

import (
	"github.com/olesho/harness-wrapper/internal/chatcore"
	"github.com/olesho/harness-wrapper/pkg/harnessname"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/opencode"
	// The built-in harnesses' classifier patterns: a TUI conversation's
	// supervisor classifies its harness's output with them.
	_ "github.com/olesho/harness-wrapper/pkg/wrapper"
)

// The built-in harness adapters, under the names Options.Harness has always
// accepted: the canonical ids, matched exactly — chat does not fold the
// "claude" alias (harness.RunTurn does, via harnessname.Unalias, before it gets
// here). The empty name is the generic adapter.
func init() {
	chatcore.RegisterAdapter(harnessname.Codex, func() turns.Adapter { return codex.New() })
	chatcore.RegisterAdapter(harnessname.ClaudeCode, func() turns.Adapter { return claudecode.New() })
	chatcore.RegisterAdapter(harnessname.OpenCode, func() turns.Adapter { return opencode.New() })
	chatcore.RegisterAdapter("generic", func() turns.Adapter { return generic.New() })
	chatcore.RegisterAdapter("", func() turns.Adapter { return generic.New() })
}
