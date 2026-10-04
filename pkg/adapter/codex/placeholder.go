package codex

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Placeholder renders what stands in for codex's credential behind an egress
// broker: for an API key, a key shaped like it, which the transport hands
// codex as it would the key; for a ChatGPT login, a login of placeholders
// (placeholderLogin), whose access token the broker swaps for the login's.
func (p Profile) Placeholder(kind string, credential, nonce []byte) (contract.PlaceholderResult, error) {
	route, _ := p.Describe().Egress.Route(kind)
	if kind == CredentialLogin {
		return placeholderLogin(credential, nonce, route)
	}
	return adapter.TokenResult(credential, nonce, route)
}

// caEnv hands codex the certificates a Runtime behind an egress broker asks
// its harness to trust, SSL_CERT_FILE, where codex reads them:
// CODEX_CA_CERTIFICATE, unless the environment names that itself.
func caEnv(env []string) []string {
	bundle := ""
	for _, kv := range env {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "CODEX_CA_CERTIFICATE":
			return env
		case "SSL_CERT_FILE":
			bundle = v
		}
	}
	if bundle == "" {
		return env
	}
	return append(env, "CODEX_CA_CERTIFICATE="+bundle)
}

// placeholderExp is when a placeholder login's tokens expire: never, as far as
// codex can tell, so it never tries to refresh one. The login's own access
// token is refreshed where its refresh token is, outside the harness.
const placeholderExp = 4102444800 // 2100-01-01

// authClaims are the login claims codex reads from its tokens: its plan and
// its account. A placeholder carries them, and no other claim of the login.
var authClaims = []string{"chatgpt_plan_type", "chatgpt_account_id", "chatgpt_user_id"}

const authClaim = "https://api.openai.com/auth"

// placeholderLogin renders a login of placeholders for a ChatGPT login
// (CredentialLogin): codex's auth.json with unsigned, JWT-shaped id and
// access tokens that carry the login's plan and account and expire in 2100,
// the login's account id, and a refresh token that refreshes nothing.
// codex's own checks take it, and every request codex sends carries the
// placeholder access token, which the broker swaps for the login's. The file
// is built afresh, never copied: no other field of the login reaches it.
func placeholderLogin(credential, nonce []byte, route contract.CredentialRoute) (contract.PlaceholderResult, error) {
	bad := func(msg string) (contract.PlaceholderResult, error) {
		// What the credential holds is a secret: no error repeats any of it.
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "credential", Message: msg}
	}
	if len(credential) > maxLogin {
		return bad("over the size of a codex auth.json")
	}
	var login struct {
		LastRefresh string `json:"last_refresh"`
		Tokens      struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(credential, &login) != nil {
		return bad("not a codex auth.json")
	}
	t := login.Tokens
	switch {
	case t.AccessToken == "":
		return bad("the login holds no access token")
	case t.RefreshToken != "" && t.RefreshToken != withheldRefresh:
		return bad("the login holds a refresh token: lend its access token alone")
	}
	access, ok := jwtClaims(t.AccessToken)
	if !ok {
		return bad("the login's access token is not a JWT")
	}
	claims := access
	if t.IDToken != "" {
		if claims, ok = jwtClaims(t.IDToken); !ok {
			return bad("the login's id token is not a JWT")
		}
	}
	auth := map[string]any{}
	if a, ok := claims[authClaim].(map[string]any); ok {
		for _, k := range authClaims {
			if v, ok := a[k]; ok {
				auth[k] = v
			}
		}
	}
	iat, _ := access["iat"].(float64)
	phAccess := placeholderJWT(nonce, "access", map[string]any{"exp": placeholderExp, "iat": int64(iat), authClaim: auth})
	id := map[string]any{"exp": placeholderExp, "iat": int64(iat), authClaim: auth}
	if email, ok := claims["email"].(string); ok {
		id["email"] = email
	}
	phID := placeholderJWT(nonce, "id", id)
	file, err := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": phID, "access_token": phAccess, "refresh_token": withheldRefresh, "account_id": t.AccountID,
		},
		"last_refresh": login.LastRefresh,
	})
	if err != nil {
		return contract.PlaceholderResult{}, err
	}
	return contract.PlaceholderResult{
		File:  file,
		Swaps: []contract.Swap{{Placeholder: phAccess, Secret: t.AccessToken, Hosts: route.Hosts, Headers: route.Headers}},
	}, nil
}

// jwtClaims are a JWT's claims, read without checking its signature: codex
// does not check them either.
func jwtClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil, false
	}
	return claims, true
}

// placeholderJWT is an unsigned token of JWT shape carrying claims, made
// unique by a value drawn from nonce for its use.
func placeholderJWT(nonce []byte, use string, claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	c := make(map[string]any, len(claims)+1)
	for k, v := range claims {
		c[k] = v
	}
	c["placeholder"] = adapter.Drawn(nonce, "codex "+use, 32)
	return enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + enc(c) + "." + adapter.Drawn(nonce, "codex signature "+use, 43)
}
