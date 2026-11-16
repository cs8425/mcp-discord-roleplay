// Audience is the fan-out key for the per-agent queues, so its tests are about
// who may READ: every rule that matches (not just the first), read:false
// exclusion, and the policy branch for events no rule owns.

package router

import (
	"path/filepath"
	"strings"
	"testing"

	"mcp-discord-roleplay/internal/config"
)

func openRouter(t *testing.T, extraArgs ...string) *Router {
	t.Helper()
	args := append([]string{"-selftest", "-no-file-root",
		"-config=" + filepath.Join(t.TempDir(), "absent.json")}, extraArgs...)
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

// names flattens an audience to "agent@alias" strings for readable assertions.
func names(aud []AudienceEntry) string {
	if len(aud) == 0 {
		return ""
	}
	out := make([]string, 0, len(aud))
	for _, e := range aud {
		a := e.Alias
		if a == "" {
			a = "-"
		}
		out = append(out, e.Agent+"@"+a)
	}
	return strings.Join(out, ",")
}

func TestAudienceSingleOwnerPerRule(t *testing.T) {
	rt := newRouter(t, scopedRouting)

	cases := []struct {
		why       string
		guild     string
		channel   string
		dm        bool
		recipient []string
		want      string
	}{
		{"boss channel", "g1", "1112223334445556", false, nil, "a1@boss"},
		{"side channel", "g1", "999888777666555444", false, nil, "a2@side"},
		{"dm by recipient", "", "777888999000111222", true,
			[]string{"333444555666777888"}, "a2@dm:333"},
		// sealed is read:false: matched but nobody may read it, and it must NOT
		// fall through to the stray branch (that would leak it to the default).
		{"sealed channel", "g1", "333444555666777888", false, nil, ""},
		// spectate is declared for no agent while agents[] scopes everything, so
		// it reaches nobody. This is the trap selftest warns about.
		{"unscoped target", "g1", "222333444555666777", false, nil, ""},
	}
	for _, c := range cases {
		if got := names(rt.Audience(c.guild, c.channel, c.dm, c.recipient)); got != c.want {
			t.Errorf("%s: audience = %q want %q", c.why, got, c.want)
		}
	}
}

// The single-value indexes (byChan/byUser) keep ONE rule per id, so a group DM
// whose recipients are owned by two different agents would reach exactly one of
// them through the index. Audience walks every rule instead.
func TestAudienceReturnsEveryMatchingRule(t *testing.T) {
	body := `{
	  "agents": [{"id": "a1", "targets": ["ami"]}, {"id": "a2", "targets": ["bo"]}],
	  "targets": [
	    {"alias": "ami", "id": "user:333444555666777888", "name": "ami"},
	    {"alias": "bo",  "id": "user:444555666777888999", "name": "bo"}
	  ]
	}`
	rt := newRouter(t, body)

	aud := rt.Audience("", "777888999000111222", true,
		[]string{"333444555666777888", "444555666777888999"})
	if got := names(aud); got != "a1@ami,a2@bo" {
		t.Fatalf("audience = %q want a1@ami,a2@bo", got)
	}
	// one recipient, one owner: no broadcast to the other persona
	if got := names(rt.Audience("", "777888999000111222", true,
		[]string{"444555666777888999"})); got != "a2@bo" {
		t.Fatalf("single recipient audience = %q want a2@bo", got)
	}
	// the index really does hold only one rule per id: that is the premise
	if got := rt.RuleForChannel("777888999000111222"); got != nil {
		t.Fatalf("premise: no channel rule should match, got %+v", got)
	}
}

func TestAudienceIsSorted(t *testing.T) {
	body := `{
	  "agents": [{"id": "zeta", "targets": ["c"]}, {"id": "alpha", "targets": ["c"]}],
	  "targets": [{"alias": "c", "id": "channel:1112223334445556"}]
	}`
	rt := newRouter(t, body)
	if got := names(rt.Audience("g1", "1112223334445556", false, nil)); got != "alpha@c,zeta@c" {
		t.Fatalf("audience must be sorted by agent id, got %q", got)
	}
}

// A target that names an agent owns it even without an agents[] block: the read
// scope must match the write scope, or send would refuse as foreign_agent while
// poll happily delivers that channel's messages to the default agent.
func TestAudienceTargetOwnedByAgent(t *testing.T) {
	body := `{"targets":[{"alias":"solo","id":"channel:1112223334445556","agent":"a9"}]}`
	rt := newRouter(t, body)
	if got := names(rt.Audience("g1", "1112223334445556", false, nil)); got != "a9@solo" {
		t.Fatalf("audience = %q want a9@solo", got)
	}
}

func TestAudienceDenyAndDMDisabled(t *testing.T) {
	rt := newRouter(t, scopedRouting, "-deny-channels=1112223334445556")
	if aud := rt.Audience("g1", "1112223334445556", false, nil); len(aud) != 0 {
		t.Fatalf("denied channel must reach nobody, got %q", names(aud))
	}

	rt = newRouter(t, scopedRouting, "-allow-dm=false")
	aud := rt.Audience("", "777888999000111222", true, []string{"333444555666777888"})
	if len(aud) != 0 {
		t.Fatalf("DMs disabled must reach nobody, got %q", names(aud))
	}
}

// An event no rule owns but policy admits goes to the default agent only, with
// an empty alias (there is no rule to name it by).
func TestAudienceStrayGuildChannelGoesToDefault(t *testing.T) {
	rt := newRouter(t, scopedRouting)
	aud := rt.Audience("g1", "444555666777888999", false, nil)
	if len(aud) != 1 {
		t.Fatalf("want exactly the default agent, got %q", names(aud))
	}
	if aud[0].Agent != "default" || aud[0].Alias != "" || aud[0].Target != "channel:444555666777888999" {
		t.Fatalf("stray audience = %+v", aud[0])
	}
}

func TestAudienceStrayHonoursAllowLists(t *testing.T) {
	rt := openRouter(t, "-allow-channels=1112223334445556")
	if got := names(rt.Audience("g1", "1112223334445556", false, nil)); got != "default@-" {
		t.Fatalf("allowlisted stray = %q want default@-", got)
	}
	if aud := rt.Audience("g1", "999999999888888777", false, nil); len(aud) != 0 {
		t.Fatalf("channel outside the allow-list must reach nobody, got %q", names(aud))
	}

	guildOnly := openRouter(t, "-allow-guilds=g2")
	if got := names(guildOnly.Audience("g2", "1112223334445556", false, nil)); got != "default@-" {
		t.Fatalf("allowlisted guild = %q want default@-", got)
	}
	if aud := guildOnly.Audience("g1", "1112223334445556", false, nil); len(aud) != 0 {
		t.Fatalf("other guild must reach nobody, got %q", names(aud))
	}
}

func TestAudienceUnknownDM(t *testing.T) {
	// ignore (the default): nobody rings for a stranger.
	rt := newRouter(t, scopedRouting)
	if aud := rt.Audience("", "777888999000111222", true, []string{"999999999888888777"}); len(aud) != 0 {
		t.Fatalf("unknown DM should be ignored, got %q", names(aud))
	}

	rt = newRouter(t, scopedRouting, "-unknown-dm=ring")
	aud := rt.Audience("", "777888999000111222", true, []string{"999999999888888777"})
	if len(aud) != 1 || aud[0].Agent != "default" {
		t.Fatalf("ring should wake the default agent, got %q", names(aud))
	}
}

// Without routing.json every declared agent queue question collapses to the
// process default, so a single-agent install keeps working unchanged.
func TestAudienceWithoutRouting(t *testing.T) {
	rt := openRouter(t)
	aud := rt.Audience("g1", "1112223334445556", false, nil)
	if got := names(aud); got != "default@-" {
		t.Fatalf("open mode audience = %q want default@-", got)
	}
	if got := rt.AgentIDs(); len(got) != 1 || got[0] != "default" {
		t.Fatalf("open mode AgentIDs = %v want [default]", got)
	}
}

func TestAgentIDsIncludesDeclaredAndDefault(t *testing.T) {
	rt := newRouter(t, scopedRouting)
	want := []string{"a1", "a2", "default"}
	got := rt.AgentIDs()
	if len(got) != len(want) {
		t.Fatalf("AgentIDs = %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AgentIDs = %v want %v", got, want)
		}
	}

	named := newRouter(t, scopedRouting, "-agent=primary")
	got = named.AgentIDs()
	if len(got) != 3 || got[0] != "a1" || got[1] != "a2" || got[2] != "primary" {
		t.Fatalf("AgentIDs with -agent=primary = %v", got)
	}
}

func TestAliasForTarget(t *testing.T) {
	rt := newRouter(t, scopedRouting)
	if got := rt.AliasForTarget(Principal{AgentID: "a1"}, "channel:1112223334445556"); got != "boss" {
		t.Errorf("a1 should know boss, got %q", got)
	}
	if got := rt.AliasForTarget(Principal{AgentID: "a2"}, "channel:1112223334445556"); got != "" {
		t.Errorf("a2 must not learn a1's alias, got %q", got)
	}
	if got := rt.AliasForTarget(Principal{AgentID: "a1"}, "user:333444555666777888"); got != "" {
		t.Errorf("a2's dm must stay unnamed for a1, got %q", got)
	}
	if got := rt.AliasForTarget(Principal{AgentID: "a1"}, "", "channel:333444555666777888"); got != "" {
		t.Errorf("read:false target must not be named, got %q", got)
	}
}
