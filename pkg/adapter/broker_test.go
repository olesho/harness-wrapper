package adapter

import (
	"errors"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

func nonceOf(seed byte) []byte {
	b := make([]byte, contract.MinNonceBytes)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// A placeholder token keeps the token's lower-case prefix and length, draws
// the rest from the nonce, and is the same for the same token and nonce.
func TestTokenPlaceholder(t *testing.T) {
	cases := map[string]string{
		"sk-ant-oat01-" + strings.Repeat("Ab9_", 24): "sk-ant-oat01-",
		"sk-proj-" + strings.Repeat("Q", 120):        "sk-proj-",
		"sk-" + strings.Repeat("x", 40):              "sk-",
		"ghp_" + strings.Repeat("Z", 36):             "",
		"UPPER-case-token-value":                     "",
		"short":                                      "",
	}
	for tok, prefix := range cases {
		ph := TokenPlaceholder(tok, nonceOf(1))
		if !strings.HasPrefix(ph, prefix) {
			t.Errorf("%q: placeholder %q lacks prefix %q", tok, ph, prefix)
		}
		if want := max(len(tok), len(prefix)+32); len(ph) != want {
			t.Errorf("%q: placeholder of %d bytes, want %d", tok, len(ph), want)
		}
		if strings.Contains(ph, strings.TrimPrefix(tok, prefix)) {
			t.Errorf("%q: the placeholder keeps the token's rest", tok)
		}
		if again := TokenPlaceholder(tok, nonceOf(1)); again != ph {
			t.Errorf("%q: not a function of token and nonce", tok)
		}
		if other := TokenPlaceholder(tok, nonceOf(2)); other == ph {
			t.Errorf("%q: another nonce gives the same placeholder", tok)
		}
	}
	if Drawn(nonceOf(1), "a", 40) == Drawn(nonceOf(1), "b", 40) {
		t.Error("two uses draw the same")
	}
}

// brokerProfile is a profile that routes "token" and renders its placeholder,
// or a bad one when told to.
type brokerProfile struct {
	*fakeProfile
	caps []contract.Capability
	bad  bool
}

func (p brokerProfile) Describe() contract.Descriptor {
	d := p.fakeProfile.Describe()
	d.Capabilities = append(d.Capabilities, p.caps...)
	d.CredentialKinds = []string{"token"}
	d.Egress = &contract.Egress{Credentials: []contract.CredentialRoute{{Kind: "token", Hosts: []string{"api.example.com"}, Headers: []string{"Authorization"}}}}
	return d
}

type renderingProfile struct{ brokerProfile }

func (p renderingProfile) Placeholder(kind string, credential, nonce []byte) (contract.PlaceholderResult, error) {
	route, _ := p.Describe().Egress.Route(kind)
	res, err := TokenResult(credential, nonce, route)
	if err == nil && p.bad {
		res.File = append(res.File, credential...)
	}
	return res, err
}

// The shared adapter checks a Placeholder request against the Descriptor,
// and the profile's rendering against the kind's route.
func TestAdapterPlaceholder(t *testing.T) {
	const secret = "tok-SECRET-0123456789abcdef"
	req := func(mut func(*contract.PlaceholderRequest)) contract.PlaceholderRequest {
		r := contract.PlaceholderRequest{Contract: contract.Version, Kind: "token", Credential: []byte(secret + "\n"), Nonce: nonceOf(1)}
		if mut != nil {
			mut(&r)
		}
		return r
	}
	broker := []contract.Capability{contract.CapBrokeredCredentials}
	good := New(renderingProfile{brokerProfile{fakeProfile: newFakeProfile(), caps: broker}})
	res, err := good.Placeholder(req(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Swaps) != 1 || res.Swaps[0].Secret != secret || string(res.File) != res.Swaps[0].Placeholder {
		t.Errorf("result %+v", res)
	}
	codes := map[string]struct {
		a    contract.Adapter
		r    contract.PlaceholderRequest
		code contract.Code
	}{
		"no capability":    {New(renderingProfile{brokerProfile{fakeProfile: newFakeProfile()}}), req(nil), contract.CodeUnsupported},
		"unrouted kind":    {good, req(func(r *contract.PlaceholderRequest) { r.Kind = "other" }), contract.CodeUnsupported},
		"short nonce":      {good, req(func(r *contract.PlaceholderRequest) { r.Nonce = r.Nonce[:8] }), contract.CodeProtocol},
		"newer contract":   {good, req(func(r *contract.PlaceholderRequest) { r.Contract = "harness-adapter/1.99" }), contract.CodeProtocol},
		"empty credential": {good, req(func(r *contract.PlaceholderRequest) { r.Credential = nil }), contract.CodeInvalidSpec},
		"two lines":        {good, req(func(r *contract.PlaceholderRequest) { r.Credential = []byte(secret + "\n" + secret) }), contract.CodeInvalidSpec},
		"no rendering":     {New(brokerProfile{fakeProfile: newFakeProfile(), caps: broker}), req(nil), contract.CodeInternal},
		"bad rendering":    {New(renderingProfile{brokerProfile{fakeProfile: newFakeProfile(), caps: broker, bad: true}}), req(nil), contract.CodeInternal},
	}
	for name, c := range codes {
		_, err := c.a.Placeholder(c.r)
		var e *contract.Error
		if !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the error quotes the secret: %v", name, err)
		}
	}
}
