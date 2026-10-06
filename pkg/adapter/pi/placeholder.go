package pi

import (
	"strings"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

var _ adapter.ModelPlaceholderer = Profile{}

// PlaceholderFor renders what stands in for an API key behind an egress
// broker: a key of its shape (its prefix kept, the rest drawn from the
// nonce), which the transport writes into auth.json as it would the key, and
// one swap narrowed to the hosts and headers of the provider the agent's model
// names. The route holds every provider's; the key goes to its own alone.
func (Profile) PlaceholderFor(kind, model string, credential, nonce []byte) (contract.PlaceholderResult, error) {
	if kind != CredentialKind {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeUnsupported, Field: "kind", Message: "no route for " + kind}
	}
	_, _, p, ok := splitModel(model)
	if !ok {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "model", Message: "names no provider the profile serves"}
	}
	key := strings.TrimSpace(string(credential))
	if key == "" || strings.ContainsAny(key, "\n\r\x00") {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "credential", Message: "not an API key: empty, or more than one line"}
	}
	if subscriptionToken(key) {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "credential", Message: "a subscription token, not an API key"}
	}
	ph := adapter.TokenPlaceholder(key, nonce)
	return contract.PlaceholderResult{
		File:  []byte(ph),
		Swaps: []contract.Swap{{Placeholder: ph, Secret: key, Hosts: p.hosts, Headers: p.headers}},
	}, nil
}
