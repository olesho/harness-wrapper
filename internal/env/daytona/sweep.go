// Daytona reaper (design doc Tier-4): finds and deletes orphaned sandboxes by
// label so a crashed live-test run doesn't leak billed resources.

package daytona

import (
	"context"
	"errors"
)

// SweepResult accounts for a sweep pass.
type SweepResult struct {
	Swept  []string
	Kept   []string
	Failed []SweepFailure
}

// SweepFailure records a sandbox that could not be deleted.
type SweepFailure struct {
	ID    string
	Error string
}

// SweepOpts parameterizes a sweep.
type SweepOpts struct {
	// Labels: a sandbox is deleted only when it matches ALL of these. Empty is
	// refused — an unscoped sweep would delete every sandbox in the account.
	Labels map[string]string
	// DryRun reports the match set (as Kept) without deleting anything.
	DryRun bool
}

// Sweep lists sandboxes and deletes every one matching ALL of opts.Labels.
//
// Empty labels are refused (billing-safety backstop). DryRun reports the match
// set without deleting.
func Sweep(ctx context.Context, config DaytonaConfig, opts SweepOpts) (SweepResult, error) {
	if len(opts.Labels) == 0 {
		return SweepResult{}, errors.New("sweep: empty labels would match every sandbox in the account — refusing")
	}

	ctor, err := loadDaytonaClass(config)
	if err != nil {
		return SweepResult{}, err
	}
	client, err := ctor(clientConfigFor(config))
	if err != nil {
		return SweepResult{}, err
	}

	sandboxes, err := client.List(ctx, ListQuery{})
	if err != nil {
		return SweepResult{}, err
	}

	// Result slices are non-nil so callers can range without a nil guard.
	result := SweepResult{Swept: []string{}, Kept: []string{}, Failed: []SweepFailure{}}
	for _, sandbox := range sandboxes {
		if !matchesLabels(sandbox, opts.Labels) {
			continue
		}
		id := sandboxID(sandbox)
		if opts.DryRun {
			result.Kept = append(result.Kept, id)
			continue
		}
		if err := sandbox.Delete(ctx, 60); err != nil {
			result.Failed = append(result.Failed, SweepFailure{ID: id, Error: err.Error()})
			continue
		}
		result.Swept = append(result.Swept, id)
	}
	return result, nil
}

func sandboxID(sandbox DaytonaSandbox) string {
	if id := sandbox.ID(); id != "" {
		return id
	}
	if id := sandbox.SandboxID(); id != "" {
		return id
	}
	return "<unknown>"
}

func matchesLabels(sandbox DaytonaSandbox, want map[string]string) bool {
	have := sandbox.Labels()
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
