// Package contract is the Harness Adapter Interface: the Go interface a
// Harness Adapter implements and a runtime's Agent Adapter calls to drive a
// coding-agent harness without naming it (ADR-012).
//
// The interface is specified in "Harness Adapter Interface v1"
// (https://coplan.olehluchkiv.com/d/engine-contract-v1-specification); this
// package is its normative form. Every type has a JSON form with the field
// names the specification uses, generated as JSON Schema (schema.json), for
// where a type crosses a process boundary or is journaled.
//
// Minor 1 adds, each behind a capability: a saved Session loaded into a fresh
// environment (session_load: ProvisionRequest.Load, history_relocations and
// OpenRequest.Loaded), and the turns a harness starts with no input
// (autonomous_turns: turn_started and turn_ended naming a turn and no input,
// and an interrupt that names a turn). Minor 2 adds credentials kept from the
// harness by an egress broker (brokered_credentials: Descriptor.Egress and
// Placeholder), and a subscription login the runtime keeps itself and lends
// behind that broker (login_keeper: Descriptor.Keeper and Keep). Minor 3
// lets a connector's secret header come from a file
// (HTTPConnector.HeadersFile), and adds the turns a harness starts to take up
// work it ran in the background (background_turns). Minor 4 reports that
// work itself as it changes (the background_tasks observation and
// State.Background). Minor 5
// gives Placeholder the agent's model (PlaceholderRequest.Model), so a
// credential kind that serves several providers keeps each credential on its
// own provider's hosts. Minor 6 lets several Sessions of one agent be open at
// once (concurrent_sessions: Limits.MaxSessions). Minor 7 lets an agent
// choose how strictly its harness is held to the adapter's pinned version
// (AgentSpec.VersionPolicy: strict, or flexible by default), and reports the
// version a Session runs (OpenResult.HarnessVersion); every 1.7 adapter
// honours both, behind no capability. Minor 8 lets a load change one value in
// a saved file's first line (ProvisionResult.HistoryRewrites), where a harness
// finds a Session by the working directory its record names.
//
// Two callers use an Adapter:
//
//   - the Supervisor, before any agent process exists, calls Describe and
//     Provision — the pure rendering of a harness-neutral Agent Spec into the
//     harness's files, argv and environment, which the Supervisor writes —
//     and, for a credential it keeps from the harness, Placeholder;
//   - the Host, inside the agent's isolation — one per agent, or with
//     concurrent_sessions one per Session — opens Sessions (NewSession, then
//     Open … Close) and record handles (OpenRecord).
//
// An Adapter registers itself under its harness's name (Register), in its
// package's init; a runtime links the harnesses it offers through one file of
// blank imports, its harness list, and finds them with Lookup.
//
// This package imports only the standard library, so importing it links
// nothing else of harness-wrapper.
package contract

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the contract version this package defines.
const Version = "harness-adapter/1.8"

// versionPrefix is every version's prefix; the major follows it.
const versionPrefix = "harness-adapter/"

// Major and Minor are Version's parts.
const (
	Major = 1
	Minor = 8
)

// ParseVersion splits a contract version, harness-adapter/<major>.<minor>,
// into its parts.
func ParseVersion(v string) (major, minor int, err error) {
	rest, ok := strings.CutPrefix(v, versionPrefix)
	if !ok {
		return 0, 0, fmt.Errorf("contract version %q: want %s<major>.<minor>", v, versionPrefix)
	}
	ma, mi, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, 0, fmt.Errorf("contract version %q: want %s<major>.<minor>", v, versionPrefix)
	}
	if major, err = strconv.Atoi(ma); err != nil || major < 1 || ma != strconv.Itoa(major) {
		return 0, 0, fmt.Errorf("contract version %q: bad major", v)
	}
	if minor, err = strconv.Atoi(mi); err != nil || minor < 0 || mi != strconv.Itoa(minor) {
		return 0, 0, fmt.Errorf("contract version %q: bad minor", v)
	}
	return major, minor, nil
}

// MinorOf is the minor of contract version v: the newest requests a caller
// may send an adapter declaring v (Compatible; CheckMinor refuses a request
// newer than the minor it is written in), and the newest capabilities that
// adapter may declare (Capability.Since).
func MinorOf(v string) (int, error) {
	_, minor, err := ParseVersion(v)
	return minor, err
}

// Compatible reports whether a caller of this package may use an adapter
// declaring contract version v: the majors match. The caller then sends it
// requests no newer than v's minor.
func Compatible(v string) bool {
	major, _, err := ParseVersion(v)
	return err == nil && major == Major
}
