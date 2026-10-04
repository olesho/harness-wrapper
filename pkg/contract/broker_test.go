package contract

import (
	"fmt"
	"strings"
	"testing"
)

var route = CredentialRoute{Kind: "token", Hosts: []string{"api.example.com", "alt.example.com"}, Headers: []string{"Authorization"}}

func swapOf(placeholder, secret string) Swap {
	return Swap{Placeholder: placeholder, Secret: secret, Hosts: []string{"api.example.com"}, Headers: []string{"authorization"}}
}

// A placeholder result holds to its route and keeps every secret out of what
// the harness gets; what breaks a rule is refused without quoting a secret.
func TestPlaceholderResultValidate(t *testing.T) {
	const ph, secret = "tok-PLACEHOLDER0123456789", "tok-SECRETvalue0123456789"
	good := PlaceholderResult{File: []byte(ph), Swaps: []Swap{swapOf(ph, secret)}}
	if err := good.Validate(route); err != nil {
		t.Fatalf("a good result: %v", err)
	}
	bad := map[string]PlaceholderResult{
		"no file":             {Swaps: good.Swaps},
		"no swap":             {File: good.File},
		"short placeholder":   {File: []byte("short"), Swaps: []Swap{swapOf("short", secret)}},
		"empty secret":        {File: good.File, Swaps: []Swap{swapOf(ph, "")}},
		"secret with a space": {File: good.File, Swaps: []Swap{swapOf(ph, "tok SECRET")}},
		"secret over bound":   {File: good.File, Swaps: []Swap{swapOf(ph, strings.Repeat("s", MaxSwapBytes+1))}},
		"secret in the file":  {File: []byte(ph + "\n" + secret), Swaps: good.Swaps},
		"placeholder twice":   {File: good.File, Swaps: []Swap{swapOf(ph, secret), swapOf(ph, secret+"2")}},
		"secret in a placeholder": {File: good.File, Swaps: []Swap{
			swapOf(ph+secret, secret),
		}},
		"host off route":   {File: good.File, Swaps: []Swap{{Placeholder: ph, Secret: secret, Hosts: []string{"evil.example.com"}, Headers: []string{"Authorization"}}}},
		"header off route": {File: good.File, Swaps: []Swap{{Placeholder: ph, Secret: secret, Hosts: route.Hosts, Headers: []string{"X-Api-Key"}}}},
		"no host":          {File: good.File, Swaps: []Swap{{Placeholder: ph, Secret: secret, Headers: route.Headers}}},
	}
	for name, r := range bad {
		err := r.Validate(route)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the error quotes the secret: %v", name, err)
		}
	}
}

// A swap printed in a log names no secret.
func TestSwapHidesItsSecret(t *testing.T) {
	s := swapOf("tok-PLACEHOLDER0123456789", "tok-SECRETvalue0123456789")
	for _, out := range []string{s.String(), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprint([]Swap{s})} {
		if strings.Contains(out, "SECRET") || strings.Contains(out, "PLACEHOLDER") {
			t.Errorf("printed %q", out)
		}
	}
}

// An egress names exact hosts, routes only kinds the harness takes, each once,
// to a host by a header, and comes with its capability.
func TestCheckEgress(t *testing.T) {
	d := Descriptor{
		Capabilities:    []Capability{CapBrokeredCredentials},
		CredentialKinds: []string{"token", "login"},
		Egress: &Egress{
			Hosts:       []string{"api.example.com"},
			Credentials: []CredentialRoute{route, {Kind: "login", Hosts: []string{"chat.example.com"}, Headers: []string{"Authorization"}}},
		},
	}
	if err := CheckEgress(d); err != nil {
		t.Fatalf("a good egress: %v", err)
	}
	if r, ok := d.Egress.Route("login"); !ok || r.Hosts[0] != "chat.example.com" {
		t.Errorf("Route(login) = %+v, %v", r, ok)
	}
	if _, ok := d.Egress.Route("other"); ok {
		t.Error("Route(other) found one")
	}
	if _, ok := (*Egress)(nil).Route("token"); ok {
		t.Error("a nil egress routes")
	}
	if err := CheckEgress(Descriptor{}); err != nil {
		t.Errorf("no egress, no capability: %v", err)
	}
	broken := map[string]func(d *Descriptor){
		"no capability":   func(d *Descriptor) { d.Capabilities = nil },
		"capability only": func(d *Descriptor) { d.Egress = nil },
		"wildcard host":   func(d *Descriptor) { d.Egress.Hosts = []string{"*.example.com"} },
		"host with port":  func(d *Descriptor) { d.Egress.Hosts = []string{"api.example.com:443"} },
		"upper-case host": func(d *Descriptor) { d.Egress.Hosts = []string{"API.example.com"} },
		"untaken kind": func(d *Descriptor) {
			d.Egress.Credentials = append(d.Egress.Credentials, CredentialRoute{Kind: "other", Hosts: []string{"a.example.com"}, Headers: []string{"X"}})
		},
		"kind twice": func(d *Descriptor) { d.Egress.Credentials = append(d.Egress.Credentials, route) },
		"no header": func(d *Descriptor) {
			d.Egress.Credentials = []CredentialRoute{{Kind: "token", Hosts: []string{"api.example.com"}}}
		},
		"bad header": func(d *Descriptor) {
			d.Egress.Credentials = []CredentialRoute{{Kind: "token", Hosts: []string{"api.example.com"}, Headers: []string{"Bad Header"}}}
		},
		"route host is an address": func(d *Descriptor) {
			d.Egress.Credentials = []CredentialRoute{{Kind: "token", Hosts: []string{"10.0.0.1"}, Headers: []string{"Authorization"}}}
		},
	}
	for name, breakIt := range broken {
		b := d
		e := *d.Egress
		e.Credentials = append([]CredentialRoute(nil), d.Egress.Credentials...)
		b.Egress = &e
		breakIt(&b)
		if err := CheckEgress(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A host is an exact lower-case name of two labels or more, never a pattern,
// an address or a name with a port.
func TestValidHost(t *testing.T) {
	for _, h := range []string{"api.anthropic.com", "chatgpt.com", "a-b.c1.example", "x.y"} {
		if !ValidHost(h) {
			t.Errorf("%q refused", h)
		}
	}
	for _, h := range []string{"", "localhost", "*.example.com", "-a.example.com", "a-.example.com", "a..b", "1.2.3.4", "api.example.com:443", "Api.example.com", "a.b/c", strings.Repeat("a.", 127) + "com"} {
		if ValidHost(h) {
			t.Errorf("%q accepted", h)
		}
	}
}
