package router

import (
	"testing"
)

// The M2c shape: one wildcard rule per kind, plus exact rules that overlap it.
// dms is declared read-only and one agent widens it, which is the whole point
// of making read/write defaults instead of ceilings.
const wildcardRouting = `{
  "agents": [
    {"id": "ami-bot", "targets": ["ami"]},
    {"id": "watcher", "targets": ["dms"]},
    {"id": "both",    "targets": ["ami", "dms"]},
    {"id": "wide",    "targets": [{"alias": "dms", "write": true}]}
  ],
  "targets": [
    {"alias": "ami",  "id": "dm:333444555666777888", "name": "ami"},
    {"alias": "dms",  "id": "dm:*", "write": false},
    {"alias": "boss", "id": "channel:1112223334445556"}
  ]
}`

// A DM from Ami hits both the exact rule (ami) and the wildcard (dms). An agent
// holding both must be handed the message ONCE, through the most specific rule
// it owns -- and an agent holding only one of them must not be silenced by the
// other's existence, because specificity is computed per agent.
func TestWildcardFanOutDeduplicatesPerAgent(t *testing.T) {
	rt := newRouter(t, wildcardRouting)
	aud := rt.Audience("", "777888999000111222", true, []string{"333444555666777888"})
	if got, want := names(aud), "ami-bot@ami,both@ami,watcher@dms,wide@dms"; got != want {
		t.Fatalf("audience = %s\n             want %s", got, want)
	}
	// Guild traffic is untouched by the DM wildcard. Nobody in this file holds
	// boss, so the correct answer is "nobody" -- dm:* must not sweep up a guild
	// channel just because it is the only wildcard around.
	if got := names(rt.Audience("g1", "1112223334445556", false, nil)); got != "" {
		t.Fatalf("boss audience = %q, want nobody", got)
	}

	// The mirror case, same agent on both sides of the wildcard: it owns an
	// exact guild channel and dm:*, and each kind of message reaches it once
	// through the rule of the right kind.
	held := newRouter(t, `{
		  "agents": [{"id": "wide", "targets": ["boss", "dms"]}],
		  "targets": [
		    {"alias": "boss", "id": "channel:1112223334445556"},
		    {"alias": "dms",  "id": "dm:*", "write": false}
		  ]
		}`)
	if got := names(held.Audience("g1", "1112223334445556", false, nil)); got != "wide@boss" {
		t.Fatalf("owned guild channel = %q, want wide@boss", got)
	}
	if got := names(held.Audience("", "777888999000111222", true, []string{"555444333222111000"})); got != "wide@dms" {
		t.Fatalf("unowned DM = %q, want wide@dms", got)
	}
	// ...and an unlisted guild channel is not claimed by dm:* either.
	if got := names(held.Audience("g1", "444555666777888999", false, nil)); got != "" {
		t.Fatalf("stray guild channel = %q, want nobody (unknown_channel defaults to ignore)", got)
	}
}

func TestWildcardGrantsAndResolution(t *testing.T) {
	rt := newRouter(t, wildcardRouting)

	// An unlisted DM resolves through dm:*, so it cannot slip into an ad-hoc
	// rule and get written with rights the wildcard never granted.
	ru, ok := rt.Resolve("dm:777888999000111222")
	if !ok || ru.Alias != "dms" || !ru.Wildcard {
		t.Fatalf("unlisted DM should resolve via dm:*: %+v ok=%v", ru, ok)
	}
	if !rt.MayRead(Principal{AgentID: "watcher"}, ru) {
		t.Fatal("watcher holds dms and must be able to read it")
	}
	if rt.MayWrite(Principal{AgentID: "watcher"}, ru) {
		t.Fatal("dms defaults to read-only")
	}
	if !rt.MayWrite(Principal{AgentID: "wide"}, ru) {
		t.Fatal("wide widened dms and must be able to write")
	}
	if rt.MayWrite(Principal{AgentID: "both"}, ru) {
		t.Fatal("both inherits dms' read-only default")
	}
	if rt.MayWrite(Principal{AgentID: "ami-bot"}, ru) {
		t.Fatal("ami-bot does not hold dms at all")
	}
	exact, _ := rt.Resolve("ami")
	if !rt.MayWrite(Principal{AgentID: "ami-bot"}, exact) {
		t.Fatal("ami keeps its writable default")
	}

	// Sending through the wildcard alias needs a concrete id: "*" is not an
	// address, and a silent mis-send to Discord is worse than an error here.
	if _, reason := rt.SendTarget(Principal{AgentID: "wide"}, "dms"); reason != "wildcard_target" {
		t.Fatalf("wildcard alias without an id: %q", reason)
	}
	sent, reason := rt.SendTarget(Principal{AgentID: "wide"}, "dm:777888999000111222")
	if reason != "" || sent == nil || sent.Target != "dm:777888999000111222" {
		t.Fatalf("wide should send to an unlisted DM: %+v %q", sent, reason)
	}
	if _, reason := rt.SendTarget(Principal{AgentID: "ami-bot"}, "dms"); reason != "foreign_agent" {
		t.Fatalf("ami-bot does not hold dms: %q", reason)
	}
	if _, reason := rt.SendTarget(Principal{AgentID: "both"}, "dms"); reason != "write_denied" {
		t.Fatalf("both holds dms read-only: %q", reason)
	}
}

func TestAdmitInboundWildcardAndDeny(t *testing.T) {
	rt := newRouter(t, wildcardRouting)
	if _, code := rt.AdmitInbound("", "666555444333222111", true, []string{"555444333222111000"}); code != AdmitOK {
		t.Fatalf("dm:* should admit an unlisted peer, got %s", code)
	}
	if _, code := rt.AdmitInbound("g1", "444555666777888999", false, nil); code != AdmitNoRuleIgnor {
		t.Fatalf("an unlisted guild channel with no channel:* should still be ignored, got %s", code)
	}

	// deny is absolute on the concrete object, even when the rule that matched
	// is a wildcard that knows nothing about that id.
	denied := newRouter(t, wildcardRouting, "-deny-channels=dm:777888999000111222")
	if _, code := denied.AdmitInbound("", "777888999000111222", true, []string{"333444555666777888"}); code != AdmitDenied {
		t.Fatalf("deny of a concrete DM must beat dm:*, got %s", code)
	}
	if got := names(denied.Audience("", "777888999000111222", true, []string{"333444555666777888"})); got != "" {
		t.Fatalf("a denied DM must reach nobody, got %q", got)
	}

	// channel:* is guild channels only, so the two wildcards never overlap.
	all := newRouter(t, `{"agents":[{"id":"a1","targets":["all"]}],"targets":[{"alias":"all","id":"channel:*"}]}`)
	if ru, ok := all.Resolve("channel:444555666777888999"); !ok || ru.Alias != "all" {
		t.Fatalf("channel:* should match an unlisted guild channel: %+v %v", ru, ok)
	}
	if _, ok := all.Resolve("dm:444555666777888999"); ok {
		t.Fatal("channel:* must not swallow DMs")
	}
}

// What each agent is told about its own grants: the same target reads differently
// for two agents, so this is the one place the effective value has to show.
func TestTargetsShowsEffectiveGrants(t *testing.T) {
	rt := newRouter(t, wildcardRouting)
	for _, who := range []string{"wide", "watcher"} {
		seen := map[string]*Rule{}
		for _, ru := range rt.Targets(Principal{AgentID: who}) {
			seen[ru.Alias] = ru
		}
		dms, ok := seen["dms"]
		if !ok {
			t.Fatalf("%s should see dms, got %d targets", who, len(seen))
		}
		if dms.Write != (who == "wide") {
			t.Fatalf("%s sees dms write=%v, want %v", who, dms.Write, who == "wide")
		}
		if !dms.Read {
			t.Fatalf("%s should still read dms", who)
		}
	}
}
