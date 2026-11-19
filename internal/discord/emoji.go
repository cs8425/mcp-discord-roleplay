package discord

// Stickers and custom emoji: two ways a message can carry its content in
// a picture that the text never describes.
//
// A sticker arrives with an empty `content`: everything worth knowing is in
// `sticker_items`, which we ignored until now, so the agent saw a message with no
// text and no attachment at all. Custom emoji are the same problem in a smaller
// package, and a far more common one: they sit in the middle of sentences all the
// time. Discord sends no attachment for `<:gaze:123>` (the client renders it from
// the id), so what reached the agent was a code it cannot look up. And the name
// inside that code is no help either: people upload
// under "emoji_23", or under nothing but the id of the emoji they scraped, so the
// name is often a label the author never chose. The picture is the message.
//
// Both therefore become attachments we synthesize, pointing at the CDN file that
// belongs to the id. Two properties make that cheap and safe: the URL is built
// from a snowflake id whose bytes Discord never replaces (renaming or re-uploading
// produces a new id), and no REST call is needed, because the CDN is a public
// static host. internal/media keeps an in-memory cache of exactly these files.
//
// Each one leaves a handle behind in the text so the picture and the word it came
// from stay matched up: `[sticker:gaze]`, and for a custom emoji
// `<:gaze:img@1>`. What that number MEANS is settled later, and only later can it
// be settled: here at ingress the only thing knowable is the position inside this
// message's `attachments` array (uploads first, then stickers, then emoji), while
// the agent reads the number as an address into the poll result's content blocks,
// which depends on how many attachments came before it IN THAT ANSWER -- a fact
// that does not exist until the queue is drained. So normalize writes a
// provisional handle, records it on the attachment (`Placeholder`/`Raw`), and
// poll.relabelPlaceholders rewrites it into the one number both sides agree on:
// `img@N` where N is exactly that attachment's `content_index`, or -- when no
// bytes came with it -- the original Discord code, because a pointer to a block
// that was never emitted is worse than no pointer at all.

import (
	"regexp"
	"strconv"
	"strings"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"

	"github.com/bwmarrin/discordgo"
)

const (
	// The CDN paths are public and need no authorization header. They are named
	// purely by id, which is why the file behind one can be cached forever.
	cdnEmojiBase   = "https://cdn.discordapp.com/emojis/"
	cdnStickerBase = "https://cdn.discordapp.com/stickers/"

	// maxEmojiPerMessage caps how many distinct emoji of one message we fetch. The
	// question we are answering is "what does this message look like", and eight
	// pictures answer it; a message waving a dozen different emoji does not need
	// twelve downloads. The ones over the cap keep their raw code in the text. It is
	// a constant and not a flag on purpose: per message it is one number we can pick
	// sensibly for everyone, and the caller who outgrows it will say so.
	maxEmojiPerMessage = 8
)

// customEmojiRe matches <:name:id> and <a:name:id>. It is deliberately wider than
// Discord's own grammar (names are 2-50 chars of \w there, ids are snowflakes):
// this pattern is only the cheap "is there any emoji in this message" gate in
// front of parseEmojiToken, and a gate stricter than the parser silently vetoes
// it -- a code nobody renders is content lost, which is the very failure this
// feature exists to fix. parseEmojiToken stays the authority on what a code means
// (TestEmojiRegexMatchesWhatTheParserAccepts pins the two together).
var customEmojiRe = regexp.MustCompile(`<a?:[0-9A-Za-z_]+:\d+>`)

// emojiToken is one parsed custom emoji code.
type emojiToken struct {
	Animated bool
	Name     string
	ID       string
}

// parseEmojiToken reads "<:name:id>" or "<a:name:id>". It is deliberately
// forgiving about the name (LastIndex, not Cut) because the id is the only part
// we build a URL from.
func parseEmojiToken(raw string) (emojiToken, bool) {
	if len(raw) < 5 || raw[0] != '<' || raw[len(raw)-1] != '>' {
		return emojiToken{}, false
	}
	s := raw[1 : len(raw)-1]
	if len(s) < 3 {
		return emojiToken{}, false
	}
	var t emojiToken
	switch {
	case strings.HasPrefix(s, "a:"):
		t.Animated, s = true, s[2:]
	case strings.HasPrefix(s, ":"):
		s = s[1:]
	default:
		return emojiToken{}, false
	}
	i := strings.LastIndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return emojiToken{}, false
	}
	t.Name, t.ID = s[:i], s[i+1:]
	return t, t.ID != ""
}

// stickerFormat maps Discord's format_type onto two extensions and a MIME type:
// the one in the filename the agent sees, and the one the sticker CDN actually
// serves. They are the same everywhere except APNG.
//
// APNG is the trap we walked into live: cdn.discordapp.com has no
// /stickers/<id>.apng endpoint — it answers 404 — while the same id at .png is
// those very APNG bytes (and the same id at .gif answers 415, so the CDN checks
// an extension list rather than transcoding anything). So we fetch .png but name
// the attachment .apng.png: the two-part name keeps both facts, that the bytes
// are an animated PNG and that they arrived under a .png, and a saved sticker
// cannot be mistaken for a still image.
//
// Declaring image/png rather than image/apng is deliberate: an APNG file is a PNG
// file with extra chunks, which is what the model APIs accept — at the cost of
// the animation, which reads as the first frame. Declaring image/apng instead
// would have a real chance of getting the whole tool result rejected over one
// sticker. Lottie has no picture to show, so it stays the JSON it is and
// ClassifyMedia files it under "other".
func stickerFormat(formatType int) (fileExt, urlExt, mime string) {
	switch formatType {
	case 2:
		return ".apng.png", ".png", "image/png"
	case 3:
		return ".json", ".json", "application/json"
	case 4:
		return ".gif", ".gif", "image/gif"
	default: // 1, and anything Discord invents next: the PNG rendition is the safe guess
		return ".png", ".png", "image/png"
	}
}

// appendNote puts a placeholder at the end of a message's text without leaving a
// leading or doubled space when there was nothing there to begin with.
func appendNote(text, note string) string {
	if strings.TrimSpace(text) == "" {
		return note
	}
	return text + " " + note
}

// addStickers turns a message's stickers into attachments plus a word of their
// own in the text. Discord sends up to three per message.
//
// From here on a sticker is an image with a name, and it follows the sticker
// policy (--media-sticker, which overrides --media-image because that one is set
// for photographs). Where the name is worthless the [sticker:...] note in the
// text still says which attachment belongs to it.
func addStickers(ev *bus.Event, items []*discordgo.StickerItem) {
	for _, st := range items {
		if st == nil || st.ID == "" {
			continue
		}
		name := strings.TrimSpace(st.Name)
		if name == "" {
			name = st.ID
		}
		fileExt, urlExt, mime := stickerFormat(int(st.FormatType))
		file := name + fileExt
		ev.Attachments = append(ev.Attachments, bus.Attachment{
			ID:          st.ID,
			Filename:    file,
			ContentType: mime,
			Kind:        bus.ClassifyMedia(file, mime),
			URL:         cdnStickerBase + st.ID + urlExt,
			Origin:      bus.OriginSticker,
		})
		ev.Text = appendNote(ev.Text, "[sticker:"+name+"]")
	}
}

// addEmoji gives a message's custom emoji their pictures and rewrites the codes in
// its text to point at them.
//
// It runs on every message that contains a custom emoji, not only on ones made
// entirely of them: an emoji in the middle of a sentence is the common case, and
// the code there is exactly as unreadable as when it stands alone. Everything else
// in the text is left as it was typed, including the whitespace between codes, so
// the pictures land where the words were.
//
// Distinct emoji are what gets capped, not occurrences: the same emoji twice is one
// download and one attachment, and both placeholders name the same position --
// which is also how the agent learns the emoji came back again.
//
// The rewrite is the whole point of the feature, so it happens here rather than in
// the media layer, which never edits text. It also means --media-emoji=text costs
// nothing: no attachment is created, so nothing is fetched.
func addEmoji(ev *bus.Event, mode string) {
	if mode != config.EmojiInline || !customEmojiRe.MatchString(ev.Text) {
		return
	}
	seen := make(map[string]int, maxEmojiPerMessage)
	var n int
	ev.Text = customEmojiRe.ReplaceAllStringFunc(ev.Text, func(raw string) string {
		t, ok := parseEmojiToken(raw)
		if !ok {
			return raw
		}
		if idx, have := seen[t.ID]; have {
			return t.render(idx) // second appearance: same picture, same index
		}
		if n >= maxEmojiPerMessage {
			return raw // over the cap: honest code, no picture
		}
		ext, mime := ".png", "image/png"
		if t.Animated {
			ext, mime = ".gif", "image/gif"
		}
		idx := len(ev.Attachments) + 1
		handle := t.render(idx)
		seen[t.ID] = idx
		n++
		ev.Attachments = append(ev.Attachments, bus.Attachment{
			ID:          t.ID,
			Filename:    t.ID + ext,
			ContentType: mime,
			Kind:        bus.KindImage,
			URL:         cdnEmojiBase + t.ID + ext,
			Origin:      bus.OriginEmoji,
			Placeholder: handle,
			Raw:         raw,
		})
		return handle
	})
}

// render writes the provisional placeholder form of the token: the code as Discord
// wrote it, with the position of its picture in place of the id. poll rewrites the
// number once it knows the real content block (see relabelPlaceholders).
func (t emojiToken) render(idx int) string {
	lead := ":"
	if t.Animated {
		lead = "a:"
	}
	return "<" + lead + t.Name + ":img@" + strconv.Itoa(idx) + ">"
}
