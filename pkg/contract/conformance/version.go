package conformance

import (
	"context"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The scenario of the version policy (1.7: AgentSpec.VersionPolicy and
// OpenResult.HarnessVersion): an unknown policy is refused; a Session reports
// the version its harness runs; under strict, a harness that is not the pin
// is refused with version_unsupported, and under flexible — the default — it
// opens and reports its own version.

// versionPolicy: the version policy, as 1.7 has it.
func versionPolicy(c *check) {
	if minor, err := contract.MinorOf(c.desc.Contract); err != nil || minor < contract.VersionPolicySince {
		c.t.Logf("contract %s, before version policies: skipped", c.desc.Contract)
		return
	}
	pin := c.desc.Harness.Version

	a := c.roots()
	req := c.request(a, nil)
	req.Spec.VersionPolicy = "lenient"
	var e *contract.Error
	if _, err := c.f.Adapter.Provision(req); !asErr(err, &e) || e.Code != contract.CodeInvalidSpec || e.Field != "version_policy" {
		c.fail("version.invalid", "policy %q: %v, want invalid_spec on version_policy", req.Spec.VersionPolicy, err)
	}

	// The pin opens under either policy, and says it is the pin.
	for _, p := range []contract.VersionPolicy{"", contract.VersionStrict} {
		if res, err := c.openUnder(p); err != nil {
			c.fail("version.pin", "the pinned harness under policy %q: %v, want it opened", p, err)
		} else if res.HarnessVersion != pin {
			c.fail("version.reported", "under policy %q the Session reports version %q, and the harness is the pin %s", p, res.HarnessVersion, pin)
		}
	}

	if c.f.NonPin == nil {
		c.t.Logf("no NonPin: the checks of a harness other than the pin skipped")
		return
	}
	other, restore := c.f.NonPin(c.t)
	defer restore()
	if other == pin {
		c.stop("setup", "NonPin gave the pin %s", pin)
	}
	for _, p := range []contract.VersionPolicy{"", contract.VersionFlexible} {
		res, err := c.openUnder(p)
		switch {
		case err != nil:
			c.fail("version.flexible", "%s %s under policy %q: %v, want it opened", c.desc.Harness.Name, other, p, err)
		case res.HarnessVersion != other:
			c.fail("version.running", "%s %s under policy %q: the Session reports version %q", c.desc.Harness.Name, other, p, res.HarnessVersion)
		default:
			c.t.Logf("%s %s under policy %q opened, reporting %s", c.desc.Harness.Name, other, p, res.HarnessVersion)
		}
	}
	_, err := c.openUnder(contract.VersionStrict)
	if !asErr(err, &e) || e.Code != contract.CodeOpenFailed || e.Reason != contract.OpenVersionUnsupported {
		c.fail("version.strict", "%s %s under policy strict, the pin %s: %v, want open_failed version_unsupported", c.desc.Harness.Name, other, pin, err)
	} else {
		c.t.Logf("%s %s under policy strict refused: %v", c.desc.Harness.Name, other, err)
	}
}

// openUnder provisions a fresh agent under version policy p and opens a
// Session of it, which it closes before it returns.
func (c *check) openUnder(p contract.VersionPolicy) (contract.OpenResult, error) {
	spec := c.f.Spec
	if c.spec != nil {
		spec = *c.spec
	}
	spec.VersionPolicy = p
	saved := c.spec
	c.spec = &spec
	a := c.newAgent()
	c.spec = saved
	s, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenFresh, "", nil))
	if err != nil {
		return contract.OpenResult{}, err
	}
	ctx, cancel := c.ctx()
	defer cancel()
	res, err := s.Open(ctx)
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	_, _ = s.Close(cctx, contract.ClosePark, 0)
	return res, err
}
