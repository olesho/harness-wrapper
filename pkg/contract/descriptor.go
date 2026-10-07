package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Descriptor is what an adapter offers: Describe's result, and the entry a
// Runtime's descriptor lists for it verbatim.
type Descriptor struct {
	// Contract is the contract version the adapter implements.
	Contract string `json:"contract"`
	// Harness names the harness and the adapter.
	Harness HarnessInfo `json:"harness"`
	// Capabilities are the optional behaviours the adapter provides.
	Capabilities []Capability `json:"capabilities"`
	// CheckpointFormat is the checkpoint format the adapter writes. It reads
	// every earlier format of its lineage.
	CheckpointFormat int `json:"checkpoint_format"`
	// CredentialKinds are the credential kinds Open accepts.
	CredentialKinds []string `json:"credential_kinds"`
	// Spec names every Agent Spec field the harness honours, with the values
	// it accepts. A field or value absent is unsupported, and Provision
	// refuses a spec that uses it with CodeUnsupported.
	Spec SpecSupport `json:"spec"`
	// Limits bounds what the adapter takes.
	Limits Limits `json:"limits"`
	// Load says which saved Sessions the adapter continues in a fresh
	// environment (capability session_load); nil without it.
	Load *LoadSupport `json:"load,omitempty"`
	// Egress is the network the harness reaches, and where it presents each
	// credential kind it takes as a placeholder (capability
	// brokered_credentials); nil without it.
	Egress *Egress `json:"egress,omitempty"`
	// Keeper is what a login the runtime keeps for the harness is lent as
	// (capability login_keeper); nil without it.
	Keeper *KeeperSupport `json:"keeper,omitempty"`
}

// LoadSupport is the saved Sessions an adapter loads: each entry a
// combination it has passed with a Session saved that way, never one inferred
// from an order of versions.
type LoadSupport struct {
	// Formats are the archive formats its load recipe — what is saved, and
	// where it goes — is written for.
	Formats []int `json:"formats"`
	// Sources are the harness versions whose saved Sessions the adapter
	// continues at its own harness version.
	Sources []string `json:"sources"`
}

// Loads reports whether d's adapter continues a Session the harness version
// saved in an archive of format.
func (d Descriptor) Loads(format int, harnessVersion string) bool {
	if d.Load == nil || !d.Has(CapSessionLoad) {
		return false
	}
	ok := false
	for _, f := range d.Load.Formats {
		ok = ok || f == format
	}
	return ok && supports(d.Load.Sources, harnessVersion)
}

// HarnessInfo names a harness, its version and its adapter.
type HarnessInfo struct {
	// Name is the harness's registered name, like claude-code.
	Name string `json:"name"`
	// Version is the harness version the adapter is pinned to.
	Version string `json:"version"`
	// Adapter is the adapter and its release, like "harness-wrapper v0.20.0".
	Adapter string `json:"adapter"`
}

// Has reports whether d declares capability c.
func (d Descriptor) Has(c Capability) bool {
	for _, have := range d.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// Capability is a named behaviour an adapter may offer. Each adds exactly
// what its constant says; without it, the operations answer CodeUnsupported,
// or the observations never appear. The set is closed within a minor version;
// 1.1 added session_load and autonomous_turns, 1.2 brokered_credentials and
// login_keeper, 1.3 background_turns (its background_tasks observation in
// 1.4), 1.6 concurrent_sessions.
type Capability string

// Capabilities.
const (
	// CapResume: Open with mode reopen continues the Session.
	CapResume Capability = "resume"
	// CapAssignSessionID: Open may choose the session id.
	CapAssignSessionID Capability = "assign_session_id"
	// CapPrompts: prompt_raised and prompt_resolved observations, Answer, and
	// the awaiting_answer phase.
	CapPrompts Capability = "prompts"
	// CapStreamingText: text_delta observations.
	CapStreamingText Capability = "streaming_text"
	// CapToolsObserved: tool_started and tool_finished observations.
	CapToolsObserved Capability = "tools_observed"
	// CapSubagents: subagent_started and subagent_stopped observations.
	CapSubagents Capability = "subagents"
	// CapRateLimits: rate_limit observations.
	CapRateLimits Capability = "rate_limits"
	// CapRetryVisible: retrying observations while the harness retries a
	// model call.
	CapRetryVisible Capability = "retry_visible"
	// CapSessionLoad: a Session saved in one environment continues in
	// another: Provision takes ProvisionRequest.Load and answers the
	// history's relocations, and Open takes OpenRequest.Loaded. The
	// Descriptor's Load names the saved Sessions it takes.
	CapSessionLoad Capability = "session_load"
	// CapAutonomousTurns: the harness may start a turn no input asked for.
	// Such a turn is reported as turn_started and turn_ended naming a turn
	// and no input, live and from the record; while it runs the Session is
	// busy with State.TurnID and no State.InputID; Interrupt may name it by
	// InterruptRequest.TurnID; and Send stops it before it submits its
	// input. Once an interrupt or an input stopped such a turn, the harness
	// starts none of its own until an input's turn completes or the Session
	// is reopened.
	CapAutonomousTurns Capability = "autonomous_turns"
	// CapBackgroundTurns: the harness starts a turn of its own when work it
	// began in the background ends — a background command or subagent — to
	// take that work's result up. Such a turn is reported as turn_started and
	// turn_ended naming a turn and no input, live, and its end from the
	// record too; while it runs the Session is busy with State.TurnID and no
	// State.InputID, and Send answers busy as during an input's turn; and
	// Interrupt may name it by InterruptRequest.TurnID. Unlike
	// autonomous_turns, the harness starts one whenever background work ends,
	// whatever stopped one before. Since 1.4 the work itself is reported as
	// it changes, live (background_tasks), and State.Background lists it: a
	// Session whose harness works on in the background is idle between
	// turns, yet not at rest.
	CapBackgroundTurns Capability = "background_turns"
	// CapBrokeredCredentials: the harness can work with placeholders for the
	// credential kinds the Descriptor's Egress routes, through an egress
	// broker that substitutes them: Placeholder renders a credential's
	// placeholder, and the harness reaches the network only at the hosts
	// Egress names, through the proxy in HTTPS_PROXY, trusting the
	// certificates in SSL_CERT_FILE.
	CapBrokeredCredentials Capability = "brokered_credentials"
	// CapLoginKeeper: the runtime can keep the harness's subscription login
	// itself, outside every agent: Keep opens a keeper that signs in with a
	// device code, has the harness's own client refresh the login, and lends
	// its credential as the kind the Descriptor's Keeper names.
	CapLoginKeeper Capability = "login_keeper"
	// CapConcurrentSessions: several Sessions of one agent may be open at
	// once, from one Host or a Host each, over one Layout and one staged
	// credential — at most Limits.MaxSessions of them. Each keeps its own
	// inputs, turn, record, observations and checkpoint; what one does
	// reaches no other; and a record handle on one may be open while others
	// run. Without it, one Session of an agent is open at a time.
	CapConcurrentSessions Capability = "concurrent_sessions"
)

// Values lists the set.
func (Capability) Values() []string {
	return []string{
		"resume", "assign_session_id", "prompts", "streaming_text", "tools_observed", "subagents", "rate_limits", "retry_visible",
		"session_load", "autonomous_turns", "brokered_credentials", "login_keeper", "background_turns",
		"concurrent_sessions",
	}
}

// Since is the minor version that added c to the set — 0 for the 1.0
// set — or -1 for a capability not in it. A Descriptor declares c only from
// that minor on (CheckCapabilities).
func (c Capability) Since() int {
	switch c {
	case CapResume, CapAssignSessionID, CapPrompts, CapStreamingText, CapToolsObserved, CapSubagents, CapRateLimits, CapRetryVisible:
		return 0
	case CapSessionLoad, CapAutonomousTurns:
		return 1
	case CapBrokeredCredentials, CapLoginKeeper:
		return 2
	case CapBackgroundTurns:
		return 3
	case CapConcurrentSessions:
		return 6
	}
	return -1
}

// CheckCapabilities refuses a Descriptor that declares a capability newer
// than the minor of its contract version: a caller of that minor would not
// know it, and a newer caller, Compatible with it, would rely on what the
// declared minor never promised. A capability not in the set is left to the
// caller, which knows its own set.
func CheckCapabilities(d Descriptor) error {
	minor, err := MinorOf(d.Contract)
	if err != nil {
		return &Error{Code: CodeProtocol, Field: "contract", Message: err.Error()}
	}
	for _, c := range d.Capabilities {
		if since := c.Since(); since > minor {
			return &Error{Code: CodeProtocol, Field: "capabilities", Message: fmt.Sprintf("%s is from %s%d.%d, and the adapter declares %s", c, versionPrefix, Major, since, d.Contract)}
		}
	}
	return nil
}

// SpecSupport is the part of the Agent Spec a harness honours.
type SpecSupport struct {
	// Models is the model ids the harness takes, or any.
	Models Models `json:"models"`
	// Efforts is the effort levels it takes.
	Efforts []string `json:"efforts,omitempty"`
	// Instructions is the instruction kinds it takes: persona, workspace.
	Instructions []string `json:"instructions,omitempty"`
	// Skills says it installs skills.
	Skills bool `json:"skills,omitempty"`
	// Memory says it keeps a memory directory and seeds its files.
	Memory bool `json:"memory,omitempty"`
	// Connectors is the connector transports it takes: stdio, http.
	Connectors []string `json:"connectors,omitempty"`
	// PermissionPostures is the postures it takes: bypass, gated.
	PermissionPostures []string `json:"permission_postures,omitempty"`
	// InputContent is the input content types Send takes; 1.0 has text only.
	InputContent []string `json:"input_content,omitempty"`
}

// Models is either any model id, or a list of them. Its JSON form is the
// string "any" or an array.
type Models struct {
	Any bool
	IDs []string
}

// Allows reports whether model is one the harness takes. The empty model — the
// harness's default — is always taken.
func (m Models) Allows(model string) bool {
	if model == "" || m.Any {
		return true
	}
	for _, id := range m.IDs {
		if id == model {
			return true
		}
	}
	return false
}

// MarshalJSON writes "any" or the list.
func (m Models) MarshalJSON() ([]byte, error) {
	if m.Any {
		return []byte(`"any"`), nil
	}
	if m.IDs == nil {
		return []byte(`[]`), nil
	}
	return json.Marshal(m.IDs)
}

// UnmarshalJSON reads "any" or a list.
func (m *Models) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != "any" {
			return fmt.Errorf("models: %q, want \"any\" or a list", s)
		}
		*m = Models{Any: true}
		return nil
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return fmt.Errorf("models: %w", err)
	}
	*m = Models{IDs: ids}
	return nil
}

// Limits bounds what an adapter takes.
type Limits struct {
	// MaxInputBytes bounds one Send's content, at most MaxInputBytes.
	MaxInputBytes int `json:"max_input_bytes"`
	// MaxSessions is how many Sessions of one agent may be open at once
	// (capability concurrent_sessions), at least 2: the most the adapter's
	// concurrency conformance has passed with the real harness at the version
	// it pins — never a number it has not run. 0 without the capability.
	MaxSessions int `json:"max_sessions,omitempty"`
}

// Sessions is how many Sessions of one agent may be open at once:
// Limits.MaxSessions with capability concurrent_sessions, 1 without.
func (d Descriptor) Sessions() int {
	if d.Has(CapConcurrentSessions) && d.Limits.MaxSessions > 1 {
		return d.Limits.MaxSessions
	}
	return 1
}

// CheckSessions refuses a Descriptor whose Session limit is malformed: the
// capability with fewer than 2 Sessions, or a limit without it.
func CheckSessions(d Descriptor) error {
	switch n := d.Limits.MaxSessions; {
	case d.Has(CapConcurrentSessions) && n < 2:
		return &Error{Code: CodeProtocol, Field: "limits.max_sessions", Message: fmt.Sprintf("%d with %s, want at least 2", n, CapConcurrentSessions)}
	case !d.Has(CapConcurrentSessions) && n != 0:
		return &Error{Code: CodeProtocol, Field: "limits.max_sessions", Message: fmt.Sprintf("%d declared without capability %s", n, CapConcurrentSessions)}
	}
	return nil
}

// Supports reports whether values holds v.
func supports(values []string, v string) bool {
	for _, have := range values {
		if have == v {
			return true
		}
	}
	return false
}
