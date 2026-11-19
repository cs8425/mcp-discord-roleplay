package discord

// Stickers and custom emoji reach us as a picture with no words, so the
// job here is to notice that and turn the picture into an attachment the media
// layer can render, leaving a handle in the text that names it. See emoji.go.

import (
	"log/slog"
	"strings"
	"testing"

	"mcp-discord-roleplay/internal/bus"

	"github.com/bwmarrin/discordgo"
)

// normalizeOf runs one message through normalize with the given flags, the way a
// live gateway event would.
func normalizeOf(t *testing.T, m *discordgo.Message, flags ...string) *bus.Event {
	t.Helper()
	c, _ := logClient(slog.LevelInfo)
	c.cfg = testCfg(t, flags...)
	return c.normalize(nil, m)
}

func stickerMsg(content string, items ...*discordgo.StickerItem) *discordgo.Message {
	return &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Content: content, StickerItems: items,
		Author: &discordgo.User{ID: "u1", Username: "someone"},
	}
}

func TestNormalizeStickerBecomesAnAttachment(t *testing.T) {
	ev := normalizeOf(t, stickerMsg("", &discordgo.StickerItem{ID: "9", Name: "gaze", FormatType: 1}))
	if ev.Text != "[sticker:gaze]" {
		t.Errorf("Text = %q, want the placeholder alone", ev.Text)
	}
	if len(ev.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1 synthesized sticker", len(ev.Attachments))
	}
	a := ev.Attachments[0]
	if a.ID != "9" || a.Origin != bus.OriginSticker || a.Kind != bus.KindImage {
		t.Errorf("sticker attachment = %+v", a)
	}
	if want := "https://cdn.discordapp.com/stickers/9.png"; a.URL != want {
		t.Errorf("URL = %q, want %q", a.URL, want)
	}
	if a.Filename != "gaze.png" {
		t.Errorf("Filename = %q, want the sticker's own name", a.Filename)
	}
}

// A sticker sent along with a sentence does not overwrite the sentence: the
// placeholder is added at the end. Discord allows three per message.
func TestNormalizeStickersAppendToText(t *testing.T) {
	ev := normalizeOf(t, stickerMsg("look at this",
		&discordgo.StickerItem{ID: "1", Name: "a", FormatType: 1},
		nil,
		&discordgo.StickerItem{ID: "2", Name: "", FormatType: 1},
	))
	if want := "look at this [sticker:a] [sticker:2]"; ev.Text != want {
		t.Errorf("Text = %q, want %q (an unnamed sticker falls back to its id)", ev.Text, want)
	}
	if len(ev.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2 (nil item skipped)", len(ev.Attachments))
	}
}

// The URL extension must match what the CDN actually serves, the name we show may
// say more than that, and only the formats that are really images should be
// classified as images.
func TestNormalizeStickerFormats(t *testing.T) {
	cases := []struct {
		format  int
		fileExt string
		urlExt  string
		kind    string
	}{
		{1, ".png", ".png", bus.KindImage},
		{2, ".apng.png", ".png", bus.KindImage}, // named as an APNG, fetched as a PNG
		{3, ".json", ".json", bus.KindOther},    // Lottie: a vector animation, nothing to inline
		{4, ".gif", ".gif", bus.KindImage},      // the newer GIF stickers
		{99, ".png", ".png", bus.KindImage},     // unknown: guess the rendition that always exists
	}
	for _, c := range cases {
		ev := normalizeOf(t, stickerMsg("", &discordgo.StickerItem{ID: "7", Name: "s", FormatType: discordgo.StickerFormat(c.format)}))
		a := ev.Attachments[0]
		if !strings.HasSuffix(a.Filename, c.fileExt) {
			t.Errorf("format %d: filename = %q, want suffix %s", c.format, a.Filename, c.fileExt)
		}
		if want := cdnStickerBase + "7" + c.urlExt; a.URL != want {
			t.Errorf("format %d: URL = %q, want %q", c.format, a.URL, want)
		}
		if a.Kind != c.kind {
			t.Errorf("format %d: kind = %s, want %s", c.format, a.Kind, c.kind)
		}
	}
}

// The live bug this pins: an APNG sticker used to be fetched at
// /stickers/<id>.apng, and the CDN answers that with a 404, so the agent got
// media:"url" reason:"error" and no picture at all. The name keeps .apng (one
// step under a .png, which is where the bytes really came from) so a saved
// sticker is not mistaken for a still image.
func TestNormalizeAPNGStickerFetchesThePngRendition(t *testing.T) {
	ev := normalizeOf(t, stickerMsg("", &discordgo.StickerItem{ID: "861289848563957801", Name: "cat", FormatType: 2}))
	if len(ev.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(ev.Attachments))
	}
	a := ev.Attachments[0]
	if want := cdnStickerBase + "861289848563957801.png"; a.URL != want {
		t.Errorf("URL = %q, want %q (the sticker CDN has no .apng endpoint)", a.URL, want)
	}
	if want := "cat.apng.png"; a.Filename != want {
		t.Errorf("Filename = %q, want %q", a.Filename, want)
	}
	if a.ContentType != "image/png" || a.Kind != bus.KindImage {
		t.Errorf("ContentType/Kind = %q/%s, want image/png/%s", a.ContentType, a.Kind, bus.KindImage)
	}
}

func emojiMsg(content string, atts ...*discordgo.MessageAttachment) *discordgo.Message {
	m := stickerMsg(content)
	m.StickerItems = nil
	m.Attachments = atts
	return m
}

// The live case that started this: a message whose content was one custom emoji
// arrived as a code the agent could not resolve, and a message whose content was
// empty arrived with no text at all.
func TestNormalizeEmojiOnlyMessageGetsItsPicture(t *testing.T) {
	ev := normalizeOf(t, emojiMsg("<:gaze:1110036146588749884>"))
	if ev.Text != "<:gaze:img@1>" {
		t.Errorf("Text = %q, want the placeholder pointing at the first attachment", ev.Text)
	}
	if len(ev.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(ev.Attachments))
	}
	a := ev.Attachments[0]
	if a.ID != "1110036146588749884" || a.Origin != bus.OriginEmoji || a.Kind != bus.KindImage {
		t.Errorf("emoji attachment = %+v", a)
	}
	if a.Filename != "1110036146588749884.png" {
		t.Errorf("Filename = %q, want <id>.png so a saved file can be found by id", a.Filename)
	}
	if want := "https://cdn.discordapp.com/emojis/1110036146588749884.png"; a.URL != want {
		t.Errorf("URL = %q, want %q", a.URL, want)
	}
}

func TestNormalizeAnimatedEmojiIsAGif(t *testing.T) {
	ev := normalizeOf(t, emojiMsg("<a:party:42>"))
	if ev.Text != "<a:party:img@1>" {
		t.Errorf("Text = %q, want the a: marker kept", ev.Text)
	}
	if ev.Attachments[0].Filename != "42.gif" || ev.Attachments[0].ContentType != "image/gif" {
		t.Errorf("animated emoji attachment = %+v", ev.Attachments[0])
	}
}

// The same emoji twice is one download, and the repetition itself is information:
// both placeholders name the same index. Everything else, including the uneven
// spacing, is left exactly as it was typed -- we replace codes, not sentences.
func TestNormalizeEmojiDedupById(t *testing.T) {
	ev := normalizeOf(t, emojiMsg("<:a:1><:b:2> <:a:1>"))
	if want := "<:a:img@1><:b:img@2> <:a:img@1>"; ev.Text != want {
		t.Errorf("Text = %q, want %q", ev.Text, want)
	}
	if len(ev.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2 distinct emoji", len(ev.Attachments))
	}
}

// The cap counts distinct emoji, and what falls over it keeps the raw code: the
// agent should see a code it can quote back, not a hole in the sentence.
func TestNormalizeEmojiCapKeepsRawCodes(t *testing.T) {
	ev := normalizeOf(t, emojiMsg("<:a:1> <:b:2> <:c:3> <:d:4> <:e:5> <:f:6> <:g:7> <:h:8> <:i:9> <:j:10>"))
	if len(ev.Attachments) != maxEmojiPerMessage {
		t.Fatalf("attachments = %d, want the cap of %d", len(ev.Attachments), maxEmojiPerMessage)
	}
	want := "<:a:img@1> <:b:img@2> <:c:img@3> <:d:img@4> <:e:img@5> <:f:img@6> <:g:img@7> <:h:img@8> <:i:9> <:j:10>"
	if ev.Text != want {
		t.Errorf("Text = %q,\nwant               %q", ev.Text, want)
	}
}

// A sentence with emoji in it is the common case, not the exception: the words stay
// and only the codes become placeholders. A mention is not an emoji and must not be
// touched (mentions_only depends on it).
func TestNormalizeEmojiInsideSentence(t *testing.T) {
	ev := normalizeOf(t, emojiMsg("<@700000000000000001> nice <:gaze:1> shot <a:party:2>!"))
	want := "<@700000000000000001> nice <:gaze:img@1> shot <a:party:img@2>!"
	if ev.Text != want {
		t.Errorf("Text = %q,\nwant               %q", ev.Text, want)
	}
	if len(ev.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(ev.Attachments))
	}
	if ev.Attachments[1].Filename != "2.gif" {
		t.Errorf("animated emoji inside a sentence = %+v", ev.Attachments[1])
	}
}

// Anything that is not a custom code is left alone, and a message with no code at
// all gets no attachment: no unicode emoji conversion, no guessing.
func TestNormalizeEmojiLeavesEverythingElseAlone(t *testing.T) {
	cases := []struct {
		in   string
		want string
		atts int
	}{
		{"hi there", "hi there", 0},
		{"😀<:gaze:1>", "😀<:gaze:img@1>", 1}, // a unicode emoji is not a custom code
		{"<:gaze:123456789012345678>", "<:gaze:img@1>", 1},
		{"<:gaze:1", "<:gaze:1", 0}, // cut mid-code by --text-max-runes: no match, no picture
		{"   ", "   ", 0},
		{"", "", 0},
	}
	for _, c := range cases {
		ev := normalizeOf(t, emojiMsg(c.in))
		if ev.Text != c.want {
			t.Errorf("%q: Text = %q, want %q", c.in, ev.Text, c.want)
		}
		if len(ev.Attachments) != c.atts {
			t.Errorf("%q: attachments = %d, want %d", c.in, len(ev.Attachments), c.atts)
		}
	}
}

// --media-emoji=text is the escape hatch: the codes stay, no attachment is made,
// so nothing is downloaded for a message the caller does not want pictures of.
func TestNormalizeEmojiTextMode(t *testing.T) {
	ev := normalizeOf(t, emojiMsg("<:gaze:1>"), "-media-emoji=text")
	if ev.Text != "<:gaze:1>" {
		t.Errorf("Text = %q, want the raw code", ev.Text)
	}
	if len(ev.Attachments) != 0 {
		t.Errorf("attachments = %d, want none in text mode", len(ev.Attachments))
	}
}

// img@N is a position in this message's attachment array, so the uploads Discord
// sent itself are numbered first and the emoji come after them.
func TestNormalizeEmojiIndexFollowsRealAttachments(t *testing.T) {
	ev := normalizeOf(t, emojiMsg("<:gaze:1>", &discordgo.MessageAttachment{ID: "a1", Filename: "shot.png", ContentType: "image/png", URL: "https://x/shot.png"}))
	if len(ev.Attachments) != 2 {
		t.Fatalf("attachments = %d, want the upload plus the emoji", len(ev.Attachments))
	}
	if ev.Text != "<:gaze:img@2>" {
		t.Errorf("Text = %q, want the emoji to be attachment #2", ev.Text)
	}
	if ev.Attachments[1].Origin != bus.OriginEmoji {
		t.Errorf("the emoji must come after the upload: %+v", ev.Attachments)
	}
}

// The gate in addEmoji and the parser behind it have to agree, or the parser never
// gets to say its piece: this caught a {2,32} name floor that vetoed every
// one-letter emoji, so the message kept its raw codes and nobody fetched a thing.
func TestEmojiRegexMatchesWhatTheParserAccepts(t *testing.T) {
	for _, raw := range []string{
		"<:a:1>", // one-letter name: impossible from Discord, but not our problem to enforce
		"<:gaze:1>",
		"<a:x:22>",
		"<:emoji_23:123456789012345678>",
		"<:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:9>",
	} {
		if _, ok := parseEmojiToken(raw); !ok {
			t.Fatalf("parseEmojiToken(%q) rejects it, so this case proves nothing", raw)
		}
		if !customEmojiRe.MatchString(raw) {
			t.Errorf("parseEmojiToken accepts %q but customEmojiRe does not: the gate vetoes the parser", raw)
		}
	}
	for _, raw := range []string{"<::1>", "<gaze:1>", "<:gaze:1"} {
		if customEmojiRe.MatchString(raw) {
			t.Errorf("customEmojiRe matches %q, which is not a custom emoji code", raw)
		}
	}
}

func TestParseEmojiToken(t *testing.T) {
	for _, raw := range []string{"<:gaze:1>", "<a:party_boom:123456789012345678>"} {
		if _, ok := parseEmojiToken(raw); !ok {
			t.Errorf("%q: should parse", raw)
		}
	}
	for _, raw := range []string{"", "gaze", ":gaze:1>", "<::1>", "<gaze:1>", "<:gaze:>"} {
		if _, ok := parseEmojiToken(raw); ok {
			t.Errorf("%q: should not parse", raw)
		}
	}
	if got, ok := parseEmojiToken("<:gaze:1>"); !ok || got.Name != "gaze" || got.ID != "1" || got.Animated {
		t.Errorf("parseEmojiToken gave %+v (ok %v)", got, ok)
	}
	if got, _ := parseEmojiToken("<a:x:2>"); !got.Animated {
		t.Error("the a: prefix must set Animated")
	}
}
