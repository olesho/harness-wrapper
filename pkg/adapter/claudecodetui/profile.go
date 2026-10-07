// Package claudecodetui is the Harness Adapter's Claude Code TUI profile: the
// "TUI hybrid" (ADR-012, decision 6). claude runs interactively, without -p,
// on a pseudo-terminal; the profile types inputs as keystrokes and never reads
// the screen. What claude does comes from its hooks (a live channel, package
// live, beside the hook spool), its transcript, and its debug log
// (--debug-file). Importing it registers the Harness Adapter under
// "claude-code-tui" (contract.Lookup), beside "claude-code", whose
// configuration, record and failure classes it shares (pkg/adapter/claudecode).
//
// It is agentd's fallback for when stream-json is unavailable or breaks. This
// is its first phase: Sessions open fresh or reopen, inputs run as turns that
// complete or error, the record is the Claude Code profile's, and Close stops
// claude. Interrupts, retries reported as they happen, prompts, turns of
// claude's own and rate-limit reports come later; the Descriptor declares
// none of them.
package claudecodetui

import (
	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Name is the harness's name: the profile registers under it.
const Name = "claude-code-tui"

// MaxInputBytes bounds an input: it is typed, or pasted, into claude's
// composer.
const MaxInputBytes = 64 << 10

// Profile is the Claude Code TUI profile. It keeps no state.
type Profile struct{}

func init() { adapter.Register(Name, Profile{}) }

// efforts are the efforts claude's --effort takes.
var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Describe describes the profile. The harness version is claude-code's pin:
// the claude this profile reads the hooks and debug log of.
func (Profile) Describe() contract.Descriptor {
	pin, _ := versions.Pinned(claudecode.Name)
	return contract.Descriptor{
		Contract: contract.Version,
		Harness:  contract.HarnessInfo{Name: Name, Version: pin, Adapter: adapter.Name()},
		Capabilities: []contract.Capability{
			contract.CapResume, contract.CapAssignSessionID,
		},
		CheckpointFormat: claudecode.CheckpointFormat,
		CredentialKinds:  []string{claudecode.CredentialKind},
		Spec: contract.SpecSupport{
			Models:             contract.Models{Any: true},
			Efforts:            efforts,
			Instructions:       []string{contract.InstructionPersona, contract.InstructionWorkspace},
			Skills:             true,
			Memory:             true,
			Connectors:         []string{contract.ConnectorStdio, contract.ConnectorHTTP},
			PermissionPostures: []string{contract.PostureBypass},
			InputContent:       []string{contract.ContentText},
		},
		Limits: contract.Limits{MaxInputBytes: MaxInputBytes},
	}
}

// liveHooks are the hooks the transport reads as claude runs, beside the
// Claude Code profile's.
func liveHooks() claudecode.Hooks {
	h := claudecode.Hooks{Harness: live.Harness}
	for _, e := range live.Hooks {
		h.Events = append(h.Events, harnesscore.HookEntry{NativeEvent: e.Native, Arg: e.Arg})
	}
	return h
}

// Provision renders claude's configuration as the Claude Code profile does,
// with the live hooks too. The open_config is the same: the transport adds
// what running claude on a terminal takes.
func (Profile) Provision(req contract.ProvisionRequest) (contract.ProvisionResult, error) {
	return claudecode.ProvisionWith(req, liveHooks())
}
