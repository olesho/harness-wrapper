package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// v1 has no auth, and POST /v1/turns and /v1/conversations run whatever
// binary_path, args, env and working_dir the caller sends. So the only
// boundary is who can reach the server, and binding to loopback alone does
// not hold it: a web page in a browser on the same machine can reach
// 127.0.0.1 too. Two ways in, each closed here:
//
//   - Drive-by: a page sends a "simple" cross-site POST (text/plain,
//     mode no-cors). The browser sends it without a preflight and chatd
//     decodes the body as JSON whatever its Content-Type. The page cannot
//     read the reply, but the harness has already been launched. Browsers
//     mark such a request with Origin and Sec-Fetch-Site; the reference
//     clients, curl and Node's fetch send neither, and chatd sets no CORS
//     headers, so no browser page can use the API anyway.
//   - DNS rebinding: an attacker's name re-resolves to 127.0.0.1, so the
//     page counts as same-origin, may send no Origin, and can read replies.
//     Its requests still carry the attacker's name in Host, so a loopback
//     server answers only Host values that are loopback addresses or
//     localhost.
//
// --allow-remote drops the Host check (a remote caller may use any name for
// the machine) and the loopback-bind check, never the browser check.

// requireLoopbackBind refuses a --bind address that is not loopback: an
// empty host or 0.0.0.0 listens on every interface.
func requireLoopbackBind(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --bind %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("--bind %q is not a loopback address: v1 has no auth, so bind to 127.0.0.1, [::1] or localhost, or pass --allow-remote", addr)
	}
	return nil
}

// isLoopbackHost reports whether host, without a port, names this machine
// only: localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localOnly wraps next with the checks above. It refuses before any handler
// reads the body, so a refused request never launches anything.
func localOnly(next http.Handler, allowRemote bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromBrowser(r) {
			writeError(w, http.StatusForbidden, "browser_refused",
				"harness-chatd does not serve browsers: the request carries Origin or a cross-site Sec-Fetch-Site")
			return
		}
		if !allowRemote && !isLoopbackHost(requestHost(r)) {
			writeError(w, http.StatusForbidden, "host_refused",
				fmt.Sprintf("Host %q is not localhost or a loopback address", r.Host))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fromBrowser reports whether a browser sent r on behalf of a page. A
// Sec-Fetch-Site of "none" is the user's own navigation (a URL typed into
// the address bar), which no page can trigger with a body.
func fromBrowser(r *http.Request) bool {
	if len(r.Header.Values("Origin")) > 0 {
		return true
	}
	site := r.Header.Values("Sec-Fetch-Site")
	return len(site) > 0 && site[0] != "none"
}

// requestHost is r.Host without its port.
func requestHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return r.Host
}
