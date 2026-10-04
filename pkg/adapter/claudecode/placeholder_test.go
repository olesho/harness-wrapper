package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Behind an egress broker claude reaches its model API alone, and a token's
// placeholder is a token of the same kind, presented there in Authorization.
func TestPlaceholder(t *testing.T) {
	d := Profile{}.Describe()
	if err := contract.CheckEgress(d); err != nil {
		t.Fatal(err)
	}
	a, ok := contract.Lookup(Name)
	if !ok {
		t.Fatal("not registered")
	}
	token := "sk-ant-oat01-" + strings.Repeat("REALtoken", 10)
	res, err := a.Placeholder(contract.PlaceholderRequest{
		Contract: contract.Version, Kind: CredentialKind, Credential: []byte(token + "\n"), Nonce: []byte("0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ph := string(res.File)
	if !strings.HasPrefix(ph, "sk-ant-oat01-") || len(ph) != len(token) || strings.Contains(ph, "REAL") {
		t.Errorf("placeholder %q for a token of %d bytes", ph, len(token))
	}
	want := contract.Swap{Placeholder: ph, Secret: token, Hosts: []string{APIHost}, Headers: []string{"Authorization"}}
	if len(res.Swaps) != 1 || res.Swaps[0].Placeholder != want.Placeholder || res.Swaps[0].Secret != want.Secret ||
		strings.Join(res.Swaps[0].Hosts, ",") != APIHost || strings.Join(res.Swaps[0].Headers, ",") != "Authorization" {
		t.Errorf("swaps %v", res.Swaps)
	}
}
