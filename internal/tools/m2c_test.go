package tools

import (
	"context"
	"testing"
	"time"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/router"
)

// One DM rule that covers every peer, read-only by default: "default" widens it
// to writable, "nosy" is not even allowed to look. This is the shape M2c exists
// for -- "most agents read, exactly one speaks" -- so the tool layer has to
// honour it in both directions.
const m2cRouting = `{
  "agents": [
    {"id": "default", "targets": ["boss", {"alias": "dms", "write": true}]},
    {"id": "nosy", "targets": [{"alias": "dms", "read": false}]}
  ],
  "targets": [
    {"alias": "boss", "id": "channel:111222333444555666", "name": "#rp-main"},
    {"alias": "dms", "id": "dm:*", "write": false}
  ]
}`

func m2cDM(id, peer, text string) *bus.Event {
	return &bus.Event{
		Kind:      bus.KindMessage,
		Ts:        time.Now(),
		Time:      "2026-01-01T00:00:00Z",
		Target:    "channel:999000111222333444",
		ChannelID: "999000111222333444",
		DM:        true,
		MessageID: id,
		Author:    bus.Author{ID: peer, Name: "ami", Tag: "ami#0001"},
		Text:      text,
		Extra:     map[string]string{"peer": peer},
	}
}

// A wildcard filter matches by KIND: no event is ever addressed to a channel
// called "*", so "dm:*" has to be read as "any DM".
func TestPollWildcardMatchesByKind(t *testing.T) {
	d := newDeps(t, pollRoutingCfg(t, m2cRouting))
	connected(d)
	push(t, d, "default", evMsg("m1", "hello"), m2cDM("d1", "555444333222111000", "in dm"))

	out := pollOK(t, d, `{"targets":["dms"],"max_wait":1}`)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("only the DM should pass a dm:* filter, got %v", out["messages"])
	}
	m := msgs[0].(map[string]any)
	if m["text"] != "in dm" {
		t.Fatalf("wrong message survived the filter: %v", m)
	}
	// The alias of a wildcard rule is not an address, so the label has to be the
	// concrete dm:<peer> the caller can send back without guessing.
	wantStr(t, m, "target", "dm:555444333222111000")
}

// The other half of the same rule: "dms" is a policy, so discord_send refuses it
// and points at the id form instead of silently failing somewhere else.
func TestSendRefusesWildcardAlias(t *testing.T) {
	d := newDeps(t, pollRoutingCfg(t, m2cRouting))
	connected(d)

	out := sendErr(t, d, `{"to":"dms","text":"hi"}`)
	wantCode(t, out, "wildcard_target")
	if hint, _ := out["hint"].(string); hint == "" {
		t.Fatalf("a refusal the model can act on needs a hint: %v", out)
	}

	buf := withDebugLog(t, d)
	if o := sendOK(t, d, `{"to":"dm:555444333222111000","text":"hi"}`); o["ok"] != true {
		t.Fatalf("default widened dms and should send: %v", o)
	}
	sentTo(t, buf, "kind=dm", "target=dms", "channel_id=dry-run-dm:555444333222111000")
}

// An agent that was told it may not read a target cannot poll it into its queue
// either -- the grant means the same thing on both ends.
func TestPollRefusesReadOnlyTargetForThisAgent(t *testing.T) {
	d := newDeps(t, pollRoutingCfg(t, m2cRouting))
	connected(d)
	push(t, d, "nosy", m2cDM("d1", "555444333222111000", "in dm"))

	ctx := router.WithPrincipal(context.Background(), router.Principal{AgentID: "nosy"})
	res := pollCall(t, d, ctx, `{"targets":["dms"],"max_wait":1}`)
	if !res.IsError {
		t.Fatalf("nosy holds dms read-only and must not be able to poll it: %s", res.Content[0].Text)
	}
	out := decode(t, res)
	if s, _ := out["error"].(string); s == "" {
		t.Fatalf("a refusal needs a code: %v", out)
	}
}
