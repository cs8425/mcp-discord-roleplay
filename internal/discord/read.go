package discord

import (
	"context"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"mcp-discord-roleplay/internal/bus"
)

// ---------------------------------------------------------------------------
// Read path (M3): history, get, channels.
//
// These answer "what does Discord have", not "what arrived while we were
// listening": every one of them is a REST GET and none of them touches a
// per-agent queue. That is the whole point -- their answers include messages
// from BEFORE we connected and ones our admission gates dropped, which poll can
// never show. They are pure reads: idempotent, they consume nothing, and two
// calls in a row change nothing for the next poll.
//
// Three decisions live here because they are about REST, not about the gateway:
//
//  1. Errors reuse the send vocabulary (classify / SendError) but the send
//     CIRCUIT BREAKER is deliberately not shared. The breaker means "we cannot
//     write, stop knocking"; a failed read must not short a send, and a send
//     locked by a 429 must not blind history.
//  2. Every call carries OUR deadline (--read-timeout) via
//     discordgo.WithContext. discordgo's own default is 20s (discord.go, New():
//     Client: &http.Client{Timeout: 20s}) -- far too long for one tool call --
//     and we do NOT mutate the shared s.Client, because media has its own
//     client (media.go) and the WS has gorilla's, and a read has no business
//     changing either.
//  3. We pass WithRetryOnRatelimit(false). discordgo's session default is TRUE
//     (discord.go, New(): ShouldRetryOnRateLimit: true, MaxRestRetries: 3) and
//     its retry is a bare time.Sleep(RetryAfter) that does not know about our
//     context; leaving it on would make --read-timeout a suggestion. A read is
//     cheap to re-issue, so a 429 goes back to the caller as rate_limited and
//     the caller decides -- which also keeps this path honest about who owns
//     the retry.
//
// The payload mapping is NOT duplicated here: REST messages go through the very
// same normalize() the gateway uses, so a message looks identical whether it
// came from a queue or from history, and the tools layer keeps its single wire
// mapper. normalize is a pure mapper (it filters nothing) and consults only the
// in-memory state cache, so a channel we have never seen arrives with an empty
// name -- the label the caller already resolved fills that hole (see event()).
// ---------------------------------------------------------------------------

// defaultReadTimeout bounds one REST read when the config leaves --read-timeout
// unset. It is a per-call budget, not a per-tool-invocation one: `channels`
// lists guilds and then their channels, and each GET gets its own.
const defaultReadTimeout = 10 * time.Second

const (
	historyDefaultLimit = 20  // what a bare `history` call means
	historyMaxLimit     = 50  // Discord allows 100; we cap lower to hold the byte budget
	guildsPerPage       = 200 // Discord's own cap for /users/@me/guilds
)

// readSeam is the test seam for the REST reads: the four session calls this
// file makes, in the shapes this file wants. It stays nil in production.
type readSeam struct {
	messages      func(ctx context.Context, channelID string, limit int, before, after, around string) ([]*discordgo.Message, error)
	message       func(ctx context.Context, channelID, messageID string) (*discordgo.Message, error)
	guilds        func(ctx context.Context, limit int) ([]*discordgo.UserGuild, error)
	guildChannels func(ctx context.Context, guildID string) ([]*discordgo.Channel, error)
}

// ReadHooks is the same three questions the read tools ask, answerable by a
// caller OUTSIDE this package -- which in practice means the tools' tests. They
// do not want to fake discordgo (this package's own tests do that, and that is
// where the gateway/REST mapping is proven); they want to say "Discord answered
// like this" in the language both sides speak, bus.Event.
//
// A nil hook leaves the real REST path alone, and production runs with all three
// nil, so this door costs nothing there.
type ReadHooks struct {
	History      func(ctx context.Context, q HistoryQuery) ([]*bus.Event, error)
	Get          func(ctx context.Context, channelID, messageID string) (*bus.Event, error)
	ListChannels func(ctx context.Context, guildID string) ([]GuildInfo, error)
}

// SetReadHooks installs the hooks above. Test-only: no production path calls it.
func (c *Client) SetReadHooks(h ReadHooks) {
	if c != nil {
		c.hooks = h
	}
}

// reader returns the seam, wrapping the live session when there is none. A
// client that never got a session cannot read either, and says so in the same
// words send uses.
func (c *Client) reader() (*readSeam, *SendError) {
	if c.read != nil {
		return c.read, nil
	}
	sess := c.Session()
	if sess == nil {
		return nil, &SendError{Code: CodeNotConnected,
			Msg: "no gateway session: REST reads need a started client (bad token, or the gateway never accepted us)"}
	}
	opts := func(ctx context.Context) []discordgo.RequestOption {
		return []discordgo.RequestOption{
			discordgo.WithContext(ctx),
			discordgo.WithRetryOnRatelimit(false),
		}
	}
	return &readSeam{
		messages: func(ctx context.Context, ch string, limit int, before, after, around string) ([]*discordgo.Message, error) {
			return sess.ChannelMessages(ch, limit, before, after, around, opts(ctx)...)
		},
		message: func(ctx context.Context, ch, id string) (*discordgo.Message, error) {
			return sess.ChannelMessage(ch, id, opts(ctx)...)
		},
		guilds: func(ctx context.Context, limit int) ([]*discordgo.UserGuild, error) {
			return sess.UserGuilds(limit, "", "", false, opts(ctx)...)
		},
		guildChannels: func(ctx context.Context, g string) ([]*discordgo.Channel, error) {
			return sess.GuildChannels(g, opts(ctx)...)
		},
	}, nil
}

// readTimeout is --read-timeout with a sane floor, so a zero value in a test
// config does not mean "no deadline".
func (c *Client) readTimeout() time.Duration {
	if c.cfg != nil && c.cfg.ReadTimeout > 0 {
		return c.cfg.ReadTimeout
	}
	return defaultReadTimeout
}

// readCtx puts our deadline on the call. If the caller's context is already
// tighter, theirs wins: context.WithTimeout takes the earlier of the two.
func (c *Client) readCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.readTimeout())
}

// readError turns a REST failure into the shared error type. classify already
// knows discord's codes; the read-specific part is the wording, because
// classify's sentences are written for sending.
func (c *Client) readError(what string, err error, start time.Time) *SendError {
	se := classify(err)
	switch {
	case se.Code == CodeCancelled:
		se.Msg = what + " timed out after " + c.readTimeout().String()
	case se.Msg != "":
		se.Msg = what + ": " + se.Msg
	}
	c.log.Warn("rest "+what+" failed", "code", se.Code,
		"err", truncate(err.Error(), 200),
		"api_ms", time.Since(start).Milliseconds())
	return se
}

// HistoryQuery is one page of a channel's history. Before/After/Around are
// message ids and mutually exclusive in practice (discord takes the filters
// apart and applies at most one); we pass them through rather than inventing a
// policy, because the cursor that matters is the one the caller got last time.
type HistoryQuery struct {
	ChannelID string
	Name      string // a channel label we already resolved; "" means "figure it out"
	Limit     int
	Before    string
	After     string
	Around    string
}

// History returns one page of messages, OLDEST FIRST. Discord answers
// newest-first -- that is what a "before" cursor wants -- but a transcript
// reads the other way, so we flip once here and nothing upstream has to.
func (c *Client) History(ctx context.Context, q HistoryQuery) ([]*bus.Event, error) {
	if c.hooks.History != nil {
		return c.hooks.History(ctx, q)
	}
	r, se := c.reader()
	if se != nil {
		return nil, se
	}
	chID := strings.TrimSpace(q.ChannelID)
	if chID == "" {
		return nil, &SendError{Code: CodeBadRequest, Msg: "history needs a channel to read"}
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = historyDefaultLimit
	case limit > historyMaxLimit:
		limit = historyMaxLimit
	}

	ctx, cancel := c.readCtx(ctx)
	defer cancel()
	start := time.Now()
	msgs, err := r.messages(ctx, chID, limit, q.Before, q.After, q.Around)
	if err != nil {
		return nil, c.readError("history", err, start)
	}

	events := make([]*bus.Event, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		if ev := c.event(msgs[i], q.Name); ev != nil {
			events = append(events, ev)
		}
	}
	c.log.Debug("rest history", "channel", chID, "limit", limit,
		"before", q.Before, "after", q.After, "around", q.Around,
		"got", len(events), "api_ms", time.Since(start).Milliseconds())
	return events, nil
}

// GetOne reads a single message by id. It exists because a read of one message
// is the unit the tools layer needs for `get`, and because the single-message
// endpoint answers even when the message was never in our ring -- including its
// attachments, which `get` then hands to the media pipeline.
func (c *Client) GetOne(ctx context.Context, channelID, messageID, name string) (*bus.Event, error) {
	if c.hooks.Get != nil {
		return c.hooks.Get(ctx, channelID, messageID)
	}
	r, se := c.reader()
	if se != nil {
		return nil, se
	}
	chID := strings.TrimSpace(channelID)
	mID := strings.TrimSpace(messageID)
	if chID == "" || mID == "" {
		return nil, &SendError{Code: CodeBadRequest, Msg: "get needs both a channel and a message id"}
	}

	ctx, cancel := c.readCtx(ctx)
	defer cancel()
	start := time.Now()
	m, err := r.message(ctx, chID, mID)
	if err != nil {
		return nil, c.readError("get", err, start)
	}
	ev := c.event(m, name)
	if ev == nil {
		return nil, &SendError{Code: CodeAPIError, Msg: "discord returned no message"}
	}
	c.log.Debug("rest get", "channel", chID, "message", mID,
		"attachments", len(ev.Attachments), "api_ms", time.Since(start).Milliseconds())
	return ev, nil
}

// event turns one message -- gateway or REST -- into the bus.Event the rest of
// the server speaks. normalize is the single mapping; the only thing added here
// is the channel label, for channels the state cache has never seen.
func (c *Client) event(m *discordgo.Message, name string) *bus.Event {
	if m == nil {
		return nil
	}
	ev := c.normalize(c.Session(), m)
	if ev == nil {
		return nil
	}
	if ev.ChannelName == "" && name != "" {
		ev.ChannelName = name
	}
	return ev
}

// ChannelInfo and GuildInfo are our own words for what Discord lists. The tools
// layer must not have to import discordgo to print a directory, and rendering
// discordgo's int-based ChannelType with a cast would give a rune, not a name.
type ChannelInfo struct {
	ID       string
	Name     string
	Type     string
	Topic    string
	GuildID  string
	ParentID string
}

type GuildInfo struct {
	ID       string
	Name     string
	Channels []ChannelInfo
}

// ListChannels answers "what is out there": the guilds we are in and their
// channels. guildID narrows it to one guild, by id or by name (case
// insensitive). Two things are deliberately absent:
//
//   - DM channels. discordgo v0.29.0 has no wrapper for /users/@me/channels,
//     and which DMs we speak of is a routing question, not a Discord one: the
//     tools layer lists those from its own routing table.
//   - our ACL. What exists is Discord's business; what this agent may write to
//     is ours, and the two must not be blended into one number.
func (c *Client) ListChannels(ctx context.Context, guildID string) ([]GuildInfo, error) {
	if c.hooks.ListChannels != nil {
		return c.hooks.ListChannels(ctx, guildID)
	}
	r, se := c.reader()
	if se != nil {
		return nil, se
	}
	want := strings.TrimSpace(guildID)

	ctx, cancel := c.readCtx(ctx)
	defer cancel()
	start := time.Now()
	gs, err := r.guilds(ctx, guildsPerPage)
	if err != nil {
		return nil, c.readError("channels", err, start)
	}

	out := make([]GuildInfo, 0, len(gs))
	for _, g := range gs {
		if g == nil {
			continue
		}
		if want != "" && g.ID != want && !strings.EqualFold(g.Name, want) {
			continue
		}
		info := GuildInfo{ID: g.ID, Name: g.Name}
		chs, err := r.guildChannels(ctx, g.ID)
		if err != nil {
			// One guild we cannot list must not blank out the rest: the caller
			// asked for a directory, and a partial one is still worth reading.
			// The empty Channels list plus the log line is the whole signal.
			c.log.Warn("rest channels: guild skipped", "guild", g.ID, "name", g.Name,
				"err", truncate(err.Error(), 160))
			out = append(out, info)
			continue
		}
		info.Channels = make([]ChannelInfo, 0, len(chs))
		for _, ch := range chs {
			if ch == nil {
				continue
			}
			info.Channels = append(info.Channels, ChannelInfo{
				ID: ch.ID, Name: ch.Name, Type: ChannelTypeWord(ch.Type),
				Topic: ch.Topic, GuildID: ch.GuildID, ParentID: ch.ParentID,
			})
		}
		out = append(out, info)
	}
	c.log.Debug("rest channels", "guild", want, "guilds", len(out),
		"api_ms", time.Since(start).Milliseconds())
	return out, nil
}

// ChannelTypeWord renders a discordgo ChannelType as one stable word. These are
// the words the `channels` tool's type filter speaks, so they are API, not
// formatting: adding a kind means adding a case, never renaming an old one.
func ChannelTypeWord(t discordgo.ChannelType) string {
	switch t {
	case discordgo.ChannelTypeGuildText:
		return "text"
	case discordgo.ChannelTypeDM:
		return "dm"
	case discordgo.ChannelTypeGuildVoice:
		return "voice"
	case discordgo.ChannelTypeGroupDM:
		return "group-dm"
	case discordgo.ChannelTypeGuildCategory:
		return "category"
	case discordgo.ChannelTypeGuildNews:
		return "news"
	case discordgo.ChannelTypeGuildStore:
		return "store"
	case discordgo.ChannelTypeGuildNewsThread:
		return "news-thread"
	case discordgo.ChannelTypeGuildPublicThread:
		return "thread"
	case discordgo.ChannelTypeGuildPrivateThread:
		return "private-thread"
	case discordgo.ChannelTypeGuildStageVoice:
		return "stage"
	case discordgo.ChannelTypeGuildDirectory:
		return "directory"
	case discordgo.ChannelTypeGuildForum:
		return "forum"
	case discordgo.ChannelTypeGuildMedia:
		return "media"
	}
	return "other"
}
