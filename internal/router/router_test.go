package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcp-discord-roleplay/internal/config"
)

func writeRouting(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "routing.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newRouter(t *testing.T, body string, extraArgs ...string) *Router {
	t.Helper()
	args := append([]string{"-selftest", "-no-file-root", "-config=" + writeRouting(t, body)}, extraArgs...)
	cfg, err := config.Load(args)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	rt, err := New(cfg)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return rt
}

const scopedRouting = `{
  "agents": [
    {"id": "a1", "targets": ["boss"]},
    {"id": "a2", "targets": ["side", "ami"]}
  ],
  "targets": [
    {"alias": "boss", "id": "channel:1112223334445556", "name": "#rp-main"},
    {"alias": "ami", "id": "dm:333444555666777888", "name": "ami"},
    {"alias": "side", "id": "channel:999888777666555444"},
    {"alias": "spectate", "id": "channel:222333444555666777", "write": false},
    {"alias": "sealed", "id": "channel:333444555666777888", "read": false, "write": false}
  ]
}`

func TestResolveForms(t *testing.T) {
	rt := newRouter(t, scopedRouting)

	cases := []struct {
		ref    string
		alias  string
		target string
	}{
		{"boss", "boss", "channel:1112223334445556"},
		{"BOSS", "boss", "channel:1112223334445556"},
		{"channel:1112223334445556", "boss", "channel:1112223334445556"},
		{"1112223334445556", "boss", "channel:1112223334445556"},
		{"#rp-main", "boss", "channel:1112223334445556"},
		{"rp-main", "boss", "channel:1112223334445556"},
		{"ami", "ami", "dm:333444555666777888"},
		{"dm:333444555666777888", "ami", "dm:333444555666777888"},
	}
	for _, c := range cases {
		ru, ok := rt.Resolve(c.ref)
		if !ok {
			t.Errorf("Resolve(%q) not found", c.ref)
			continue
		}
		if ru.Alias != c.alias || ru.Target != c.target {
			t.Errorf("Resolve(%q) = %s/%s want %s/%s", c.ref, ru.Alias, ru.Target, c.alias, c.target)
		}
	}

	if _, ok := rt.Resolve(""); ok {
		t.Error("empty ref must not resolve")
	}
	if _, ok := rt.Resolve("nonsense"); ok {
		t.Error("unknown ref must not resolve")
	}
	// routing.json exists → ad-hoc ids are refused
	if _, ok := rt.Resolve("channel:555555555555555555"); ok {
		t.Error("unlisted channel id must not resolve when routing is declared")
	}
	ru, ok := rt.Resolve("me")
	if !ok || ru.Kind != KindMe {
		t.Error("me must resolve to the self target")
	}
}

func TestAgentScopingAndWriteACL(t *testing.T) {
	rt := newRouter(t, scopedRouting)
	a1 := Principal{AgentID: "a1"}
	a2 := Principal{AgentID: "a2"}

	if _, reason := rt.SendTarget(a1, "boss"); reason != "" {
		t.Errorf("a1 may write boss, got %q", reason)
	}
	if _, reason := rt.SendTarget(a1, "side"); reason != "foreign_agent" {
		t.Errorf("a1 must not write side, got %q", reason)
	}
	if _, reason := rt.SendTarget(a2, "side"); reason != "" {
		t.Errorf("a2 may write side, got %q", reason)
	}
	// spectate is declared for everybody but read-only
	if _, reason := rt.SendTarget(Principal{AgentID: "nobody"}, "spectate"); reason != "foreign_agent" {
		t.Errorf("unlisted agent must be scoped out, got %q", reason)
	}
	if _, reason := rt.SendTarget(a2, "ami"); reason != "" {
		t.Errorf("a2 may write ami, got %q", reason)
	}
}

func TestWriteDeniedAndUnknown(t *testing.T) {
	// no agents block → every target visible, write:false still enforced
	body := `{"targets":[{"alias":"spectate","id":"channel:222333444555666777","write":false}]}`
	rt := newRouter(t, body)
	p := Principal{AgentID: "default"}
	if _, reason := rt.SendTarget(p, "spectate"); reason != "write_denied" {
		t.Fatalf("want write_denied, got %q", reason)
	}
	if _, reason := rt.SendTarget(p, ""); reason != "empty_target" {
		t.Fatalf("want empty_target, got %q", reason)
	}
	if _, reason := rt.SendTarget(p, "ghost"); reason != "unknown_target" {
		t.Fatalf("want unknown_target, got %q", reason)
	}
	if _, reason := rt.SendTarget(p, "me"); reason != "" {
		t.Fatalf("me is always writable, got %q", reason)
	}
}

func TestTargetsVisibility(t *testing.T) {
	rt := newRouter(t, scopedRouting)
	if got := len(rt.Targets(Principal{AgentID: "a1"})); got != 1 {
		t.Fatalf("a1 should see exactly 1 target, saw %d", got)
	}
	if got := len(rt.Targets(Principal{AgentID: "a2"})); got != 2 {
		t.Fatalf("a2 should see 2 targets, saw %d", got)
	}
	unscoped := newRouter(t, `{"targets":[{"alias":"boss","id":"channel:1112223334445556"}]}`)
	if got := len(unscoped.Targets(Principal{AgentID: "whoever"})); got != 1 {
		t.Fatalf("without an agents block every target is visible, saw %d", got)
	}
}

func TestAdmitInbound(t *testing.T) {
	rt := newRouter(t, scopedRouting)

	// declared channel
	if _, reason := rt.AdmitInbound("g1", "1112223334445556", false, nil); reason != AdmitOK {
		t.Errorf("boss channel should be admitted, got %q", reason)
	}
	// read:false channel
	if _, reason := rt.AdmitInbound("g1", "333444555666777888", false, nil); reason != AdmitReadDenied {
		t.Errorf("sealed should be read_denied, got %q", reason)
	}
	// DM matching a rule by recipient
	if ru, reason := rt.AdmitInbound("", "777888999000111222", true, []string{"333444555666777888"}); reason != AdmitOK || ru == nil || ru.Alias != "ami" {
		t.Errorf("DM by recipient should match ami, got %v/%q", ru, reason)
	}
	// unknown DM with the default ignore policy
	if _, reason := rt.AdmitInbound("", "777888999000111222", true, []string{"999999999888888777"}); reason != AdmitNoRuleIgnor {
		t.Errorf("unknown DM should be ignored, got %q", reason)
	}
	// unlisted guild channel: M2c made it behave exactly like a DM, so the
	// unknown_channel policy (default ignore) answers, not open mode.
	if _, reason := rt.AdmitInbound("g1", "444555666777888999", false, nil); reason != AdmitNoRuleIgnor {
		t.Errorf("unlisted guild channel should be ignored, got %q", reason)
	}
}

func TestAdmitInboundUnknownChannelRing(t *testing.T) {
	rt := newRouter(t, scopedRouting, "-unknown-channel=ring")
	ru, reason := rt.AdmitInbound("", "777888999000111222", true, []string{"999999999888888777"})
	if reason != AdmitOK || ru != nil {
		t.Fatalf("ring mode should admit with no rule, got %v/%q", ru, reason)
	}
	if _, reason := rt.AdmitInbound("g1", "444555666777888999", false, nil); reason != AdmitOK {
		t.Fatalf("ring mode admits an unlisted guild channel too, got %q", reason)
	}
}

func TestAdmitInboundDenyAndAllowLists(t *testing.T) {
	rt := newRouter(t, `{"targets":[{"alias":"boss","id":"channel:1112223334445556"}]}`,
		"-deny-channels=999999999888888777")
	if _, reason := rt.AdmitInbound("g1", "999999999888888777", false, nil); reason != AdmitDenied {
		t.Fatalf("deny must win, got %q", reason)
	}

	// No routing file at all: the allow-lists become the ACL.
	dir := t.TempDir()
	cfg, err := config.Load([]string{"-selftest", "-no-file-root", "-config=" + filepath.Join(dir, "absent.json"),
		"-allow-channels=1112223334445556", "-deny-channels=222333444555666777"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := plain.AdmitInbound("g1", "1112223334445556", false, nil); reason != AdmitOK {
		t.Fatalf("allowlisted channel should be admitted, got %q", reason)
	}
	if _, reason := plain.AdmitInbound("g1", "222333444555666777", false, nil); reason != AdmitDenied {
		t.Fatalf("denied channel, got %q", reason)
	}
	if _, reason := plain.AdmitInbound("g1", "333444555666777888", false, nil); reason != AdmitNotListed {
		t.Fatalf("channel outside the allow-list, got %q", reason)
	}
	if _, reason := plain.SendTarget(Principal{AgentID: "default"}, "channel:1112223334445556"); reason != "" {
		t.Fatalf("ad-hoc target should be writable in open mode, got %q", reason)
	}
	if _, reason := plain.SendTarget(Principal{AgentID: "default"}, "channel:333444555666777888"); reason != "not_allowlisted" {
		t.Fatalf("ad-hoc outside allow-list, got %q", reason)
	}
}

// TestDenyDMWildcard is the replacement for --allow-dm=false: one wildcard in
// the deny list closes DMs, and only them.
func TestDenyDMWildcard(t *testing.T) {
	rt := newRouter(t, scopedRouting, "-deny-channels=dm:*")
	if _, reason := rt.AdmitInbound("", "777888999000111222", true, []string{"333444555666777888"}); reason != AdmitDenied {
		t.Fatalf("dm:* in deny must close the DM even when a rule owns it, got %q", reason)
	}
	if _, reason := rt.AdmitInbound("g1", "1112223334445556", false, nil); reason != AdmitOK {
		t.Fatalf("a guild channel is not a DM, got %q", reason)
	}
	if _, reason := rt.SendTarget(Principal{AgentID: "a2"}, "ami"); reason != "denied_channel" {
		t.Fatalf("deny blocks sending too, got %q", reason)
	}
}

func TestDeclaresAgent(t *testing.T) {
	rt := newRouter(t, scopedRouting) // declares a1 and a2
	if !rt.DeclaresAgent("a1") {
		t.Error("a1 is declared")
	}
	if rt.DeclaresAgent("default") {
		t.Error("an agent missing from routing.json must be reported (selftest fails on it)")
	}
	open := newRouter(t, `{"targets":[{"alias":"boss","id":"channel:1112223334445556"}]}`)
	if !open.DeclaresAgent("anyone") {
		t.Error("without an agents block nothing is scoped, so any agent is fine")
	}
}

func TestPrincipalRoundTrip(t *testing.T) {
	want := Principal{AgentID: "a7"}
	ctx := WithPrincipal(context.Background(), want)
	if got := FromContext(ctx); got != want {
		t.Fatalf("principal = %+v want %+v", got, want)
	}
	if got := FromContext(context.Background()); got.AgentID != "" {
		t.Fatalf("empty ctx should give zero principal, got %+v", got)
	}
}
