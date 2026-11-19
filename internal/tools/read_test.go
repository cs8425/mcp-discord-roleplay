package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/router"
	mcp "mcptransport"
)

// The read tools answer from Discord, not from the queue, so the fixture is a
// scripted answer (discord.ReadHooks) rather than a gateway: what is under test
// here is the tools' own judgement -- who may read, what a row looks like, how a
// budget trims, and which questions deserve a note instead of an error.
//
// The mapping from a discordgo.Message to a bus.Event is proven in
// internal/discord, where the seam sits one level lower.

func readEvent(id, text string, bot bool) *bus.Event {
	return &bus.Event{
		Kind:        bus.KindMessage,
		MessageID:   id,
		Text:        text,
		Ts:          time.Now().Add(-time.Hour),
		Time:        "2026-11-19T10:00:00+08:00",
		Target:      "channel:111222333444555666",
		ChannelID:   "111222333444555666",
		ChannelName: "#rp-main",
		Author:      bus.Author{ID: "477426421900378113", Name: "tapris", Tag: "tapris", Bot: bot},
	}
}

func callRead(t *testing.T, h mcp.Handler, ctx context.Context, args string) *mcp.ToolResult {
	t.Helper()
	res, err := h(ctx, &mcp.Call{Name: "read", Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("a read tool must answer through the result, not the protocol: %v", err)
	}
	if len(res.Content) == 0 || res.Content[0].Type != "text" {
		t.Fatalf("answer has no text part: %+v", res)
	}
	return res
}

// rowKeys names every key one history/poll row may carry. It is written out
// rather than derived so the contract is a decision, not a side effect: a new
// field has to be argued for here before it can cost tokens on every row.
var rowKeys = map[string]bool{
	"id": true, "author": true, "author_id": true, "bot": true, "dm": true,
	"ts": true, "age_s": true, "text": true, "text_truncated": true,
	"mentions_self": true, "attachments": true, "embeds": true, "target": true,
}

func assertRowKeys(t *testing.T, row map[string]any, banned ...string) {
	t.Helper()
	for k := range row {
		if !rowKeys[k] {
			t.Errorf("row carries a key nobody voted for: %q", k)
		}
		for _, b := range banned {
			if k == b {
				t.Errorf("row must not carry %q here: %v", b, row)
			}
		}
	}
}

func TestHistoryAnswersFromTheStore(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	var asked discord.HistoryQuery
	d.DC.SetReadHooks(discord.ReadHooks{
		History: func(_ context.Context, q discord.HistoryQuery) ([]*bus.Event, error) {
			asked = q
			// Oldest first is what the read layer promises, so the hook hands
			// over exactly that and the tool must not reorder it.
			return []*bus.Event{
				readEvent("10", "older", false),
				readEvent("11", "bot noise", true),
				readEvent("12", "newer", false),
			}, nil
		},
	})

	res := callRead(t, d.handleHistory, context.Background(), `{"target":"boss"}`)
	if res.IsError {
		t.Fatalf("history failed: %s", res.Content[0].Text)
	}
	if asked.ChannelID != "111222333444555666" {
		t.Errorf("asked Discord about the wrong channel: %q", asked.ChannelID)
	}
	if asked.Limit != historyDefaultLimit {
		t.Errorf("default limit: got %d, want %d", asked.Limit, historyDefaultLimit)
	}
	if asked.Name != "#rp-main" {
		t.Errorf("the label we already had should go along (cache miss fallback): %q", asked.Name)
	}

	out := decode(t, res)
	if out["target"] != "boss" {
		t.Errorf("envelope target should be the alias the caller can reuse: %v", out["target"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("bot row should be filtered by default, got %d rows: %s", len(msgs), res.Content[0].Text)
	}
	first, _ := msgs[0].(map[string]any)
	last, _ := msgs[1].(map[string]any)
	if first["id"] != "10" || last["id"] != "12" {
		t.Errorf("rows must stay oldest first with their ids: %v / %v", first["id"], last["id"])
	}
	assertRowKeys(t, first, "age_s", "target", "dm")
	if !strings.Contains(fmt.Sprint(out["notes"]), "bot") {
		t.Errorf("dropping a bot's message silently is exactly what a note is for: %v", out["notes"])
	}
}

func TestHistoryIncludeBotsAndCursors(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	var asked discord.HistoryQuery
	d.DC.SetReadHooks(discord.ReadHooks{
		History: func(_ context.Context, q discord.HistoryQuery) ([]*bus.Event, error) {
			asked = q
			return []*bus.Event{readEvent("1", "a", false), readEvent("2", "b", true)}, nil
		},
	})
	res := callRead(t, d.handleHistory, context.Background(),
		`{"target":"boss","include_bots":true,"limit":9,"before":"777"}`)
	if res.IsError {
		t.Fatalf("history failed: %s", res.Content[0].Text)
	}
	if asked.Limit != 9 || asked.Before != "777" {
		t.Errorf("limit/before did not reach the query: %+v", asked)
	}
	out := decode(t, res)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("include_bots should keep the bot row: %s", res.Content[0].Text)
	}
}

func TestHistoryBudgetDropsTheOldestRows(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-history-max-bytes=1"))
	d.DC.SetReadHooks(discord.ReadHooks{
		History: func(context.Context, discord.HistoryQuery) ([]*bus.Event, error) {
			return []*bus.Event{
				readEvent("10", strings.Repeat("x", 200), false),
				readEvent("11", "middle", false),
				readEvent("12", "newest", false),
			}, nil
		},
	})
	res := callRead(t, d.handleHistory, context.Background(), `{"target":"boss"}`)
	out := decode(t, res)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("a 1-byte budget should leave one row, got %d: %s", len(msgs), res.Content[0].Text)
	}
	one, _ := msgs[0].(map[string]any)
	if one["id"] != "12" {
		t.Errorf("trimming must eat the OLDEST rows, not the newest: %v", one["id"])
	}
	if !strings.Contains(fmt.Sprint(out["notes"]), "--history-max-bytes") {
		t.Errorf("a trimmed answer has to say which budget cut it: %v", out["notes"])
	}
}

func TestHistoryEmptyIsAnAnswerNotAnError(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	d.DC.SetReadHooks(discord.ReadHooks{
		History: func(context.Context, discord.HistoryQuery) ([]*bus.Event, error) {
			return []*bus.Event{}, nil
		},
	})
	res := callRead(t, d.handleHistory, context.Background(), `{"target":"log"}`)
	if res.IsError {
		t.Fatalf("an empty channel is not an error: %s", res.Content[0].Text)
	}
	out := decode(t, res)
	if _, ok := out["messages"]; ok {
		t.Errorf("no rows should mean no messages key: %s", res.Content[0].Text)
	}
	if !strings.Contains(fmt.Sprint(out["notes"]), "沒有讀到") {
		t.Errorf("silence needs a note: %v", out["notes"])
	}
}

// TestHistorySaysWhenTheEmptinessIsOurs: "no rows" and "rows with nothing in them" are
// two different facts, and only the rows can tell them apart. We report which one we
// got and stop there -- why Discord withholds content is decided over there (Portal
// setting / review), not by us. So the note names the shape, not a cause.
func TestHistorySaysWhenTheEmptinessIsOurs(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-no-message-content"))
	d.DC.SetReadHooks(discord.ReadHooks{
		History: func(context.Context, discord.HistoryQuery) ([]*bus.Event, error) {
			return []*bus.Event{readEvent("30", "", false), readEvent("31", "", false)}, nil
		},
	})
	out := decode(t, callRead(t, d.handleHistory, context.Background(), `{"target":"boss"}`))
	if !strings.Contains(fmt.Sprint(out["notes"]), "全空") {
		t.Errorf("a blank page should say it is blank, not just empty: %v", out["notes"])
	}

	// But it must stay quiet when the channel is merely talking, because the same
	// sentence on a normal answer is just noise.
	d2 := newDeps(t, routingCfg(t, "-no-message-content"))
	d2.DC.SetReadHooks(discord.ReadHooks{
		History: func(context.Context, discord.HistoryQuery) ([]*bus.Event, error) {
			return []*bus.Event{readEvent("32", "talking", false)}, nil
		},
	})
	out2 := decode(t, callRead(t, d2.handleHistory, context.Background(), `{"target":"boss"}`))
	if strings.Contains(fmt.Sprint(out2["notes"]), "全空") {
		t.Errorf("no noise when the rows have content: %v", out2["notes"])
	}
}

func TestHistoryAsksTheReadHalfOfTheACL(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	called := 0
	d.DC.SetReadHooks(discord.ReadHooks{
		History: func(context.Context, discord.HistoryQuery) ([]*bus.Event, error) {
			called++
			return []*bus.Event{readEvent("1", "hi", false)}, nil
		},
	})

	// log is read:true write:false -- send refuses it, history must not.
	if res := callRead(t, d.handleHistory, context.Background(), `{"target":"log"}`); res.IsError {
		t.Errorf("a read_only target must be readable: %s", res.Content[0].Text)
	}
	if called != 1 {
		t.Fatalf("history should have asked Discord once, asked %d", called)
	}

	// solo may see boss only: a target that exists but is somebody else's.
	solo := router.WithPrincipal(context.Background(), router.Principal{AgentID: "solo"})
	res := callRead(t, d.handleHistory, solo, `{"target":"log"}`)
	if !res.IsError {
		t.Fatalf("a foreign agent must not read another persona's target: %s", res.Content[0].Text)
	}
	if decode(t, res)["error"] != "foreign_agent" {
		t.Errorf("wrong reason: %s", res.Content[0].Text)
	}
	if called != 1 {
		t.Errorf("a rejected target must never reach Discord (called=%d)", called)
	}

	res = callRead(t, d.handleHistory, context.Background(), `{"target":"ghost"}`)
	if decode(t, res)["error"] != "unknown_target" {
		t.Errorf("wrong reason for an alias that is not in routing.json: %s", res.Content[0].Text)
	}

	res = callRead(t, d.handleHistory, context.Background(), `{}`)
	if decode(t, res)["error"] != "empty_target" {
		t.Errorf("target is required: %s", res.Content[0].Text)
	}
}

func TestGetReadsOneMessageWithItsOwnChannel(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	var gotCh, gotID string
	d.DC.SetReadHooks(discord.ReadHooks{
		Get: func(_ context.Context, channelID, messageID string) (*bus.Event, error) {
			gotCh, gotID = channelID, messageID
			ev := readEvent("55", "look", false)
			ev.Attachments = []bus.Attachment{{ID: "a1", Filename: "pic.png", Kind: bus.KindImage, URL: "https://cdn/pic.png"}}
			return ev, nil
		},
	})
	res := callRead(t, d.handleGet, context.Background(), `{"target":"boss","message":"55"}`)
	if res.IsError {
		t.Fatalf("get failed: %s", res.Content[0].Text)
	}
	if gotCh != "111222333444555666" || gotID != "55" {
		t.Errorf("get asked about the wrong place: %s / %s", gotCh, gotID)
	}
	out := decode(t, res)
	if out["target"] != "boss" || out["channel"] != "111222333444555666" {
		t.Errorf("envelope should name both the alias and the resolved channel: %v", out)
	}
	msg, _ := out["message"].(map[string]any)
	if msg["id"] != "55" || msg["text"] != "look" {
		t.Errorf("message body: %v", msg)
	}
	assertRowKeys(t, msg, "age_s", "target", "dm")
	atts, _ := msg["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("the attachment row disappeared: %v", msg["attachments"])
	}
	att, _ := atts[0].(map[string]any)
	if att["media"] != "url" {
		t.Errorf("a deployment with no media fetcher reports a url: %v", att)
	}
}

func TestGetRejectsWhatItCannotDo(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	d.DC.SetReadHooks(discord.ReadHooks{
		Get: func(_ context.Context, _, id string) (*bus.Event, error) {
			return readEvent(id, "x", false), nil
		},
	})
	res := callRead(t, d.handleGet, context.Background(), `{"target":"boss","message":"55","media":"bogus"}`)
	if decode(t, res)["error"] != "bad_media" {
		t.Errorf("media has a fixed vocabulary: %s", res.Content[0].Text)
	}
	res = callRead(t, d.handleGet, context.Background(), `{"target":"boss"}`)
	if decode(t, res)["error"] != "bad_arguments" {
		t.Errorf("message is required: %s", res.Content[0].Text)
	}
	// The target is checked before the message id is ever used, so a typo in the
	// address costs one round trip and says "target", not "not found".
	res = callRead(t, d.handleGet, context.Background(), `{"target":"ghost","message":"55"}`)
	if decode(t, res)["error"] != "unknown_target" {
		t.Errorf("wrong reason: %s", res.Content[0].Text)
	}
}

// TestReadEnvelopeAnswersDMOnce is the contract the live box taught, not a
// hypothetical: a message read back over REST carries no guild_id (only the gateway
// puts one there), so normalize set DM=true on every row we read -- a text channel in
// a guild answered "dm":true, five times in a row, confidently. The read tools know
// the channel from the rule they resolved, and one true answer per call beats a
// guess per row; poll keeps dm per row because a poll really does mix channels.
func TestReadEnvelopeAnswersDMOnce(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	guildish := func(id, text string) *bus.Event {
		ev := readEvent(id, text, false)
		ev.DM = true // exactly what a REST message looks like to normalize()
		return ev
	}
	d.DC.SetReadHooks(discord.ReadHooks{
		History: func(_ context.Context, _ discord.HistoryQuery) ([]*bus.Event, error) {
			return []*bus.Event{guildish("20", "one")}, nil
		},
		Get: func(_ context.Context, _, _ string) (*bus.Event, error) {
			return guildish("21", "two"), nil
		},
	})

	h := decode(t, callRead(t, d.handleHistory, context.Background(), `{"target":"boss"}`))
	if _, ok := h["dm"]; ok {
		t.Errorf("a guild channel must not answer dm:true just because REST omitted guild_id: %v", h["dm"])
	}
	rows, _ := h["messages"].([]any)
	if len(rows) != 1 {
		t.Fatalf("one row expected: %v", h["messages"])
	}
	assertRowKeys(t, rows[0].(map[string]any), "dm")

	g := decode(t, callRead(t, d.handleGet, context.Background(), `{"target":"ami","message":"21"}`))
	if g["dm"] != true {
		t.Errorf("an alias whose rule is a DM says so once, in the envelope: %v", g)
	}
	assertRowKeys(t, g["message"].(map[string]any), "dm")
	if s, _ := g["channel"].(string); s != "dry-run-dm:222333444555666777" {
		t.Errorf("a DM target reads the DM channel, not the alias: %q", s)
	}
}

func TestChannelsKeepsDiscordsDirectoryApartFromOurACL(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	var asked string
	d.DC.SetReadHooks(discord.ReadHooks{
		ListChannels: func(_ context.Context, guildID string) ([]discord.GuildInfo, error) {
			asked = guildID
			all := []discord.GuildInfo{{
				ID: "1572219567919472865", Name: "tapris",
				Channels: []discord.ChannelInfo{
					{ID: "1", Name: "test", Type: "text"},
					{ID: "2", Name: "general", Type: "text"},
					{ID: "3", Name: "suggestions", Type: "forum"},
				},
			}}
			// Narrowing to one guild is the read layer's job (it is the one that
			// knows Discord's directory), so the scripted answer respects it too.
			if guildID != "" && all[0].ID != guildID && !strings.EqualFold(all[0].Name, guildID) {
				return nil, nil
			}
			return all, nil
		},
	})
	res := callRead(t, d.handleChannels, context.Background(), `{}`)
	if res.IsError {
		t.Fatalf("channels failed: %s", res.Content[0].Text)
	}
	if asked != "" {
		t.Errorf("no guild filter should mean every guild: %q", asked)
	}
	out := decode(t, res)
	guilds, _ := out["guilds"].([]any)
	if len(guilds) != 1 {
		t.Fatalf("guilds: %v", out["guilds"])
	}
	g, _ := guilds[0].(map[string]any)
	chs, _ := g["channels"].([]any)
	if len(chs) != 3 {
		t.Errorf("all three channels should be listed: %v", g["channels"])
	}
	rt, _ := out["routing"].(map[string]any)
	if rt == nil {
		t.Fatalf("the routing half is the answer to 'can I post here': %s", res.Content[0].Text)
	}
	if !strings.Contains(fmt.Sprint(rt["targets"]), "boss") {
		t.Errorf("writable aliases: %v", rt["targets"])
	}
	if !strings.Contains(fmt.Sprint(rt["read_only"]), "log") {
		t.Errorf("log is write:false, so it belongs in read_only: %v", rt["read_only"])
	}

	res = callRead(t, d.handleChannels, context.Background(), `{"type":"forum"}`)
	out = decode(t, res)
	guilds, _ = out["guilds"].([]any)
	g, _ = guilds[0].(map[string]any)
	chs, _ = g["channels"].([]any)
	if len(chs) != 1 {
		t.Errorf("type filter: %v", g["channels"])
	}

	res = callRead(t, d.handleChannels, context.Background(), `{"type":"bogus"}`)
	if decode(t, res)["error"] != "bad_type" {
		t.Errorf("an unknown filter word is a typo, and must be refused: %s", res.Content[0].Text)
	}

	res = callRead(t, d.handleChannels, context.Background(), `{"guild":"Nope"}`)
	if decode(t, res)["error"] != "no_such_guild" {
		t.Errorf("no_such_guild: %s", res.Content[0].Text)
	}
}

func TestChannelsGuildFilterIsPassedThrough(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	d.DC.SetReadHooks(discord.ReadHooks{
		ListChannels: func(_ context.Context, guildID string) ([]discord.GuildInfo, error) {
			asked := guildID
			if asked != "tapris" {
				t.Errorf("guild should reach the read layer verbatim: %q", asked)
			}
			return []discord.GuildInfo{{ID: "9", Name: "tapris"}}, nil
		},
	})
	res := callRead(t, d.handleChannels, context.Background(), `{"guild":"tapris"}`)
	out := decode(t, res)
	guilds, _ := out["guilds"].([]any)
	if len(guilds) != 1 {
		t.Fatalf("guilds: %v", out["guilds"])
	}
	g, _ := guilds[0].(map[string]any)
	if _, ok := g["channels"]; ok {
		t.Errorf("a guild with no channels should omit the key, not ship an empty list: %v", g)
	}
	if !strings.Contains(fmt.Sprint(out["notes"]), "讀不到任何頻道") {
		t.Errorf("an empty guild needs a note saying it is empty, not silence: %v", out["notes"])
	}
}
