// Package codex is the Harness Adapter's Codex profile: codex driven over its
// app-server protocol (JSON-RPC 2.0 on stdio, the protocol codex's own IDE
// extension speaks), one process per Session, with the thread's rollout as
// its record. Importing it registers the Harness Adapter under "codex", and
// links no other harness.
//
// The harness distribution under harness_root holds the pinned codex binary
// (BinaryPath): the native one from its npm package's vendor directory, never
// the node shim, whose death would leave the native process holding the
// thread.
package codex

import (
	"path/filepath"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Name is the harness's name: the profile registers under it.
const Name = "codex"

// Credential kinds codex takes: an OpenAI API key, handed to codex over the
// protocol, which codex keeps in memory only; or a ChatGPT workspace's Codex
// access token, which codex reads from CODEX_ACCESS_TOKEN.
const (
	CredentialAPIKey      = "openai_api_key"
	CredentialAccessToken = "codex_access_token"
)

// CheckpointFormat is the checkpoint format the profile writes: the rollout
// follower's checkpoint (transcript.Checkpoint) as JSON.
const CheckpointFormat = 1

// BinaryPath is codex's path in the harness distribution rooted at root.
func BinaryPath(root string) string { return filepath.Join(root, "bin", "codex") }

// Profile is the Codex profile. It keeps no state.
type Profile struct{}

func init() { adapter.Register(Name, Profile{}) }

// efforts are the reasoning efforts codex takes (model_reasoning_effort).
var efforts = []string{"minimal", "low", "medium", "high", "xhigh"}

// Describe describes the profile. The harness version is hw's pin: the codex
// the profile was verified against. codex chooses a thread's id itself.
func (Profile) Describe() contract.Descriptor {
	pin, _ := versions.Pinned(Name)
	return contract.Descriptor{
		Contract: contract.Version,
		Harness:  contract.HarnessInfo{Name: Name, Version: pin, Adapter: adapter.Name()},
		Capabilities: []contract.Capability{
			contract.CapResume, contract.CapRateLimits, contract.CapRetryVisible,
		},
		CheckpointFormat: CheckpointFormat,
		CredentialKinds:  []string{CredentialAPIKey, CredentialAccessToken},
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
