package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// jwt is an unsigned token of JWT shape carrying claims.
func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".SIGNATURE"
}

var planClaims = map[string]any{
	"chatgpt_plan_type": "plus", "chatgpt_account_id": "acct-1", "chatgpt_user_id": "user-1", "secret_claim": "LOGIN-ONLY",
}

// login is a ChatGPT login's auth.json as a keeper lends it: no refresh token.
func login(t *testing.T, refresh string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": "sk-NOT-FOR-THE-WORKLOAD", "last_refresh": "2026-10-04T10:00:00Z",
		"tokens": map[string]any{
			"id_token":      jwt(map[string]any{"email": "a@example.com", "exp": 1, authClaim: planClaims}),
			"access_token":  jwt(map[string]any{"exp": 1, "iat": 1759572000, "scp": "LOGIN-ONLY", authClaim: planClaims}),
			"refresh_token": refresh, "account_id": "acct-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lookup(t *testing.T) contract.Adapter {
	t.Helper()
	a, ok := contract.Lookup(Name)
	if !ok {
		t.Fatal("not registered")
	}
	return a
}

var nonce = []byte("0123456789abcdef")

// A ChatGPT login's placeholder is a login codex takes — JWT-shaped tokens
// with its plan and account that expire in 2100, and a refresh token that
// refreshes nothing — and nothing more of the login: its access token is the
// broker's, swapped at chatgpt.com.
func TestPlaceholderLogin(t *testing.T) {
	if err := contract.CheckEgress(Profile{}.Describe()); err != nil {
		t.Fatal(err)
	}
	cred := login(t, "")
	res, err := lookup(t).Placeholder(contract.PlaceholderRequest{Contract: contract.Version, Kind: CredentialLogin, Credential: cred, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"LOGIN-ONLY", "sk-NOT-FOR-THE-WORKLOAD", "SIGNATURE"} {
		if strings.Contains(string(res.File), s) {
			t.Errorf("the placeholder login holds %q: %s", s, res.File)
		}
	}
	var got struct {
		AuthMode    string  `json:"auth_mode"`
		APIKey      *string `json:"OPENAI_API_KEY"`
		LastRefresh string  `json:"last_refresh"`
		Tokens      struct {
			ID      string `json:"id_token"`
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(res.File, &got); err != nil {
		t.Fatal(err)
	}
	if got.AuthMode != "chatgpt" || got.APIKey != nil || got.LastRefresh != "2026-10-04T10:00:00Z" ||
		got.Tokens.Refresh != withheldRefresh || got.Tokens.Account != "acct-1" {
		t.Errorf("placeholder login %s", res.File)
	}
	for name, tok := range map[string]string{"access": got.Tokens.Access, "id": got.Tokens.ID} {
		c, ok := jwtClaims(tok)
		if !ok {
			t.Fatalf("%s token %q is not JWT-shaped", name, tok)
		}
		auth, _ := c[authClaim].(map[string]any)
		if c["exp"] != float64(placeholderExp) || auth["chatgpt_plan_type"] != "plus" || auth["chatgpt_account_id"] != "acct-1" ||
			auth["chatgpt_user_id"] != "user-1" || auth["secret_claim"] != nil {
			t.Errorf("%s claims %v", name, c)
		}
	}
	if c, _ := jwtClaims(got.Tokens.ID); c["email"] != "a@example.com" {
		t.Errorf("id claims %v", c)
	}
	access := strings.Split(string(cred), `"access_token":"`)[1]
	access = access[:strings.IndexByte(access, '"')]
	if len(res.Swaps) != 1 || res.Swaps[0].Placeholder != got.Tokens.Access || res.Swaps[0].Secret != access ||
		strings.Join(res.Swaps[0].Hosts, ",") != ChatGPTHost {
		t.Errorf("swaps %v", res.Swaps)
	}

	// The transport writes it as it writes a lent login.
	dir := t.TempDir()
	staged := filepath.Join(dir, "login.json")
	if err := os.WriteFile(staged, res.File, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeLogin(staged, dir); err != nil {
		t.Errorf("writeLogin of the placeholder login: %v", err)
	}
}

// A login that holds a refresh token, no access token, or tokens that are no
// JWTs has no placeholder, and the refusal quotes none of it.
func TestPlaceholderLoginRefused(t *testing.T) {
	a := lookup(t)
	bad := map[string][]byte{
		"refresh token": login(t, "REFRESH-SECRET"),
		"no access":     []byte(`{"tokens":{"id_token":"x.y.z"}}`),
		"not json":      []byte("REFRESH-SECRET"),
		"opaque access": []byte(`{"tokens":{"access_token":"REFRESH-SECRET-opaque"}}`),
	}
	for name, cred := range bad {
		_, err := a.Placeholder(contract.PlaceholderRequest{Contract: contract.Version, Kind: CredentialLogin, Credential: cred, Nonce: nonce})
		var e *contract.Error
		if !errors.As(err, &e) || e.Code != contract.CodeInvalidSpec {
			t.Errorf("%s: %v, want invalid_spec", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "REFRESH-SECRET") {
			t.Errorf("%s: the error quotes the login: %v", name, err)
		}
	}
	if _, err := a.Placeholder(contract.PlaceholderRequest{Contract: contract.Version, Kind: CredentialAccessToken, Credential: []byte("tok"), Nonce: nonce}); err == nil {
		t.Error("a workspace access token, never tried behind a broker, has a placeholder")
	}
}

// An API key's placeholder is a key of its shape, swapped at the OpenAI API.
func TestPlaceholderAPIKey(t *testing.T) {
	key := "sk-proj-" + strings.Repeat("K", 64)
	res, err := lookup(t).Placeholder(contract.PlaceholderRequest{Contract: contract.Version, Kind: CredentialAPIKey, Credential: []byte(key), Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	if ph := string(res.File); !strings.HasPrefix(ph, "sk-proj-") || len(ph) != len(key) || ph == key ||
		res.Swaps[0].Secret != key || strings.Join(res.Swaps[0].Hosts, ",") != APIHost {
		t.Errorf("placeholder %q, swaps %v", ph, res.Swaps)
	}
}

// codex reads the certificates to trust from CODEX_CA_CERTIFICATE: the
// bundle a Runtime names in SSL_CERT_FILE goes there, unless the environment
// names one itself.
func TestCAEnv(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"PATH=/bin"}, []string{"PATH=/bin"}},
		{[]string{"SSL_CERT_FILE=/etc/ca.pem"}, []string{"SSL_CERT_FILE=/etc/ca.pem", "CODEX_CA_CERTIFICATE=/etc/ca.pem"}},
		{[]string{"SSL_CERT_FILE=/etc/ca.pem", "CODEX_CA_CERTIFICATE=/own.pem"}, []string{"SSL_CERT_FILE=/etc/ca.pem", "CODEX_CA_CERTIFICATE=/own.pem"}},
	}
	for _, c := range cases {
		if got := caEnv(c.in); strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("caEnv(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
