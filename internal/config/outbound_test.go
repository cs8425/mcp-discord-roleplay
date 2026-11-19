package config

import (
	"strings"
	"testing"
)

// clearProxyEnv blanks both spellings of every variable the summary reads, so a
// developer's own shell (which may well have a proxy set) cannot leak into these
// expectations. t.Setenv restores each one after the test.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	} {
		t.Setenv(k, "")
	}
}

func TestOutboundProxySummaryAllUnset(t *testing.T) {
	clearProxyEnv(t)
	got, want := OutboundProxySummary(), "HTTPS_PROXY:-,HTTP_PROXY:-,ALL_PROXY:-,NO_PROXY:-"
	if got != want {
		t.Errorf("unset env: got %q, want %q", got, want)
	}
}

func TestOutboundProxySummarySchemes(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "socks5h://user:pass@127.0.0.1:1180")
	t.Setenv("HTTP_PROXY", "http://corp-proxy:3128")
	t.Setenv("ALL_PROXY", "127.0.0.1:1080") // bare host:port, no scheme
	t.Setenv("NO_PROXY", "localhost,127.0.0.1")

	got, want := OutboundProxySummary(),
		"HTTPS_PROXY:socks5h://,HTTP_PROXY:http://,ALL_PROXY:no-scheme"+goIgnores+",NO_PROXY:set"
	if got != want {
		t.Errorf("schemes: got %q, want %q", got, want)
	}
	// The whole point of the summary being a string of schemes and not the values:
	// a proxy URL carries a host, a port and often credentials, and this goes to a
	// log file.
	for _, leak := range []string{"127.0.0.1", "1180", "corp-proxy", "3128", "user:pass", "localhost"} {
		if strings.Contains(got, leak) {
			t.Errorf("summary leaks %q: %q", leak, got)
		}
	}
}

// ALL_PROXY is the one people expect to work because curl and docker read it. Go
// does not, so it has to stay visible and stay marked, even when nothing else is
// set — that is the case where it is most likely to be mistaken for a proxy.
func TestOutboundProxySummaryAllProxyIsMarkedDead(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1080")
	got, want := OutboundProxySummary(),
		"HTTPS_PROXY:-,HTTP_PROXY:-,ALL_PROXY:socks5://"+goIgnores+",NO_PROXY:-"
	if got != want {
		t.Errorf("ALL_PROXY alone: got %q, want %q", got, want)
	}
}

func TestOutboundProxySummaryLowercaseIsHonoured(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("https_proxy", "socks5://localhost:1080")
	got, want := OutboundProxySummary(), "HTTPS_PROXY:socks5://,HTTP_PROXY:-,ALL_PROXY:-,NO_PROXY:-"
	if got != want {
		t.Errorf("lowercase spelling: got %q, want %q", got, want)
	}
}

// Lowercase first is what httpproxy.FromEnvironment does, so the summary must
// report the value Go would actually use rather than the one a human wrote.
func TestOutboundProxySummaryLowercaseWinsLikeGo(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "socks5://a:1")
	t.Setenv("https_proxy", "http://b:2")
	got, want := OutboundProxySummary(), "HTTPS_PROXY:http://,HTTP_PROXY:-,ALL_PROXY:-,NO_PROXY:-"
	if got != want {
		t.Errorf("lowercase precedence: got %q, want %q", got, want)
	}
}

func TestProxySchemeForms(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "-"},
		{"socks5://x", "socks5://"},
		{"SOCKS5H://x", "socks5h://"},
		{"http://user:pass@h:1", "http://"},
		{"127.0.0.1:1080", "no-scheme"},
		{"://x", "no-scheme"},
		{"1http://x", "invalid"},
		{"my proxy://x", "invalid"},
	} {
		if got := proxyScheme(tc.in); got != tc.want {
			t.Errorf("proxyScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
