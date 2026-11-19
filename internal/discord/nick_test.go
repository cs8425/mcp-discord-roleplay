package discord

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// --- the author's guild nickname on the read path ---------------------------
//
// Discord's REST message object carries neither `member` nor `guild_id`, so a
// history row has no nickname in it while a gateway row does. These tests are the
// proof that the read path closes that gap without going to the network for
// anything the cache can answer, and without ever erasing what it learned.

func nickChannel() *discordgo.Channel {
	return &discordgo.Channel{ID: "c1", GuildID: "g1", Name: "test"}
}

// nickState is a state cache that knows guild g1 and its channel c1. Passing a nil
// member leaves the guild's member map EMPTY, which is exactly what a large guild
// looks like: GUILD_CREATE arrived, Discord sent no members with it.
func nickState(t *testing.T, member *discordgo.Member, ch *discordgo.Channel) *discordgo.Session {
	t.Helper()
	g := &discordgo.Guild{ID: "g1", Channels: []*discordgo.Channel{ch}}
	if member != nil {
		g.Members = []*discordgo.Member{member}
	} else {
		g.Members = []*discordgo.Member{}
	}
	st := discordgo.NewState()
	if err := st.GuildAdd(g); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return &discordgo.Session{State: st}
}

// restMsg is a message as GET /channels/{id}/messages answers it: an author, and
// nothing that says which guild this is.
func restMsg(id, authorID, globalName string) *discordgo.Message {
	return &discordgo.Message{
		ID: id, ChannelID: "c1",
		Author: &discordgo.User{ID: authorID, Username: "rawname", GlobalName: globalName},
	}
}

func TestReadPathAuthorNickComesFromStateCache(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t)
	c.sess = nickState(t, &discordgo.Member{GuildID: "g1", User: &discordgo.User{ID: "u1"}, Nick: "群內暱稱"}, nickChannel())
	calls := 0
	c.read = &readSeam{guildMember: func(context.Context, string, string) (*discordgo.Member, error) {
		calls++
		return &discordgo.Member{Nick: "不該被用到"}, nil
	}}

	ev := c.event(context.Background(), c.read, restMsg("m1", "u1", "全域名"), "test", map[string]struct{}{})
	if ev.Author.Name != "群內暱稱" {
		t.Errorf("author = %q, want the cached nickname", ev.Author.Name)
	}
	if calls != 0 {
		t.Errorf("a cache hit cost %d REST calls, want 0", calls)
	}
}

func TestReadPathMissFetchesOnceAndCachesIt(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t)
	c.sess = nickState(t, nil, nickChannel()) // member map exists but is empty
	calls := 0
	c.read = &readSeam{guildMember: func(context.Context, string, string) (*discordgo.Member, error) {
		calls++
		// Discord's answer, missing the identity fields a cache entry needs.
		return &discordgo.Member{Nick: "群內暱稱"}, nil
	}}

	first := c.event(context.Background(), c.read, restMsg("m1", "u1", "全域名"), "test", map[string]struct{}{})
	if first.Author.Name != "群內暱稱" {
		t.Fatalf("author = %q, want the fetched nickname", first.Author.Name)
	}
	// A NEW page (fresh probe set) must not ask again: the answer went into state.
	second := c.event(context.Background(), c.read, restMsg("m2", "u1", "全域名"), "test", map[string]struct{}{})
	if calls != 1 {
		t.Errorf("member fetches = %d, want 1 for two pages of the same author", calls)
	}
	if second.Author.Name != "群內暱稱" {
		t.Errorf("second page author = %q, want the cached nickname", second.Author.Name)
	}
	mem, err := c.sess.State.Member("g1", "u1")
	if err != nil || mem == nil {
		t.Fatalf("the answer was not written back to state: %v", err)
	}
	if mem.GuildID != "g1" || mem.User == nil || mem.User.ID != "u1" {
		t.Errorf("the cache entry has no identity, so nothing else could ever find it: %+v", mem)
	}
}

// A cached member whose Nick is empty is an ANSWER, not a miss: it is the
// difference between "this person has no nickname" and "we never looked", and
// treating them alike would re-fetch every user without a nickname on every page.
func TestReadPathCachedNoNickDoesNotFetch(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t)
	c.sess = nickState(t, &discordgo.Member{GuildID: "g1", User: &discordgo.User{ID: "u1"}}, nickChannel())
	calls := 0
	c.read = &readSeam{guildMember: func(context.Context, string, string) (*discordgo.Member, error) {
		calls++
		return nil, errors.New("must not be asked")
	}}

	ev := c.event(context.Background(), c.read, restMsg("m1", "u1", "全域名"), "test", map[string]struct{}{})
	if calls != 0 {
		t.Errorf("a known user without a nickname cost %d REST calls", calls)
	}
	if ev.Author.Name != "全域名" {
		t.Errorf("author = %q, want the global display name to survive", ev.Author.Name)
	}
}

// The dedupe is per call, and it holds for failures too: a guild we may not read
// members of must not turn one page of 50 into 50 requests.
func TestHistoryDedupesAFailingLookupPerPage(t *testing.T) {
	c, _ := logClient(slog.LevelWarn)
	c.cfg = testCfg(t)
	c.sess = nickState(t, nil, nickChannel())
	calls := 0
	authorOf := map[string]string{}
	c.read = &readSeam{
		messages: func(context.Context, string, int, string, string, string) ([]*discordgo.Message, error) {
			return []*discordgo.Message{
				restMsg("m1", "u1", "全域名"), restMsg("m2", "u1", "全域名"),
				restMsg("m3", "u2", "另一人"),
			}, nil
		},
		guildMember: func(_ context.Context, _, userID string) (*discordgo.Member, error) {
			calls++
			authorOf[userID] = userID
			return nil, errors.New("403 Missing Access")
		},
	}

	evs, err := c.History(context.Background(), HistoryQuery{ChannelID: "c1", Name: "test"})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if calls != 2 {
		t.Errorf("member fetches = %d, want one per DISTINCT author (%v)", calls, authorOf)
	}
	// A failed lookup must not rewrite anything -- and "anything" is per author:
	// m1/m2 are u1 (全域名), m3 is u2 (另一人). Asserting one literal name for the
	// whole page would only prove the page had a single author.
	want := map[string]string{"u1": "全域名", "u2": "另一人"}
	for _, ev := range evs {
		if ev.Author.Name != want[ev.Author.ID] {
			t.Errorf("author %s name = %q, want %q (the name Discord's own REST object gave); a failed lookup must not rewrite the row",
				ev.Author.ID, ev.Author.Name, want[ev.Author.ID])
		}
	}
}

func TestReadPathNickLookupOffNeverLooks(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t, "-nick-lookup=off")
	c.sess = nickState(t, &discordgo.Member{GuildID: "g1", User: &discordgo.User{ID: "u1"}, Nick: "群內暱稱"}, nickChannel())
	calls := 0
	c.read = &readSeam{guildMember: func(context.Context, string, string) (*discordgo.Member, error) {
		calls++
		return nil, nil
	}}

	ev := c.event(context.Background(), c.read, restMsg("m1", "u1", "全域名"), "test", map[string]struct{}{})
	if ev.Author.Name != "全域名" {
		t.Errorf("author = %q; off means off, even when the cache knows better", ev.Author.Name)
	}
	if calls != 0 {
		t.Errorf("off still made %d REST calls", calls)
	}
}

// A DM has no nickname to look up, and a channel the state has never seen has no
// guild to ask about. Both must answer without a request, or every DM page burns
// one.
func TestReadPathLooksUpNothingForDMAndUnknownChannel(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t)
	c.sess = nickState(t, nil, &discordgo.Channel{ID: "c1", Name: "dm-ish"}) // GuildID empty
	calls := 0
	c.read = &readSeam{guildMember: func(context.Context, string, string) (*discordgo.Member, error) {
		calls++
		return nil, nil
	}}

	ev := c.event(context.Background(), c.read, restMsg("m1", "u1", "全域名"), "dm:u1", map[string]struct{}{})
	if calls != 0 {
		t.Errorf("a DM cost %d member requests", calls)
	}
	if ev.Author.Name != "全域名" {
		t.Errorf("DM author = %q, want the name as given", ev.Author.Name)
	}
}

// An object that DOES carry a member (a gateway event, or a message we read out of
// discordgo's own ring) is already answered by normalize; applyNick must not
// second-guess it.
func TestReadPathLeavesAGatewayShapedMessageAlone(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t)
	c.sess = nickState(t, &discordgo.Member{GuildID: "g1", User: &discordgo.User{ID: "u1"}, Nick: "舊的"}, nickChannel())
	calls := 0
	c.read = &readSeam{guildMember: func(context.Context, string, string) (*discordgo.Member, error) {
		calls++
		return nil, nil
	}}

	m := restMsg("m1", "u1", "全域名")
	m.GuildID = "g1"
	m.Member = &discordgo.Member{Nick: "新的"}
	ev := c.event(context.Background(), c.read, m, "test", map[string]struct{}{})
	if calls != 0 {
		t.Errorf("an object that carries its own member still went to REST %d times", calls)
	}
	if ev.Author.Name != "新的" {
		t.Errorf("author = %q, want the member the message itself carried", ev.Author.Name)
	}
}

// rememberMember is why the cache fills up at all on a guild where GUILD_CREATE
// brought no members: discordgo drops the member object of a message, and we do
// not subscribe to GUILD_MEMBERS, so this is the only traffic that teaches us
// names.
func TestRememberMemberFeedsTheCache(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t)
	c.sess = nickState(t, nil, nickChannel())

	msg := &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Author: &discordgo.User{ID: "u1", Username: "rawname"},
		Member: &discordgo.Member{Nick: "群內暱稱"}, // partial: no guild_id, no user
	}
	c.rememberMember(c.sess, msg)
	mem, err := c.sess.State.Member("g1", "u1")
	if err != nil || mem == nil {
		t.Fatalf("the member of a message never reached the cache: %v", err)
	}
	if mem.Nick != "群內暱稱" {
		t.Errorf("cached nick = %q", mem.Nick)
	}

	// An empty nick is not evidence: it is the field being absent as much as the
	// nickname being cleared, and memberAdd replaces wholesale. One such message
	// must not erase what we know.
	c.rememberMember(c.sess, &discordgo.Message{
		ID: "m2", ChannelID: "c1", GuildID: "g1",
		Author: &discordgo.User{ID: "u1"},
		Member: &discordgo.Member{Nick: ""},
	})
	if mem, _ = c.sess.State.Member("g1", "u1"); mem.Nick != "群內暱稱" {
		t.Errorf("an empty member object erased the cached nickname: %q", mem.Nick)
	}

	// A guild state does not know must be a no-op, not a panic and not an error
	// the gateway handler has to care about.
	c.rememberMember(c.sess, &discordgo.Message{
		ID: "m3", ChannelID: "c9", GuildID: "g404",
		Author: &discordgo.User{ID: "u1"},
		Member: &discordgo.Member{Nick: "別組"},
	})
	// And nothing at all to write.
	c.rememberMember(c.sess, &discordgo.Message{ID: "m4", Author: &discordgo.User{ID: "u1"}})
	c.rememberMember(nil, &discordgo.Message{ID: "m5", Author: &discordgo.User{ID: "u1"}})
}

// The pair this whole feature exists for: the same message, read two ways, must
// name its author the same way.
func TestGatewayAndReadPathAgreeOnTheNickname(t *testing.T) {
	c, _ := logClient(slog.LevelDebug)
	c.cfg = testCfg(t)
	c.sess = nickState(t, nil, nickChannel())
	c.read = &readSeam{guildMember: func(context.Context, string, string) (*discordgo.Member, error) {
		return &discordgo.Member{Nick: "同一個名字"}, nil
	}}

	viaGateway := c.normalize(c.sess, &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Author: &discordgo.User{ID: "u1", GlobalName: "全域名"},
		Member: &discordgo.Member{Nick: "同一個名字"},
	})
	c.rememberMember(c.sess, &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Author: &discordgo.User{ID: "u1"},
		Member: &discordgo.Member{Nick: "同一個名字"},
	})
	viaREST := c.event(context.Background(), c.read, restMsg("m1", "u1", "全域名"), "test", map[string]struct{}{})
	if viaGateway.Author.Name != viaREST.Author.Name {
		t.Errorf("poll says %q and history says %q", viaGateway.Author.Name, viaREST.Author.Name)
	}
}
