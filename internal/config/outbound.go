package config

import (
	"fmt"
	"os"
	"strings"
)

// Outbound proxy: there is deliberately no flag for it.
//
// All three things this server opens to the network — the gateway WebSocket, the
// REST calls and every media fetch — ride Go's defaults: http.DefaultTransport
// for the two HTTP paths, and gorilla's dialer for the socket, whose proxy comes
// from the same http.ProxyFromEnvironment. So the process environment is the
// switch, and we neither set nor override it here.
//
// What Go actually reads is narrower than the folklore, and this was measured
// here first (only HTTPS_PROXY moved traffic; ALL_PROXY did nothing) and then
// checked against golang.org/x/net/http/httpproxy, the code http.DefaultTransport
// uses for this:
//
//   - FromEnvironment takes http_proxy/HTTP_PROXY, https_proxy/HTTPS_PROXY and
//     no_proxy/NO_PROXY, and nothing else — no ALL_PROXY. When both spellings of
//     one exist the lowercase one wins (it calls getEnvAny("http_proxy",
//     "HTTP_PROXY")), which is why firstProxyEnv below checks lowercase first.
//   - proxyForURL answers only for scheme "https" (HTTPS_PROXY) and "http"
//     (HTTP_PROXY), and returns nil for anything else. Every path we open is
//     https — the WebSocket is normalised from wss to https before Go is asked —
//     so HTTPS_PROXY is the single variable that moves this server.
//   - A value may be a full URL or a bare "host[:port]", and then Go assumes
//     http. A SOCKS proxy must therefore be written with its scheme; "no-scheme"
//     in the summary is that mistake being named instead of silently becoming an
//     http proxy.
//
// The one cost of having no flag was that nothing in the log said whether
// outbound traffic is proxied at all, and the question only gets interesting
// when a failure mentions a proxy nobody remembers setting.
// OutboundProxySummary closes that on the startup line. It prints the scheme
// prefix and nothing else — never a host, a port or a credential, because proxy
// URLs routinely look like socks5://user:pass@host:1080 and this line is written
// into a log file.
func OutboundProxySummary() string {
	noProxy := "-"
	if firstProxyEnv("NO_PROXY") != "" {
		noProxy = "set"
	}
	all := proxyScheme(firstProxyEnv("ALL_PROXY"))
	if all != "-" {
		// Marked, because it is set but dead: better an odd-looking log line than
		// an operator believing traffic is proxied when nothing reads the name.
		all += goIgnores
	}
	return fmt.Sprintf("HTTPS_PROXY:%s,HTTP_PROXY:%s,ALL_PROXY:%s,NO_PROXY:%s",
		proxyScheme(firstProxyEnv("HTTPS_PROXY")),
		proxyScheme(firstProxyEnv("HTTP_PROXY")),
		all,
		noProxy)
}

// goIgnores labels a variable Go's proxy code never reads.
const goIgnores = "(go-ignored)"

// firstProxyEnv reads name the way Go does: lowercase first, then uppercase.
// The order is not cosmetic — when a shell exports both with different values,
// Go uses the lowercase one, and a summary that preferred the other would name a
// proxy that is not in the path. The uppercase spelling is what gets printed
// either way, because that is the name people configure.
//
// .env is applied by LoadDotEnv with os.Setenv (without overriding a variable
// that already exists), so a proxy written in .env reaches the environment
// before anything dials out — which is why this may only be called after
// config.Load.
func firstProxyEnv(name string) string {
	if v := strings.TrimSpace(os.Getenv(strings.ToLower(name))); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(name))
}

// proxyScheme reduces a proxy URL to its scheme prefix ("socks5h://", "http://").
// "-" means unset, "no-scheme" a bare host:port (Go assumes http — see above),
// and "invalid" something that cannot form a scheme at all.
func proxyScheme(v string) string {
	if v == "" {
		return "-"
	}
	i := strings.Index(v, "://")
	if i <= 0 {
		return "no-scheme"
	}
	scheme := strings.ToLower(v[:i])
	if scheme[0] < 'a' || scheme[0] > 'z' {
		return "invalid"
	}
	for n := 1; n < len(scheme); n++ {
		r := scheme[n]
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.') {
			return "invalid"
		}
	}
	return scheme + "://"
}
