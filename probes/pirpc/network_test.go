package pirpc

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
)

// broker stands in for agentd's credential broker: an HTTPS proxy that
// answers CONNECT for any host, ends TLS there with a CA of its own, swaps
// each placeholder for its secret in a request's headers, and sends the
// request on over plain HTTP to the upstream it maps the host to; a host it
// maps to none gets 403. It records every host a client asked for, and the
// headers each request came with.
type broker struct {
	t        *testing.T
	ln       net.Listener
	caFile   string
	ca       *x509.Certificate
	caKey    *ecdsa.PrivateKey
	upstream map[string]string
	swaps    map[string]string

	mu    sync.Mutex
	certs map[string]*tls.Certificate
	hosts []string
	seen  []seen
}

// seen is one request the broker saw, before its swap.
type seen struct {
	Host, Method, Path string
	Header             http.Header
}

func startBroker(t *testing.T, upstream, swaps map[string]string) *broker {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pirpc probe broker CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &broker{t: t, ln: ln, caFile: caFile, ca: ca, caKey: key, upstream: upstream, swaps: swaps, certs: map[string]*tls.Certificate{}}
	srv := &http.Server{Handler: http.HandlerFunc(b.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return b
}

func (b *broker) URL() string { return "http://" + b.ln.Addr().String() }

// reset forgets what the broker saw.
func (b *broker) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hosts, b.seen = nil, nil
}

func (b *broker) record() ([]string, []seen) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.hosts...), append([]seen(nil), b.seen...)
}

// leaf is the broker's certificate for host, signed by its CA.
func (b *broker) leaf(host string) (*tls.Certificate, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.certs[host]; c != nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, b.ca, &key.PublicKey, b.caKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	b.certs[host] = c
	return c, nil
}

func (b *broker) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		b.mu.Lock()
		b.seen = append(b.seen, seen{Host: r.Host, Method: r.Method, Path: r.URL.String(), Header: r.Header.Clone()})
		b.mu.Unlock()
		http.Error(w, "the probe's broker only tunnels", http.StatusForbidden)
		return
	}
	host := r.URL.Hostname()
	if host == "" {
		host, _, _ = net.SplitHostPort(r.Host)
	}
	b.mu.Lock()
	b.hosts = append(b.hosts, host)
	b.mu.Unlock()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	c := tls.Server(conn, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return b.leaf(host) },
		NextProtos:     []string{"http/1.1"},
	})
	go b.tunnel(host, c)
}

// tunnel serves one CONNECT tunnel's requests, one after another.
func (b *broker) tunnel(host string, c *tls.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		b.mu.Lock()
		b.seen = append(b.seen, seen{Host: host, Method: req.Method, Path: req.URL.Path, Header: req.Header.Clone()})
		b.mu.Unlock()
		up, ok := b.upstream[host]
		if !ok {
			body := `{"error":{"message":"the probe's broker reaches no ` + host + `"}}`
			resp := &http.Response{
				StatusCode: http.StatusForbidden, ProtoMajor: 1, ProtoMinor: 1,
				Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: int64(len(body)),
				Body: io.NopCloser(strings.NewReader(body)),
			}
			_ = resp.Write(c)
			continue
		}
		for _, vs := range req.Header {
			for i, v := range vs {
				for ph, secret := range b.swaps {
					vs[i] = strings.ReplaceAll(v, ph, secret)
					v = vs[i]
				}
			}
		}
		out, err := http.NewRequest(req.Method, up+req.URL.RequestURI(), req.Body)
		if err != nil {
			return
		}
		out.Header = req.Header
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			_, _ = fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			continue
		}
		if err := resp.Write(c); err != nil {
			return
		}
	}
}

// carrying is the header of a request the broker saw for host that carries
// placeholder, and its value.
func carrying(reqs []seen, host, placeholder string) (string, string) {
	for _, r := range reqs {
		if r.Host != host {
			continue
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				if strings.Contains(v, placeholder) {
					return strings.ToLower(k), v
				}
			}
		}
	}
	return "", ""
}

// Placeholders the probe's broker swaps for the keys.
const (
	anthropicPlaceholder = "sk-ant-api03-PLACEHOLDER-0000000000000000000000"
	openaiPlaceholder    = "sk-proj-PLACEHOLDER-00000000000000000000000000"
	googlePlaceholder    = "AIzaPLACEHOLDER0000000000000000000000000"
)

// Behind a broker: pi reaches the providers' own hosts through HTTPS_PROXY,
// trusting the broker's CA from NODE_EXTRA_CA_CERTS, and sends each key in its
// API's header — x-api-key for Anthropic's, Authorization for OpenAI's,
// x-goog-api-key for Gemini's — where the broker swaps the placeholder.
// With PI_OFFLINE=1 it asks for no other host; without it, it also fetches
// its model catalog from pi.dev.
func TestBehindABroker(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	b := startBroker(
		t,
		map[string]string{"api.anthropic.com": mock.URL(), "api.openai.com": mock.URL()},
		map[string]string{anthropicPlaceholder: anthropicKey, openaiPlaceholder: openaiKey},
	)
	behind := func(t *testing.T, model string, env ...string) (*agent, *proc) {
		a := newAgent(t, nil, model)
		a.writeJSON("auth.json", map[string]any{
			"anthropic": map[string]any{"type": "api_key", "key": anthropicPlaceholder},
			"openai":    map[string]any{"type": "api_key", "key": openaiPlaceholder},
			"google":    map[string]any{"type": "api_key", "key": googlePlaceholder},
		})
		a.env = append(a.env, append([]string{"HTTPS_PROXY=" + b.URL()}, env...)...)
		return a, a.start()
	}

	for _, c := range []struct {
		name, model, ca, host, header, placeholder, auth string
	}{
		{"anthropic, NODE_EXTRA_CA_CERTS", anthropicModel, "NODE_EXTRA_CA_CERTS", "api.anthropic.com", "x-api-key", anthropicPlaceholder, anthropicKey},
		{"anthropic, SSL_CERT_FILE", anthropicModel, "SSL_CERT_FILE", "api.anthropic.com", "x-api-key", anthropicPlaceholder, anthropicKey},
		{"openai, NODE_EXTRA_CA_CERTS", openaiModel, "NODE_EXTRA_CA_CERTS", "api.openai.com", "authorization", openaiPlaceholder, "Bearer " + openaiKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			b.reset()
			_, p := behind(t, c.model, c.ca+"="+b.caFile)
			from := p.mark()
			r, ok := p.await(p.prompt("in-1", "PING 1"), 30*time.Second)
			if !ok || !r.Success {
				t.Fatalf("prompt: %+v", r)
			}
			p.mustWait("agent_settled", from, 90*time.Second)
			last := lastAssistant(p.since(from))
			hosts, reqs := b.record()
			header, value := carrying(reqs, c.host, c.placeholder)
			t.Logf("answer %q (stop %q, error %q); hosts %v; the key went in %s: %q", last.text(), last.StopReason, last.ErrorMessage, hosts, header, value)
			if last.text() != "PONG 1" {
				t.Errorf("no answer through the broker with %s", c.ca)
				return
			}
			if header != c.header {
				t.Errorf("the key went in %q, want %s", header, c.header)
			}
			if mr := mock.Requests(); mr[len(mr)-1].Auth != c.auth {
				t.Errorf("the model got %q, want the swapped key", mr[len(mr)-1].Auth)
			}
			for _, h := range hosts {
				if h != c.host {
					t.Errorf("offline, pi asked the broker for %s too", h)
				}
			}
		})
	}

	t.Run("gemini", func(t *testing.T) {
		b.reset()
		_, p := behind(t, "google/gemini-2.5-flash", "NODE_EXTRA_CA_CERTS="+b.caFile)
		p.run("in-1", "PING 1", 90*time.Second)
		_, reqs := b.record()
		header, _ := carrying(reqs, "generativelanguage.googleapis.com", googlePlaceholder)
		var paths []string
		for _, r := range reqs {
			paths = append(paths, r.Host+r.Path)
		}
		t.Logf("requests %v; the key went in %q", paths, header)
		if header != "x-goog-api-key" {
			t.Errorf("the key went in %q, want x-goog-api-key", header)
		}
	})

	t.Run("online", func(t *testing.T) {
		b.reset()
		_, p := behind(t, anthropicModel, "NODE_EXTRA_CA_CERTS="+b.caFile, "PI_OFFLINE=")
		p.run("in-1", "PING 1", 90*time.Second)
		time.Sleep(3 * time.Second)
		hosts, reqs := b.record()
		var paths []string
		for _, r := range reqs {
			paths = append(paths, r.Host+r.Path)
		}
		t.Logf("without PI_OFFLINE: hosts %v, requests %v", hosts, paths)
	})
}
