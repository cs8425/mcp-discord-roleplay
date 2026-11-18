package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRoutingFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "routing.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// M2c shape: read/write on a target are defaults every agent inherits, an agent
// may override them (tighten OR widen), the gate lists speak the same language
// as targets (alias or id), and unknown_channel replaces unknown_dm.
func TestLoadRoutingM2cGatesAndRefs(t *testing.T) {
	p := writeRoutingFile(t, `{
	  "agents": [
	    {"id": "scribe", "targets": ["boss", {"alias": "dms", "write": true}]},
	    {"id": "peek",   "targets": [{"alias": "boss", "write": false}]}
	  ],
	  "targets": [
	    {"alias": "boss", "id": "channel:1112223334445556"},
	    {"alias": "dms",  "id": "dm:*", "write": false}
	  ],
	  "allow_channels": ["boss", "dm:333444555666777888"],
	  "deny_channels":  ["dm:999888777666555444"],
	  "unknown_channel": "ignore"
	}`)
	r, err := LoadRouting(p)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if r.UnknownChannel != "ignore" {
		t.Fatalf("unknown_channel lost: %q", r.UnknownChannel)
	}
	if got := strings.Join(r.AllowChannels, ","); got != "boss,dm:333444555666777888" {
		t.Fatalf("allow_channels: %q", got)
	}

	agents := map[string]Agent{}
	for _, a := range r.Agents {
		agents[a.ID] = a
	}
	boss, dms := agents["scribe"].Targets[0], agents["scribe"].Targets[1]
	if boss.Alias != "boss" || boss.Read != nil || boss.Write != nil {
		t.Fatalf("a plain alias must stay a plain alias: %+v", boss)
	}
	if dms.Alias != "dms" || dms.Write == nil || !*dms.Write {
		t.Fatalf("scribe must widen dms to writable: %+v", dms)
	}
	if got := agents["peek"].Targets[0]; got.Alias != "boss" || got.Write == nil || *got.Write {
		t.Fatalf("peek must tighten boss to read-only: %+v", got)
	}

	// Round-tripping must not turn an inherited default into a written-out
	// object: the compact spelling is what users should keep seeing.
	b, err := json.Marshal(boss)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"boss"` {
		t.Fatalf("plain alias marshals as %s", b)
	}

	// A wildcard id is a legal target, and the retired "user:" prefix is not.
	if kind, id, err := NormalizeTargetID("dm:*"); err != nil || kind != TargetDM || id != WildcardID {
		t.Fatalf("dm:* must parse as dm/*, got %s/%s (%v)", kind, id, err)
	}
	if got := CanonicalTarget(TargetDM, WildcardID); got != "dm:*" {
		t.Fatalf("CanonicalTarget = %s", got)
	}
	if _, _, err := NormalizeTargetID("user:333444555666777888"); err == nil {
		t.Fatal("the user: prefix must be rejected")
	}
}

func TestLoadRoutingRejectsRenamedKeys(t *testing.T) {
	p := writeRoutingFile(t, `{"targets":[{"alias":"boss","id":"channel:1112223334445556"}],"unknown_dm":"ring"}`)
	_, err := LoadRouting(p)
	if err == nil || !strings.Contains(err.Error(), "unknown_channel") {
		t.Fatalf("unknown_dm must be reported together with its replacement, got %v", err)
	}
}

func TestLoadRoutingRejectsBadGatesAndRefs(t *testing.T) {
	cases := []string{
		// gate entries must be a declared alias or a parseable target
		`{"targets":[{"alias":"boss","id":"channel:1112223334445556"}],"allow_channels":["nope"]}`,
		`{"targets":[{"alias":"boss","id":"channel:1112223334445556"}],"deny_channels":["boss","channel:zz"]}`,
		`{"targets":[{"alias":"boss","id":"channel:1112223334445556"}],"unknown_channel":"queue"}`,
		// a Ref is {alias,read,write} -- nothing else, and never empty
		`{"targets":[{"alias":"boss","id":"channel:1112223334445556"}],"agents":[{"id":"a","targets":[{"alias":"boss","writ":true}]}]}`,
		`{"targets":[{"alias":"boss","id":"channel:1112223334445556"}],"agents":[{"id":"a","targets":[{"read":true}]}]}`,
		`{"targets":[{"alias":"boss","id":"channel:1112223334445556"}],"agents":[{"id":"a","targets":[""]}]}`,
	}
	for _, body := range cases {
		if _, err := LoadRouting(writeRoutingFile(t, body)); err == nil {
			t.Errorf("want an error for %s", body)
		}
	}
}

// allow ∩ deny is almost certainly a typo, but refusing to start over it is
// worse than saying so loudly: deny wins, and it wins over the read too.
func TestACLOverlapWarnsWithoutFailing(t *testing.T) {
	cfg, err := Load([]string{"-selftest", "-no-file-root",
		"-config=" + writeRoutingFile(t, `{"targets":[{"alias":"ami","id":"dm:333444555666777888"}],"allow_channels":["ami"]}`),
		"-deny-channels=dm:333444555666777888"})
	if err != nil {
		t.Fatalf("an overlap must not fail startup: %v", err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "deny wins") {
		t.Fatalf("want one overlap warning, got %v", cfg.Warnings)
	}

	// The same digits with a different kind are two different objects: a guild
	// channel and a DM are never the same target, so they must not warn.
	quiet, err := Load([]string{"-selftest", "-no-file-root",
		"-allow-channels=333444555666777888", "-deny-channels=dm:333444555666777888"})
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet.Warnings) != 0 {
		t.Fatalf("channel:333… vs dm:333… are different targets: %v", quiet.Warnings)
	}
}
