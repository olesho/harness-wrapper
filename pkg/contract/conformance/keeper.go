package conformance

import (
	"context"
	"path/filepath"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// keeperScenario: a keeper (capability login_keeper) starts signed out and
// lends nothing; a device-code sign-in, once approved, signs it in; it lends
// a credential its kind's placeholder takes, refreshed on asking; the login
// outlives the keeper in its home; and signing out forgets it. Without the
// capability, Keep is unsupported.
func keeperScenario(c *check) {
	home := filepath.Join(c.t.TempDir(), "keeper")
	req := contract.KeeperRequest{Contract: contract.Version, HarnessRoot: c.f.HarnessRoot, Home: home}
	if !c.has(contract.CapLoginKeeper) {
		if _, err := c.f.Adapter.Keep(req); codeOf(err) != contract.CodeUnsupported {
			c.fail("keeper.unsupported", "Keep without %s: %v, want unsupported", contract.CapLoginKeeper, err)
		}
		return
	}
	if c.f.Approve == nil {
		c.t.Logf("no way to approve a sign-in: the keeper is not run")
		return
	}
	k, err := c.f.Adapter.Keep(req)
	if err != nil {
		c.stop("keeper.open", "Keep: %v", err)
	}
	c.cleanups = append(c.cleanups, func() { _ = k.Close() })
	ctx, cancel := c.ctx()
	defer cancel()
	if st, err := k.Status(ctx); err != nil || st.State != contract.LoginSignedOut {
		c.fail("keeper.signed-out", "a new keeper: %+v %v, want signed_out", st, err)
	}
	if _, err := k.Lend(ctx); codeOf(err) != contract.CodeUnexpected {
		c.fail("keeper.lend-none", "Lend with no login: %v, want unexpected", err)
	}
	dc, err := k.SignIn(ctx)
	if err != nil || dc.URL == "" || dc.Code == "" {
		c.stop("keeper.sign-in", "SignIn: %+v %v, want a URL and a code", dc, err)
	}
	if st, err := k.Status(ctx); err != nil || st.State != contract.LoginSigningIn {
		c.fail("keeper.sign-in", "a sign-in under way: %+v %v, want signing_in", st, err)
	}
	c.f.Approve(c.t, k, dc)
	signedIn := func(k contract.Keeper) contract.LoginStatus {
		var st contract.LoginStatus
		for deadline := time.Now().Add(c.f.timeout()); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if st, err = k.Status(ctx); err == nil && st.State == contract.LoginSignedIn {
				break
			}
		}
		return st
	}
	if st := signedIn(k); st.State != contract.LoginSignedIn || st.Expiry == nil {
		c.stop("keeper.signed-in", "an approved sign-in: %+v, want signed_in with an expiry", st)
	}
	lend := func(rule string) contract.Lent {
		l, err := k.Lend(ctx)
		if err != nil {
			c.stop(rule, "Lend: %v", err)
		}
		if !l.Expiry.After(time.Now()) {
			c.fail(rule, "lent a credential that expired at %s", l.Expiry)
		}
		nonce := make([]byte, contract.MinNonceBytes)
		if _, err := c.f.Adapter.Placeholder(contract.PlaceholderRequest{
			Contract: contract.Version, Kind: c.desc.Keeper.Kind, Credential: l.Credential, Nonce: nonce,
		}); err != nil {
			c.fail(rule, "the lent credential has no placeholder: %v", err)
		}
		return l
	}
	first := lend("keeper.lend")
	if err := k.Refresh(ctx); err != nil {
		c.fail("keeper.refresh", "Refresh: %v", err)
	}
	if again := lend("keeper.refresh"); again.Expiry.Before(first.Expiry) {
		c.fail("keeper.refresh", "after a refresh the credential expires at %s, before %s", again.Expiry, first.Expiry)
	}
	if err := k.Close(); err != nil {
		c.fail("keeper.persists", "Close: %v", err)
	}
	k2, err := c.f.Adapter.Keep(req)
	if err != nil {
		c.stop("keeper.persists", "Keep again: %v", err)
	}
	c.cleanups = append(c.cleanups, func() { _ = k2.Close() })
	k = k2
	if st, err := k.Status(ctx); err != nil || st.State != contract.LoginSignedIn {
		c.fail("keeper.persists", "a keeper reopened on its home: %+v %v, want signed_in", st, err)
	}
	if err := k.SignOut(ctx); err != nil {
		c.fail("keeper.sign-out", "SignOut: %v", err)
	}
	if st, err := k.Status(ctx); err != nil || st.State != contract.LoginSignedOut {
		c.fail("keeper.sign-out", "after SignOut: %+v %v, want signed_out", st, err)
	}
	if _, err := k.Lend(context.Background()); codeOf(err) != contract.CodeUnexpected {
		c.fail("keeper.sign-out", "Lend after SignOut: %v, want unexpected", err)
	}
}
