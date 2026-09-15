package contain

import "github.com/olesho/harness-wrapper/pkg/containment"

// Preview is what contain-check prints: the policy a launch would apply,
// what the kernel and host offer, and every requirement that would refuse the
// launch. It is a preview — it neither starts the harness nor certifies that
// enforcement would succeed — and private directories not yet allocated appear
// as placeholders ($STATE/home, $STATE/tmp, $TERMINAL). The applied policy a
// launch reports records the real paths.
type Preview struct {
	// Preview is always true, so the output can never pass for an applied
	// policy.
	Preview bool `json:"preview"`
	// Planned is the policy the launch would apply.
	Planned *containment.Applied `json:"planned,omitempty"`
	// KernelABI is the kernel's Landlock ABI (0 when unavailable) and Kernel
	// explains an unavailable one.
	KernelABI int    `json:"kernel_abi"`
	Kernel    string `json:"kernel,omitempty"`
	// Supervision is "cgroup" or "none", with the reason for "none".
	Supervision       string `json:"supervision"`
	SupervisionReason string `json:"supervision_reason,omitempty"`
	// Omitted lists optional profile paths absent on this host.
	Omitted []string `json:"omitted,omitempty"`
	// Missing lists every requirement that would refuse the launch.
	Missing []string `json:"missing,omitempty"`
	// Notes lists things that would not refuse the launch but will likely
	// break the harness, such as Git metadata outside every grant.
	Notes []string `json:"notes,omitempty"`
	// WouldLaunch is false when Missing is not empty.
	WouldLaunch bool `json:"would_launch"`
}
