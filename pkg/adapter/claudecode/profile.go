// Package claudecode is the Harness Adapter's Claude Code profile: claude
// driven over its stream-json protocol, one process per Session, with the
// session transcript and the hook spool as its record. Importing it registers
// the Harness Adapter under "claude-code" (contract.Lookup), and links no
// other harness.
//
// The harness distribution under harness_root holds the pinned claude binary
// (BinaryPath) and this profile's hook helper, cmd/claude-code-hook
// (HookPath), which writes what claude's hooks report to the spool in
// layout.scratch.
package claudecode

import (
	"path/filepath"
	"runtime/debug"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Name is the harness's name: the profile registers under it.
const Name = "claude-code"

// CredentialKind is the credential claude takes: a `claude setup-token`
// token, given to claude as CLAUDE_CODE_OAUTH_TOKEN.
const CredentialKind = "claude_oauth_token"

// CheckpointFormat is the checkpoint format the profile writes: the transcript
// follower's checkpoint (transcript.Checkpoint) as JSON — the form agentd
// stored before this profile existed, which it therefore reads.
const CheckpointFormat = 1

// BinaryPath is claude's path in the harness distribution rooted at root.
func BinaryPath(root string) string { return filepath.Join(root, "bin", "claude") }

// HookPath is the hook helper's path in the harness distribution rooted at
// root.
func HookPath(root string) string { return filepath.Join(root, "bin", "claude-code-hook") }

// Profile is the Claude Code profile. It keeps no state.
type Profile struct{}

func init() { adapter.Register(Name, Profile{}) }

// efforts are the efforts claude's --effort takes.
var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Describe describes the profile. The harness version is hw's pin: the claude
// the profile was verified against.
func (Profile) Describe() contract.Descriptor {
	pin, _ := versions.Pinned(Name)
	return contract.Descriptor{
		Contract: contract.Version,
		Harness:  contract.HarnessInfo{Name: Name, Version: pin, Adapter: "harness-wrapper " + moduleVersion()},
		Capabilities: []contract.Capability{
			contract.CapResume, contract.CapAssignSessionID, contract.CapToolsObserved,
			contract.CapSubagents, contract.CapRateLimits, contract.CapRetryVisible,
		},
		CheckpointFormat: CheckpointFormat,
		CredentialKinds:  []string{CredentialKind},
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
		Limits: contract.Limits{MaxInputBytes: contract.MaxInputBytes},
	}
}

// moduleVersion is harness-wrapper's version in this binary: its module's
// version as a dependency, "(devel)" in its own builds.
func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	const mod = "github.com/olesho/harness-wrapper"
	if bi.Main.Path == mod {
		return bi.Main.Version
	}
	for _, d := range bi.Deps {
		if d.Path == mod {
			if d.Replace != nil && d.Replace.Version != "" {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return "(unknown)"
}
