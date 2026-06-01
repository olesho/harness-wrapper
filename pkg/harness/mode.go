package harness

// Mode selects how the orchestrator acquires the transcript for a run. It is
// the F1 flag of the P3 rollout: default Off (no acquisition, no behavior
// change) until a caller opts in.
type Mode int

const (
	// TranscriptOff disables transcript acquisition entirely. The orchestrator
	// still composes wrapper.Run (and may capture the session id for resume),
	// but emits no events. This is the zero value / safe default.
	TranscriptOff Mode = iota

	// TranscriptStreamParse parses the harness's live headless stdout (the
	// durable line tap → StreamParser) as the sole parent-conversation source.
	// The generic floor: no hook config, no IPC.
	TranscriptStreamParse

	// TranscriptHooks drives acquisition from the harness's hook mechanism (the
	// on-disk transcript file is authoritative for the parent; the live stream
	// contributes only session-id/usage). Requires a resolved HookProvider;
	// implemented in P3c. Until then it degrades to the StreamParse floor.
	TranscriptHooks

	// TranscriptAuto uses Hooks when installable for the run, else StreamParse.
	// The effective parent strategy is decided once and latched (review #2);
	// the authority filter is always invoked with the LATCHED effective mode
	// (StreamParse or Hooks), never Auto.
	TranscriptAuto
)

// String renders the mode for logs/diagnostics.
func (m Mode) String() string {
	switch m {
	case TranscriptOff:
		return "off"
	case TranscriptStreamParse:
		return "stream"
	case TranscriptHooks:
		return "hooks"
	case TranscriptAuto:
		return "auto"
	default:
		return "unknown"
	}
}
