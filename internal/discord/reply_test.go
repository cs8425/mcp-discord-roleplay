package discord

// A reply is the one piece of conversation structure Discord keeps somewhere
// other than the text: message_reference says which message this one answers, and
// referenced_message — when Discord bothered to look it up — carries the words.
// discordgo types that second field as *Message, so "we did not go look" and "that
// message is gone" both arrive as nil, and a mapping that guessed would be
// inventing either a deletion or an empty quote. Forwarding rides the same field
// with a different type, except the quoted message is not in referenced_message at
// all: it is a snapshot in message_snapshots, and the forwarding message itself
// has nothing to say. These tests pin what reaches bus.Event, because poll,
// history and get all read this one mapping and none of them can fix what it
// dropped.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// replyMsg is a gateway-shaped message that answers another one.
func replyMsg(content string, ref *discordgo.MessageReference, quoted *discordgo.Message) *discordgo.Message {
	return &discordgo.Message{
		ID: "m2", ChannelID: "c1", GuildID: "g1", Content: content,
		Author:            &discordgo.User{ID: "u2", Username: "carrier", GlobalName: "carrier"},
		MessageReference:  ref,
		ReferencedMessage: quoted,
	}
}

// forwardMsg is what Discord sends when someone forwards: a message with no words
// of its own, a reference of type 1, and the original photographed in a snapshot.
func forwardMsg(content string, snap *discordgo.Message) *discordgo.Message {
	m := &discordgo.Message{
		ID: "f1", ChannelID: "c1", GuildID: "g1", Content: content,
		Author:           &discordgo.User{ID: "u3", Username: "carrier", GlobalName: "carrier"},
		MessageReference: &discordgo.MessageReference{Type: discordgo.MessageReferenceTypeForward, MessageID: "orig1"},
	}
	if snap != nil {
		m.MessageSnapshots = []discordgo.MessageSnapshot{{Message: snap}}
	}
	return m
}

func TestNormalizeReplyWithoutTheQuotedMessageGivesOnlyTheID(t *testing.T) {
	ev := normalizeOf(t, replyMsg("對啊", &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"}, nil))
	if ev.Reply == nil {
		t.Fatal("a message_reference must reach the row even when nothing was fetched for it")
	}
	if ev.Reply.ID != "m1" {
		t.Errorf("the id is the one thing we always have: %q", ev.Reply.ID)
	}
	if ev.Reply.Text != "" || ev.Reply.Author != "" {
		t.Errorf("nil referenced_message must not become an invented quote: %+v", *ev.Reply)
	}
	if ev.Forwarded {
		t.Error("a reply is not a forward")
	}
}

func TestNormalizeReplyCarriesTheQuotedAuthorAndText(t *testing.T) {
	quoted := &discordgo.Message{
		ID:      "m1",
		Content: "今晚要下海嗎",
		Author:  &discordgo.User{ID: "u1", Username: "rawname", GlobalName: " displayName "},
	}
	ev := normalizeOf(t, replyMsg("要", &discordgo.MessageReference{MessageID: "m1"}, quoted))
	if ev.Reply == nil {
		t.Fatal("the quoted message was handed to us and must show up")
	}
	if ev.Reply.Text != "今晚要下海嗎" {
		t.Errorf("quoted text: %q", ev.Reply.Text)
	}
	if ev.Reply.AuthorID != "u1" {
		t.Errorf("the author id is what a caller can look up later: %q", ev.Reply.AuthorID)
	}
	// DisplayName trims and falls back to the username; whatever it decides, the
	// quoted person must be spelled the way the row's own author would be.
	if ev.Reply.Author == "" || ev.Reply.AuthorID == "" {
		t.Errorf("a quoted author must be named: %+v", *ev.Reply)
	}
	if ev.Reply.Author == "rawname" {
		t.Errorf("a user with a display name must not degrade to their username: %q", ev.Reply.Author)
	}
}

func TestNormalizeReplyAuthorPrefersTheGuildNickname(t *testing.T) {
	quoted := &discordgo.Message{
		ID:      "m1",
		Content: "被引用的話",
		Author:  &discordgo.User{ID: "u1", Username: "rawname", GlobalName: "全域名"},
		Member:  &discordgo.Member{Nick: "群內暱稱"},
	}
	ev := normalizeOf(t, replyMsg("嗯", &discordgo.MessageReference{MessageID: "m1"}, quoted))
	if ev.Reply == nil || ev.Reply.Author != "群內暱稱" {
		t.Fatalf("the gateway put a nickname in our hand: %+v", ev.Reply)
	}
}

// A REST row has no `member` on the quoted message either, so the state cache is
// the only place a nickname can come from — and it must not cost a request.
func TestNormalizeReplyAuthorComesFromStateCache(t *testing.T) {
	c, _ := logClient(slog.LevelInfo)
	c.cfg = testCfg(t)
	c.sess = nickState(t,
		&discordgo.Member{GuildID: "g1", User: &discordgo.User{ID: "u1"}, Nick: "cache暱稱"},
		nickChannel(),
	)
	quoted := &discordgo.Message{
		ID: "m1", Content: "話",
		Author: &discordgo.User{ID: "u1", Username: "rawname", GlobalName: "全域名"},
	}
	ev := c.normalize(c.sess, replyMsg("嗯", &discordgo.MessageReference{MessageID: "m1"}, quoted))
	if ev.Reply == nil || ev.Reply.Author != "cache暱稱" {
		t.Fatalf("a cached nickname must reach the quote: %+v", ev.Reply)
	}
}

// stateNick is allowed to be called with nothing to answer from: no session, no
// guild, a stranger. Every one of those has to be a quiet "" rather than a panic
// or a request.
func TestStateNickSurvivesHavingNothingToSay(t *testing.T) {
	c, _ := logClient(slog.LevelInfo)
	if got := c.stateNick(nil, "g1", "u1"); got != "" {
		t.Errorf("no session: %q", got)
	}
	sess := nickState(t, nil, nickChannel())
	if got := c.stateNick(sess, "", "u1"); got != "" {
		t.Errorf("no guild: %q", got)
	}
	if got := c.stateNick(sess, "g1", "stranger"); got != "" {
		t.Errorf("an uncached member: %q", got)
	}
}

func TestNormalizeForwardTakesTheSnapshotWords(t *testing.T) {
	snap := &discordgo.Message{
		Content: "原本的話",
		Attachments: []*discordgo.MessageAttachment{
			{ID: "a1", Filename: "p.png", ContentType: "image/png", URL: "https://cdn/p.png"},
		},
	}
	ev := normalizeOf(t, forwardMsg("", snap))
	if !ev.Forwarded {
		t.Error("a forwarded message says so on the row: the words are not this person's")
	}
	if ev.Text != "原本的話" {
		t.Errorf("the snapshot is the only place the words are: %q", ev.Text)
	}
	if len(ev.Attachments) != 1 || ev.Attachments[0].Filename != "p.png" {
		t.Errorf("an attachment that lives only in the snapshot must still be ours: %+v", ev.Attachments)
	}
}

// Someone can forward and then add a line of their own. That line is the message;
// the snapshot only fills the holes it left.
func TestNormalizeForwardKeepsItsOwnWordsWhereItSpoke(t *testing.T) {
	snap := &discordgo.Message{Content: "原本的話"}
	ev := normalizeOf(t, forwardMsg("加上自己的一句", snap))
	if ev.Text != "加上自己的一句" {
		t.Errorf("the forwarding message wins where it is not empty: %q", ev.Text)
	}
	if !ev.Forwarded {
		t.Error("it is still a forward")
	}
}

// A snapshot whose inner message is nil is a shape we cannot read, not an empty
// original: keep the message, lose the quote's words, and above all do not panic.
func TestNormalizeForwardWithAnUnreadableSnapshot(t *testing.T) {
	ev := normalizeOf(t, forwardMsg("", &discordgo.Message{})) // MessageSnapshots[0].Message == nil
	if !ev.Forwarded {
		t.Error("the reference still says forward")
	}
	if ev.Text != "" {
		t.Errorf("nothing was filled from nothing: %q", ev.Text)
	}
}

func TestNormalizeForwardWithNoSnapshotAtAll(t *testing.T) {
	ev := normalizeOf(t, forwardMsg("", nil))
	if !ev.Forwarded {
		t.Error("a forward with no snapshot is still a forward")
	}
	if ev.Text != "" {
		t.Errorf("no snapshot, no words: %q", ev.Text)
	}
}

// The quoted original of a forward lives in another channel, sometimes another
// guild, and referenced_message is not filled for it at all. Reporting it as a
// reply would point at a message this conversation never saw.
func TestNormalizeForwardIsNotAReply(t *testing.T) {
	ev := normalizeOf(t, forwardMsg("", &discordgo.Message{Content: "話"}))
	if ev.Reply != nil {
		t.Errorf("a forward is not a reply to anything here: %+v", *ev.Reply)
	}
}

// mentions_self is computed from the words the row carries, and for a forward
// those words are in the snapshot — a bot mentioned inside the original must
// still be considered spoken to.
func TestNormalizeMentionsSelfFromTheSnapshot(t *testing.T) {
	c, _ := logClient(slog.LevelInfo)
	c.cfg = testCfg(t)
	c.snap.SelfID = "u9"
	m := forwardMsg("", &discordgo.Message{Content: "呼叫 <@u9> 過來看"})
	ev := c.normalize(nil, m)
	if !ev.MentionsSelf {
		t.Errorf("a mention inside the snapshot must count: %q", ev.Text)
	}
	if !strings.Contains(ev.Text, "<@u9>") {
		t.Errorf("the raw mention stays in the text for the model to see: %q", ev.Text)
	}
}

// The reference is soft and the ping is off, and both are decisions the model must
// not get to make for itself: an id Discord cannot see should cost us the quote and
// not the message, and answering someone a dozen times a day should not notify them
// a dozen times.
func TestBuildMessageSendReplyReference(t *testing.T) {
	if data := buildMessageSend(SendRequest{ChannelID: "c", Text: "hi"}); data.Reference != nil {
		t.Errorf("no reply_to means no reference at all, not an empty one: %+v", data.Reference)
	}
	data := buildMessageSend(SendRequest{ChannelID: "c", Text: "hi", ReplyTo: "1572567398089883688"})
	if data.Reference == nil {
		t.Fatal("reply_to has to become a message_reference")
	}
	if data.Reference.MessageID != "1572567398089883688" {
		t.Errorf("the reference must carry the id we were given: %q", data.Reference.MessageID)
	}
	if data.Reference.FailIfNotExists == nil || *data.Reference.FailIfNotExists {
		t.Error("the reference must be soft: fail_if_not_exists=false, so an id that is gone sends the message unquoted")
	}
	if data.AllowedMentions == nil || data.AllowedMentions.RepliedUser {
		t.Error("a reply quotes without pinging: replied_user has to go out as false")
	}
}
