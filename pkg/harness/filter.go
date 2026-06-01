package harness

import "github.com/olesho/harness-wrapper/pkg/transcript"

// admitParent reports whether an event should be admitted to the OnEvent stream,
// enforcing the mode-dependent SOURCE authority for the PARENT conversation. It
// is the single central authority filter every event passes before delivery
// (reviews #3/#5): no acquisition strategy emits to OnEvent directly, so a
// parser cannot bypass it and double-record the parent from two sources.
//
// The decision is purely (effectiveMode, source, kind, isSubagent) — it never
// inspects session identity beyond the parent/subagent distinction:
//
//   - SUBAGENT events (ParentSessionID set) are admitted in any mode: a
//     subagent is a different native session, captured from the file/export
//     side, and never competes with the parent's source.
//   - StreamParse: the LIVE stream is the sole parent source. Live parent events
//     (conversation kinds AND session/usage) are admitted; file parent events
//     are dropped (the file contributes subagents only).
//   - Hooks: the FILE is authoritative for the parent. File parent events are
//     admitted; live parent events are admitted ONLY for non-conversation kinds
//     (session id + usage) — live conversation kinds (message/tool-use/
//     tool-result/output-chunk) are dropped so they don't double-leak alongside
//     the file copy.
//
// effectiveMode must be the LATCHED parent strategy (StreamParse or Hooks),
// never Auto — Auto is resolved before any event reaches the filter. Off admits
// nothing (the orchestrator does not invoke the filter in Off mode, but a
// defensive false keeps the contract total).
func admitParent(effectiveMode Mode, source, kind string, isSubagent bool) bool {
	if isSubagent {
		return true
	}
	switch effectiveMode {
	case TranscriptStreamParse:
		return source == transcript.SourceLive
	case TranscriptHooks:
		if source == transcript.SourceFile {
			return true
		}
		return !isParentConversationKind(kind)
	default: // Off / unknown
		return false
	}
}

// isParentConversationKind reports whether an event Type is part of the parent
// CONVERSATION (the kinds that must come from exactly one source per mode), as
// opposed to the out-of-band session/usage metadata that the live stream may
// always contribute. Unknown types are treated as NON-conversation (advisory):
// they are never dropped by the conversation rule, mirroring the plan's
// "unknown native types → system (advisory)".
func isParentConversationKind(kind string) bool {
	switch kind {
	case transcript.EventText, transcript.EventToolUse, transcript.EventToolResult:
		// (+ output-chunk once that Kind is added — also a conversation kind.)
		return true
	default:
		// session_meta (session), usage, system, and any unknown type.
		return false
	}
}
