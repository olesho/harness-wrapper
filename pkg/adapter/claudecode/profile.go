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

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Name is the harness's name: the profile registers under it.
const Name = "claude-code"

// CredentialKind is the credential claude takes: a `claude setup-token`
// token, given to claude as CLAUDE_CODE_OAUTH_TOKEN.
const CredentialKind = "claude_oauth_token"

// APIHost is the one host claude reaches with its nonessential traffic off,
// which the profile always turns off: its model API, where it presents its
// token, as a bearer token in Authorization.
const APIHost = "api.anthropic.com"

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

// MaxSessions is how many Sessions of one agent the profile runs side by
// side: the most the kit's concurrency scenarios have passed with the pinned
// claude (TestClaudeConforms), run again whenever the pin moves.
const MaxSessions = 8

// efforts are the efforts claude's --effort takes.
var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Describe describes the profile. The harness version is hw's pin: the claude
// the profile was verified against. It loads the Sessions that claude saved,
// and no other's: a version joins Load's sources once a Session it saved has
// passed the kit's load scenarios at the pin (testdata/load). Behind an
// egress broker claude reaches APIHost alone, with a placeholder token.
func (Profile) Describe() contract.Descriptor {
	pin, _ := versions.Pinned(Name)
	return contract.Descriptor{
		Contract: contract.Version,
		Harness:  contract.HarnessInfo{Name: Name, Version: pin, Adapter: adapter.Name()},
		Capabilities: []contract.Capability{
			contract.CapResume, contract.CapAssignSessionID, contract.CapToolsObserved,
			contract.CapSubagents, contract.CapRateLimits, contract.CapRetryVisible, contract.CapBackgroundTurns,
			contract.CapSessionLoad, contract.CapBrokeredCredentials, contract.CapConcurrentSessions,
		},
		Load: &contract.LoadSupport{Formats: []int{adapter.ArchiveFormat}, Sources: []string{"2.1.283", pin}},
		Egress: &contract.Egress{
			Hosts:       []string{APIHost},
			Credentials: []contract.CredentialRoute{{Kind: CredentialKind, Hosts: []string{APIHost}, Headers: []string{"Authorization"}}},
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
		Limits: contract.Limits{MaxInputBytes: contract.MaxInputBytes, MaxSessions: MaxSessions},
	}
}
