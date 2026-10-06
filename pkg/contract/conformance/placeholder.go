package conformance

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// placeholderScenario: what stands in for the fixture's credential
// (capability brokered_credentials) is a function of its request, holds to
// its kind's route and keeps the secret out of the file, and a Session opens
// and runs a turn on it, as on the credential. The request names the
// fixture's model; a model the harness cannot place is refused on field
// model, or answered within the route all the same. Without the capability,
// Placeholder is unsupported.
func placeholderScenario(c *check) {
	nonce := func(seed byte) []byte {
		b := make([]byte, contract.MinNonceBytes)
		for i := range b {
			b[i] = seed + byte(i)
		}
		return b
	}
	if !c.has(contract.CapBrokeredCredentials) {
		_, err := c.f.Adapter.Placeholder(contract.PlaceholderRequest{Contract: contract.Version, Kind: "token", Credential: []byte("token"), Nonce: nonce(1)})
		if codeOf(err) != contract.CodeUnsupported {
			c.fail("placeholder.unsupported", "Placeholder without %s: %v, want unsupported", contract.CapBrokeredCredentials, err)
		}
		return
	}
	a := c.roots()
	if a.cred == nil {
		c.t.Logf("the harness takes no credential: nothing stands in for one")
		return
	}
	route, ok := c.desc.Egress.Route(a.cred.Kind)
	if !ok {
		c.t.Logf("the fixture's credential kind %s has no route: it is never brokered", a.cred.Kind)
		return
	}
	cred, err := os.ReadFile(a.cred.File)
	if err != nil {
		c.stop("setup", "reading the staged credential: %v", err)
	}
	req := contract.PlaceholderRequest{Contract: contract.Version, Kind: a.cred.Kind, Model: c.f.Spec.Model, Credential: cred, Nonce: nonce(1)}
	got, err := c.f.Adapter.Placeholder(req)
	if err != nil {
		c.stop("placeholder.valid", "Placeholder: %v", err)
	}
	if err := got.Validate(route); err != nil {
		c.fail("placeholder.valid", "%v", err)
	}
	if again, err := c.f.Adapter.Placeholder(req); err != nil || !reflect.DeepEqual(again, got) {
		c.fail("placeholder.pure", "the same request rendered another placeholder (%v)", err)
	}
	req.Nonce = nonce(2)
	if other, err := c.f.Adapter.Placeholder(req); err != nil || len(other.Swaps) == 0 || other.Swaps[0].Placeholder == got.Swaps[0].Placeholder {
		c.fail("placeholder.nonce", "another nonce rendered the same placeholder (%v)", err)
	}
	// A model narrows the swaps, or goes unread: one the harness cannot place
	// is refused on field model, and never widens a swap past the route.
	req.Model = "nonesuch/conformance-unknown-model"
	if other, err := c.f.Adapter.Placeholder(req); err != nil {
		var ce *contract.Error
		if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidSpec || ce.Field != "model" {
			c.fail("placeholder.model", "a model the harness cannot place: %v, want invalid_spec on field model", err)
		}
	} else if err := other.Validate(route); err != nil {
		c.fail("placeholder.model", "a model the harness cannot place: %v", err)
	}
	req.Model = c.f.Spec.Model
	req.Nonce = nonce(1)[:contract.MinNonceBytes-1]
	if _, err := c.f.Adapter.Placeholder(req); codeOf(err) != contract.CodeProtocol {
		c.fail("placeholder.nonce", "a nonce of %d bytes: %v, want protocol", len(req.Nonce), err)
	}
	req.Nonce, req.Kind = nonce(1), "unrouted_kind"
	if _, err := c.f.Adapter.Placeholder(req); codeOf(err) != contract.CodeUnsupported {
		c.fail("placeholder.unsupported", "a kind with no route: %v, want unsupported", err)
	}
	// Errors never quote the credential.
	req.Kind, req.Credential = a.cred.Kind, []byte("x\n"+strings.Repeat("secret-in-a-broken-credential\n", 2))
	if _, err := c.f.Adapter.Placeholder(req); err != nil && strings.Contains(err.Error(), "secret-in-a-broken-credential") {
		c.fail("placeholder.valid", "an error quotes the credential: %v", err)
	}

	staged := filepath.Join(a.layout.Secrets, "placeholder")
	if err := os.WriteFile(staged, got.File, 0o600); err != nil {
		c.stop("setup", "staging the placeholder: %v", err)
	}
	a.cred = &contract.CredentialFile{Kind: a.cred.Kind, File: staged}
	res, err := c.provision(a, nil)
	if err != nil {
		c.stop("provision.valid", "Provision: %v", err)
	}
	if err := Apply(a.layout, res); err != nil {
		c.stop("provision.valid", "applying the result: %v", err)
	}
	a.result = res
	s, h := c.openSession(a)
	in := c.send(s, "PING 7")
	if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted || !strings.Contains(end.Text, "PONG 7") {
		c.fail("placeholder.opens", "a turn on the placeholder: %+v, want completed with PONG 7", end)
	}
}
