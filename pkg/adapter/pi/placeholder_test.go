package pi

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

var nonce = []byte("0123456789abcdef")

func lookup(t *testing.T) contract.Adapter {
	t.Helper()
	a, ok := contract.Lookup(Name)
	if !ok {
		t.Fatal("not registered")
	}
	return a
}

// An API key's placeholder is a key of its shape, and its one swap goes to
// the hosts and headers of the provider the model names: the route holds
// every provider's.
func TestPlaceholder(t *testing.T) {
	if err := contract.CheckEgress(Profile{}.Describe()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		model, key, prefix string
		hosts, headers     []string
	}{
		{"anthropic/claude-haiku-4-5", "sk-ant-api03-" + strings.Repeat("k", 40), "sk-ant-api03-", []string{"api.anthropic.com"}, []string{"x-api-key"}},
		{"openai/gpt-4.1-mini", "sk-proj-" + strings.Repeat("k", 40), "sk-proj-", []string{"api.openai.com"}, []string{"Authorization"}},
		{"google/gemini-2.5-flash", "AIza" + strings.Repeat("k", 35), "", []string{"generativelanguage.googleapis.com"}, []string{"x-goog-api-key"}},
		{"openrouter/anthropic/claude-sonnet-4.5", "sk-or-v1-" + strings.Repeat("k", 40), "sk-or-v1-", []string{"openrouter.ai"}, []string{"x-api-key", "Authorization"}},
	} {
		res, err := lookup(t).Placeholder(contract.PlaceholderRequest{
			Contract: contract.Version, Kind: CredentialKind, Model: c.model, Credential: []byte(c.key + "\n"), Nonce: nonce,
		})
		if err != nil {
			t.Fatalf("%s: %v", c.model, err)
		}
		ph := string(res.File)
		if strings.Contains(ph, "kkkk") || !strings.HasPrefix(ph, c.prefix) || len(ph) < len(c.key) {
			t.Errorf("%s: placeholder %q, want the key's shape and none of it", c.model, ph)
		}
		if len(res.Swaps) != 1 {
			t.Fatalf("%s: swaps %+v", c.model, res.Swaps)
		}
		s := res.Swaps[0]
		if s.Placeholder != ph || s.Secret != c.key || !slices.Equal(s.Hosts, c.hosts) || !slices.Equal(s.Headers, c.headers) {
			t.Errorf("%s: swap %+v, want hosts %v, headers %v", c.model, s, c.hosts, c.headers)
		}
	}
}

// A model naming no provider the profile serves has no hosts to narrow the
// swap to, and a subscription token is no API key: both are refused, the
// error repeating nothing of the credential.
func TestPlaceholderRefused(t *testing.T) {
	for _, c := range []struct {
		model, key, field string
	}{
		{"nonesuch/model", "sk-SECRET-SECRET-SECRET", "model"},
		{"claude-haiku-4-5", "sk-SECRET-SECRET-SECRET", "model"},
		{"", "sk-SECRET-SECRET-SECRET", "model"},
		{"anthropic/claude-haiku-4-5", "sk-ant-oat01-SECRET-SECRET", "credential"},
		{"anthropic/claude-haiku-4-5", "sk-SECRET\nsk-SECRET", "credential"},
	} {
		_, err := lookup(t).Placeholder(contract.PlaceholderRequest{
			Contract: contract.Version, Kind: CredentialKind, Model: c.model, Credential: []byte(c.key), Nonce: nonce,
		})
		var ce *contract.Error
		if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidSpec || ce.Field != c.field || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%q, %q: %v, want invalid_spec on %s", c.model, c.key, err, c.field)
		}
	}
}
