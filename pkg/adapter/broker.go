package adapter

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Placeholderer is the part of a Profile that renders placeholders for its
// harness's credentials (capability brokered_credentials).
type Placeholderer interface {
	// Placeholder renders the placeholder of credential, a credential of
	// kind, as a function of nonce. A malformed credential fails with
	// CodeInvalidSpec on field credential, and the error repeats none of it.
	Placeholder(kind string, credential, nonce []byte) (contract.PlaceholderResult, error)
}

// ModelPlaceholderer is a Placeholderer whose placeholders follow the agent's
// model (contract 1.5): a credential kind that serves several providers, each
// swap narrowed to the provider the model names. A model it cannot place
// fails with CodeInvalidSpec on field model.
type ModelPlaceholderer interface {
	PlaceholderFor(kind, model string, credential, nonce []byte) (contract.PlaceholderResult, error)
}

// Placeholder renders what stands in for a credential: the request checked
// against the Descriptor, the profile's rendering — with the agent's model,
// for a profile that reads it — and the result checked against the kind's
// route.
func (a *harnessAdapter) Placeholder(req contract.PlaceholderRequest) (contract.PlaceholderResult, error) {
	if err := checkVersion(req.Contract); err != nil {
		return contract.PlaceholderResult{}, err
	}
	if !a.desc.Has(contract.CapBrokeredCredentials) {
		return contract.PlaceholderResult{}, contract.Errorf(contract.CodeUnsupported, "%s keeps no credential behind a broker", a.desc.Harness.Name)
	}
	route, ok := a.desc.Egress.Route(req.Kind)
	if !ok {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeUnsupported, Field: "kind", Message: "no route for " + req.Kind}
	}
	if len(req.Nonce) < contract.MinNonceBytes {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "nonce", Message: "fewer than the least random bytes"}
	}
	if len(req.Credential) == 0 {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "credential", Message: "empty"}
	}
	var res contract.PlaceholderResult
	var err error
	switch p := a.p.(type) {
	case ModelPlaceholderer:
		res, err = p.PlaceholderFor(req.Kind, req.Model, req.Credential, req.Nonce)
	case Placeholderer:
		res, err = p.Placeholder(req.Kind, req.Credential, req.Nonce)
	default:
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInternal, Message: "the profile declares " + string(contract.CapBrokeredCredentials) + " and renders no placeholder"}
	}
	if err != nil {
		return contract.PlaceholderResult{}, err
	}
	if err := res.Validate(route); err != nil {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInternal, Message: "the profile rendered an invalid placeholder: " + err.Error()}
	}
	return res, nil
}

// TokenResult is the placeholder of a token credential — one line, sent as it
// is — presented in route: the placeholder token is the file, and the one swap
// covers the whole route.
func TokenResult(credential, nonce []byte, route contract.CredentialRoute) (contract.PlaceholderResult, error) {
	tok := strings.TrimSpace(string(credential))
	if tok == "" || strings.ContainsAny(tok, "\n\r\x00") {
		return contract.PlaceholderResult{}, &contract.Error{Code: contract.CodeInvalidSpec, Field: "credential", Message: "not a token: empty, or more than one line"}
	}
	ph := TokenPlaceholder(tok, nonce)
	return contract.PlaceholderResult{
		File:  []byte(ph),
		Swaps: []contract.Swap{{Placeholder: ph, Secret: tok, Hosts: route.Hosts, Headers: route.Headers}},
	}, nil
}

// TokenPlaceholder is a placeholder shaped like token: token's lower-case
// prefix kept — "sk-ant-oat01-", "sk-proj-": the leading run of lower-case
// letters, digits and hyphens up to its last hyphen, within its first 16
// bytes — and the rest, as long as token's rest and at least 32 bytes, letters
// and digits drawn from nonce. A harness that tells credentials apart by their
// prefix takes it as the token it stands for, and the same token and nonce
// always give the same placeholder.
func TokenPlaceholder(token string, nonce []byte) string {
	prefix := tokenPrefix(token)
	n := max(len(token)-len(prefix), 32)
	return prefix + Drawn(nonce, "token", n)
}

func tokenPrefix(token string) string {
	run := 0
	for run < len(token) && run < 16 {
		c := token[run]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			break
		}
		run++
	}
	if i := strings.LastIndexByte(token[:run], '-'); i > 0 {
		return token[:i+1]
	}
	return ""
}

// Drawn is n letters and digits drawn from nonce for use: the same nonce and
// use always give the same ones, and another use others.
func Drawn(nonce []byte, use string, n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	var block [8]byte
	for i := uint64(0); b.Len() < n; i++ {
		binary.BigEndian.PutUint64(block[:], i)
		m := hmac.New(sha256.New, nonce)
		m.Write([]byte("harness-wrapper placeholder\x00" + use + "\x00"))
		m.Write(block[:])
		for _, x := range m.Sum(nil) {
			// 248 = 4·62: drawing only below it keeps every character equally likely.
			if x < 248 && b.Len() < n {
				b.WriteByte(alphabet[x%62])
			}
		}
	}
	return b.String()
}

// Keeping is the part of a Profile that keeps its harness's subscription
// login for a runtime (capability login_keeper).
type Keeping interface {
	// Keep opens a keeper of the login under req.Home.
	Keep(req contract.KeeperRequest) (contract.Keeper, error)
}

// Keep opens the profile's keeper, the request checked against the
// Descriptor first.
func (a *harnessAdapter) Keep(req contract.KeeperRequest) (contract.Keeper, error) {
	if err := checkVersion(req.Contract); err != nil {
		return nil, err
	}
	if !a.desc.Has(contract.CapLoginKeeper) {
		return nil, contract.Errorf(contract.CodeUnsupported, "%s keeps no login for a runtime", a.desc.Harness.Name)
	}
	for field, p := range map[string]string{"home": req.Home, "harness_root": req.HarnessRoot} {
		if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, &contract.Error{Code: contract.CodeProtocol, Field: field, Message: "not a clean absolute path"}
		}
	}
	k, ok := a.p.(Keeping)
	if !ok {
		return nil, &contract.Error{Code: contract.CodeInternal, Message: "the profile declares " + string(contract.CapLoginKeeper) + " and keeps no login"}
	}
	return k.Keep(req)
}
