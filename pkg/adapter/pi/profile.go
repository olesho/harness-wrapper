// Package pi is the Harness Adapter's Pi profile: the pi coding agent
// (@earendil-works/pi-coding-agent) driven over its RPC mode (pi --mode rpc:
// JSON lines on stdio), one process per Session, with the session's file as
// its record. Importing it registers the Harness Adapter under "pi", and
// links no other harness.
//
// pi records no id a client chooses for an input, so the profile's tag
// extension (hwtag.ts, loaded with -e) takes a tag off each input and writes
// it into the session as a custom entry, the parent of the input's user
// message, or its grandparent through the system message pi writes when the
// system prompt changed (probes/pirpc).
//
// The harness distribution under harness_root holds the pinned pi release's
// executable and the package.json it reads its version from (BinaryPath),
// and the tag extension (ExtensionPath).
package pi

import (
	_ "embed"
	"path/filepath"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Name is the harness's name: the profile registers under it.
const Name = "pi"

// CredentialKind is the one credential kind pi takes: an API key of the
// provider the agent's model names (providers.go). The profile writes it into
// auth.json under that provider at every launch.
const CredentialKind = "api_key"

// CheckpointFormat is the checkpoint format the profile writes: the session
// follower's checkpoint (transcript.Checkpoint) as JSON.
const CheckpointFormat = 1

// BinaryPath is pi's path in the harness distribution rooted at root: the
// release's executable, beside the release's package.json, which it reads
// its version from.
func BinaryPath(root string) string { return filepath.Join(root, "bin", "pi") }

// ExtensionPath is the tag extension's path in the distribution.
func ExtensionPath(root string) string { return filepath.Join(root, "hw-tag.ts") }

// Extension is the tag extension: an input that starts with <!--hw:ID-->
// loses the tag, and the session gets a custom entry hw.input {id} before the
// input's user message. Its command, /hw-tag, shows that it loaded.
//
//go:embed hwtag.ts
var Extension []byte

// Profile is the Pi profile. It keeps no state.
type Profile struct{}

func init() { adapter.Register(Name, Profile{}) }

// efforts are pi's thinking levels (--thinking).
var efforts = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// pinned is the pi version the profile is verified against.
func pinned() string {
	pin, _ := versions.Pinned(Name)
	return pin
}

// Describe describes the profile. The harness version is hw's pin: the pi the
// profile was verified against. The caller may choose a session's id; pi
// raises no prompts, and starts no turn by itself. Behind an egress broker it
// takes its API key as a placeholder: the route holds every provider's hosts
// and headers, and each placeholder's swap narrows it to the provider its
// model names.
func (Profile) Describe() contract.Descriptor {
	pin := pinned()
	hosts, headers := route()
	return contract.Descriptor{
		Contract: contract.Version,
		Harness:  contract.HarnessInfo{Name: Name, Version: pin, Adapter: adapter.Name()},
		Capabilities: []contract.Capability{
			contract.CapResume, contract.CapAssignSessionID, contract.CapToolsObserved,
			contract.CapRetryVisible, contract.CapBrokeredCredentials,
		},
		Egress: &contract.Egress{
			Credentials: []contract.CredentialRoute{{Kind: CredentialKind, Hosts: hosts, Headers: headers}},
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
