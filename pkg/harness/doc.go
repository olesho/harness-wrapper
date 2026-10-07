// Package harness is the per-harness orchestration + capability layer for
// harness-wrapper. It sits ABOVE the thin pkg/wrapper supervisor: a caller
// resolves a per-harness Profile to a run-specific ResolvedProfile (after any
// runtime capability detection) and dispatches on the capabilities that are
// actually available for that run.
//
// This package is DISTINCT from pkg/turns/harness/* (the per-harness TUI
// turn adapters); this one owns resume + transcript-acquisition capabilities.
//
// Capability model (see ResolvedProfile): rather than static type-asserts on a
// fat interface — which would wrongly treat a probe-gated harness (Codex) as
// available before detection — Profile.Resolve runs detection ONCE and returns
// a ResolvedProfile whose capability fields are non-nil only when CONFIRMED for
// this run. Callers check `rp.<Cap> != nil`, never the static profile.
//
// The ResolvedProfile starts (P1) with only the SessionID + Resume capabilities
// populated for Claude; later phases ADD fields (Stream/Hooks/Reader/Export) —
// an additive change, not a breaking re-migration of the Profile shape.
//
// Everything but Run and RunTurn lives in internal/harnesscore, and every
// exported identifier here that is not theirs is the identical one there. Run
// and RunTurn drive a harness through pkg/chat, which links every built-in
// harness; a per-harness hook profile imports the core, and links only its
// own harness.
package harness

//go:generate go run ../../internal/facadegen -core ../../internal/harnesscore -import github.com/olesho/harness-wrapper/internal/harnesscore -alias harnesscore -pkg harness -skip RenderSettingsJSONHooks,WriteSpoolFile -o forward.go
