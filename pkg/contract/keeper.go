package contract

import (
	"context"
	"fmt"
	"time"
)

// A runtime may keep a harness's subscription login itself, outside every
// agent (capability login_keeper): signed in once with a device code, which
// the person the login belongs to approves wherever they are; kept fresh by
// the harness's own client, which alone refreshes it; and lent to the
// runtime's agents behind an egress broker as a credential of the kind
// Descriptor.Keeper names, without what refreshes it. An agent that never
// holds the refresh token cannot spend it, and the login it was lent goes on
// working after the agent is gone.

// KeeperSupport is what a harness's keeper lends.
type KeeperSupport struct {
	// Kind is the credential kind a kept login is lent as: one the harness
	// takes, and one Egress routes.
	Kind string `json:"kind"`
}

// KeeperRequest opens a keeper.
type KeeperRequest struct {
	// Contract is the version the request is written in.
	Contract string `json:"contract"`
	// HarnessRoot is the harness distribution's root, as Provision's.
	HarnessRoot string `json:"harness_root"`
	// Home is the keeper's own directory, where the harness keeps the login:
	// a clean absolute path only the runtime's keeper identity reaches,
	// which no agent can.
	Home string `json:"home"`
}

// Keeper keeps one login. Its methods are safe to call concurrently.
type Keeper interface {
	// SignIn starts a device-code sign-in, in place of one under way, and
	// returns where to go and the code to enter there. It completes, or
	// fails, on its own; Status says which.
	SignIn(ctx context.Context) (DeviceCode, error)
	// Status is the login's state.
	Status(ctx context.Context) (LoginStatus, error)
	// Refresh has the harness's own client refresh the login now, before
	// its credential expires. A refresh the harness's service refused fails,
	// and Status's Error says why.
	Refresh(ctx context.Context) error
	// Lend is the login's current credential, of the kind KeeperSupport
	// names and as Placeholder takes it, with nothing that refreshes it, and
	// when it expires. Without a login it fails with CodeUnexpected. Its
	// result is a secret: the runtime hands it to Placeholder and its broker
	// alone.
	Lend(ctx context.Context) (Lent, error)
	// SignOut forgets the login, and any sign-in under way.
	SignOut(ctx context.Context) error
	// Close stops whatever the keeper runs. The login stays in its home.
	Close() error
}

// DeviceCode is a device-code sign-in under way: the person the login
// belongs to opens URL and enters Code.
type DeviceCode struct {
	URL  string `json:"url"`
	Code string `json:"code"`
}

// LoginState is where a kept login stands.
type LoginState string

// Login states.
const (
	// LoginSignedOut: there is no login.
	LoginSignedOut LoginState = "signed_out"
	// LoginSigningIn: a device-code sign-in waits to be approved.
	LoginSigningIn LoginState = "signing_in"
	// LoginSignedIn: Lend lends a credential that has not expired.
	LoginSignedIn LoginState = "signed_in"
	// LoginExpired: the credential Lend lends has expired. Refresh may renew
	// it; when the harness's service refuses, Error says why, and the login
	// needs a new sign-in.
	LoginExpired LoginState = "expired"
)

// Values lists the set.
func (LoginState) Values() []string {
	return []string{"signed_out", "signing_in", "signed_in", "expired"}
}

// LoginStatus is a kept login's state. It holds no secret.
type LoginStatus struct {
	State LoginState `json:"state"`
	// Account names the account as the harness shows it, an email, when
	// known.
	Account string `json:"account,omitempty"`
	// Plan is the account's plan, when known.
	Plan string `json:"plan,omitempty"`
	// Expiry is when the credential Lend lends expires.
	Expiry *time.Time `json:"expiry,omitempty"`
	// Error is why the last sign-in or refresh failed, as the harness said
	// it, with no credential in it.
	Error string `json:"error,omitempty"`
}

// Lent is a kept login's credential, lent.
type Lent struct {
	// Credential is the credential, as Placeholder takes it. It is a
	// secret.
	Credential []byte `json:"credential"`
	// Expiry is when it expires: the runtime refreshes the login before.
	Expiry time.Time `json:"expiry"`
}

// String describes l without its credential.
func (l Lent) String() string {
	return fmt.Sprintf("lent{%d bytes, expires %s}", len(l.Credential), l.Expiry.UTC().Format(time.RFC3339))
}

// GoString describes l without its credential.
func (l Lent) GoString() string { return l.String() }

// CheckKeeper refuses a Descriptor whose keeper is malformed: one without the
// capability, or lending a kind the harness does not take or Egress does not
// route.
func CheckKeeper(d Descriptor) error {
	invalid := func(format string, args ...any) error {
		return &Error{Code: CodeProtocol, Field: "keeper", Message: fmt.Sprintf(format, args...)}
	}
	switch k := d.Keeper; {
	case k == nil && d.Has(CapLoginKeeper):
		return invalid("%s, and no keeper", CapLoginKeeper)
	case k == nil:
		return nil
	case !d.Has(CapLoginKeeper):
		return invalid("declared without capability %s", CapLoginKeeper)
	case !supports(d.CredentialKinds, k.Kind):
		return invalid("lends %q, a kind the harness does not take", k.Kind)
	}
	if _, ok := d.Egress.Route(d.Keeper.Kind); !ok || !d.Has(CapBrokeredCredentials) {
		return invalid("lends %q, which Egress does not route: a kept login is lent behind a broker", d.Keeper.Kind)
	}
	return nil
}
