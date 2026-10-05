package contract

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// A Runtime may keep a harness's credentials out of the harness's reach
// (capability brokered_credentials). It stages a placeholder where the
// credential would be, fences the harness's network so that an egress broker
// is its only way out, and has the broker put the credential in the
// placeholder's place in the requests the harness sends where the credential
// belongs. The harness never holds the credential, and the broker never lets
// it reach another host.
//
// What only the adapter knows of that, it says: the hosts the harness
// reaches, where it presents each credential kind (Descriptor.Egress), and
// what stands in for a credential (Adapter.Placeholder). The Runtime gives
// the harness the broker's address in HTTPS_PROXY and the certificates it
// must trust in SSL_CERT_FILE, through HW_HARNESS_ENV; a profile hands those
// certificates to its harness however that harness reads them.

// MinNonceBytes is the least randomness a PlaceholderRequest carries.
const MinNonceBytes = 16

// MaxSwapBytes bounds a swap's placeholder and its secret.
const MaxSwapBytes = 16 << 10

// Egress is the network a harness reaches.
type Egress struct {
	// Hosts are the hosts every Session of the harness reaches, whatever its
	// credential: exact names, no wildcard, no port.
	Hosts []string `json:"hosts,omitempty"`
	// Credentials say where the harness presents each credential kind it
	// takes as a placeholder. A kind with no route is never brokered.
	Credentials []CredentialRoute `json:"credentials,omitempty"`
}

// CredentialRoute is where a harness presents a credential kind: the hosts it
// sends it to, which it needs to reach, and the request headers that carry it
// there. A broker injects the credential there and nowhere else.
type CredentialRoute struct {
	Kind    string   `json:"kind"`
	Hosts   []string `json:"hosts"`
	Headers []string `json:"headers"`
}

// Route is kind's route in e, and whether it has one.
func (e *Egress) Route(kind string) (CredentialRoute, bool) {
	if e == nil {
		return CredentialRoute{}, false
	}
	for _, r := range e.Credentials {
		if r.Kind == kind {
			return r, true
		}
	}
	return CredentialRoute{}, false
}

// PlaceholderRequest asks an adapter for a credential's placeholder.
type PlaceholderRequest struct {
	// Contract is the version the request is written in.
	Contract string `json:"contract"`
	// Kind is the credential's kind: one the Descriptor's egress routes.
	Kind string `json:"kind"`
	// Credential is the credential, as the Runtime would otherwise stage it.
	Credential []byte `json:"credential"`
	// Nonce is at least MinNonceBytes random bytes from the Runtime. The
	// placeholder is a function of the request: the same nonce gives the same
	// placeholder, and another nonce another.
	Nonce []byte `json:"nonce"`
}

// PlaceholderResult is what stands in for a credential. It holds the
// credential's secrets: the Runtime keeps it in memory, hands the secrets to
// its broker alone, and never journals or logs it.
type PlaceholderResult struct {
	// File is what the Runtime stages in the credential's place. Open reads
	// it as it reads the credential, and the harness then sends each swap's
	// placeholder where it would have sent the secret.
	File []byte `json:"file"`
	// Swaps are what the broker substitutes, and where.
	Swaps []Swap `json:"swaps"`
}

// Swap is one substitution: in the requests the harness sends to Hosts, the
// broker replaces Placeholder with Secret in Headers.
type Swap struct {
	// Placeholder is what the harness sends.
	Placeholder string `json:"placeholder"`
	// Secret is what the broker sends in its place.
	Secret string `json:"secret"`
	// Hosts and Headers are the route's, or a part of it.
	Hosts   []string `json:"hosts"`
	Headers []string `json:"headers"`
}

// String describes s without its secret.
func (s Swap) String() string {
	return fmt.Sprintf("swap{placeholder %d bytes, secret %d bytes, hosts %v, headers %v}", len(s.Placeholder), len(s.Secret), s.Hosts, s.Headers)
}

// GoString describes s without its secret.
func (s Swap) GoString() string { return s.String() }

// Validate checks r against route: at least one swap; each placeholder and
// secret printable, header-safe and bounded, every placeholder at least 16
// bytes and distinct; no placeholder in a secret, no secret in a
// placeholder or in the file; each swap's hosts and headers a part of the
// route's. Its errors never quote a secret.
func (r PlaceholderResult) Validate(route CredentialRoute) error {
	invalid := func(field, format string, args ...any) error {
		return &Error{Code: CodeProtocol, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	if len(r.File) == 0 {
		return invalid("file", "empty")
	}
	if len(r.Swaps) == 0 {
		return invalid("swaps", "none")
	}
	placeholders := map[string]bool{}
	for i, s := range r.Swaps {
		field := "swaps[" + strconv.Itoa(i) + "]"
		if err := headerValue(s.Placeholder); err != nil || len(s.Placeholder) < 16 {
			return invalid(field+".placeholder", "want 16 to %d printable bytes", MaxSwapBytes)
		}
		if err := headerValue(s.Secret); err != nil {
			return invalid(field+".secret", "%v", err)
		}
		if placeholders[s.Placeholder] {
			return invalid(field+".placeholder", "given twice")
		}
		placeholders[s.Placeholder] = true
		if len(s.Hosts) == 0 || !subset(s.Hosts, route.Hosts) {
			return invalid(field+".hosts", "%v, want a part of the route's %v", s.Hosts, route.Hosts)
		}
		if len(s.Headers) == 0 || !subsetFold(s.Headers, route.Headers) {
			return invalid(field+".headers", "%v, want a part of the route's %v", s.Headers, route.Headers)
		}
	}
	for i, s := range r.Swaps {
		field := "swaps[" + strconv.Itoa(i) + "]"
		if bytes.Contains(r.File, []byte(s.Secret)) {
			return invalid(field+".secret", "the file holds it")
		}
		for _, o := range r.Swaps {
			if strings.Contains(o.Placeholder, s.Secret) || strings.Contains(s.Secret, o.Placeholder) {
				return invalid(field+".secret", "a placeholder holds it, or it holds a placeholder")
			}
		}
	}
	return nil
}

// headerValue checks that v can be a header value's swapped part: 1 to
// MaxSwapBytes visible ASCII bytes.
func headerValue(v string) error {
	if v == "" || len(v) > MaxSwapBytes {
		return fmt.Errorf("want 1 to %d bytes, have %d", MaxSwapBytes, len(v))
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return fmt.Errorf("byte %d is not visible ASCII", i)
		}
	}
	return nil
}

// CheckEgress refuses a Descriptor whose egress is malformed: one without the
// capability, a host that is not an exact lower-case name, a route for a kind
// the harness does not take or given twice, a route with no host or header,
// or a header that is not a field name.
func CheckEgress(d Descriptor) error {
	invalid := func(field, format string, args ...any) error {
		return &Error{Code: CodeProtocol, Field: field, Message: fmt.Sprintf(format, args...)}
	}
	e := d.Egress
	switch {
	case e == nil && d.Has(CapBrokeredCredentials):
		return invalid("egress", "%s, and no egress", CapBrokeredCredentials)
	case e == nil:
		return nil
	case !d.Has(CapBrokeredCredentials):
		return invalid("egress", "declared without capability %s", CapBrokeredCredentials)
	}
	for i, h := range e.Hosts {
		if !ValidHost(h) {
			return invalid("egress.hosts["+strconv.Itoa(i)+"]", "%q is not an exact host name", h)
		}
	}
	kinds := map[string]bool{}
	for i, r := range e.Credentials {
		field := "egress.credentials[" + strconv.Itoa(i) + "]"
		switch {
		case !supports(d.CredentialKinds, r.Kind):
			return invalid(field+".kind", "%q is not a kind the harness takes", r.Kind)
		case kinds[r.Kind]:
			return invalid(field+".kind", "%q routed twice", r.Kind)
		case len(r.Hosts) == 0 || len(r.Headers) == 0:
			return invalid(field, "a route needs a host and a header")
		}
		kinds[r.Kind] = true
		for j, h := range r.Hosts {
			if !ValidHost(h) {
				return invalid(field+".hosts["+strconv.Itoa(j)+"]", "%q is not an exact host name", h)
			}
		}
		for j, h := range r.Headers {
			if !validFieldName(h) {
				return invalid(field+".headers["+strconv.Itoa(j)+"]", "%q is not a header name", h)
			}
		}
	}
	return nil
}

// ValidHost reports whether h is an exact host name: lower-case labels of
// letters, digits and inner hyphens, at least two of them, 253 bytes at most.
// No wildcard, no port, no address.
func ValidHost(h string) bool {
	if len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	// A name of digits alone in its last label is an address, not a host.
	last := labels[len(labels)-1]
	return strings.Trim(last, "0123456789") != ""
}

// validFieldName is an HTTP field name: a token (RFC 9110).
func validFieldName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

func subset(part, whole []string) bool {
	for _, p := range part {
		if !supports(whole, p) {
			return false
		}
	}
	return true
}

// subsetFold is subset for header names, which compare case-insensitively.
func subsetFold(part, whole []string) bool {
	for _, p := range part {
		found := false
		for _, w := range whole {
			found = found || strings.EqualFold(p, w)
		}
		if !found {
			return false
		}
	}
	return true
}
