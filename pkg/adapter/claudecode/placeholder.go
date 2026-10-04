package claudecode

import (
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Placeholder renders a placeholder token for claude's token: shaped like it,
// prefix and all, so claude takes it for the kind of token it is, and staged
// where the token would be; claude sends it to APIHost, and the broker swaps
// it for the token there.
func (p Profile) Placeholder(kind string, credential, nonce []byte) (contract.PlaceholderResult, error) {
	route, _ := p.Describe().Egress.Route(kind)
	return adapter.TokenResult(credential, nonce, route)
}
