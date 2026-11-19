// Package discord owns the single discordgo session (one gateway connection
// per bot token — two processes with the same token would fight over the
// gateway session) and turns raw gateway events into normalized bus.Event
// values.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/router"
	"mcp-discord-roleplay/internal/tz"
)

// Intents is the minimal gateway subscription for this server: guild list (so
// channels resolve), messages in guilds we are in, and DMs.
//
// withContent adds Message Content, the one PRIVILEGED intent we use. It has to
// be switched on in the Discord Developer Portal as well. Whether declaring it
// changes anything is not settled from here: the app's Message Content setting
// in the Portal is what decides, and we have watched text arrive with the bit
// undeclared and text withheld with it declared (docs/DESIGN.md §7 keeps both
// observations). So pass false to mean "I do not want a privileged bit", not
// "my messages will come back empty" -- the old wording promised that, and it
// also claimed attachments survive either way, which the Portal experiment
// disproved: when the setting is off, content, embeds, attachments and
// components go empty together, on the REST paths too.
func Intents(withContent bool) discordgo.Intent {
	i := discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages
	if withContent {
		i |= discordgo.IntentsMessageContent
	}
	return i
}

// IntentsList is the same thing rendered for humans.
func IntentsList(withContent bool) []string {
	l := []string{"Guilds", "GuildMessages", "DirectMessages"}
	if withContent {
		l = append(l, "MessageContent")
	}
	return l
}

// Snapshot is the observable gateway state, handed to discord_status.
type Snapshot struct {
	Started       bool      `json:"started"`
	Connected     bool      `json:"connected"`
	Ready         bool      `json:"ready"`
	ReadyAt       time.Time `json:"-"`
	ReadyAtS      string    `json:"ready_at,omitempty"`
	LastEventAt   time.Time `json:"-"`
	LastEventAgoS float64   `json:"last_event_age_s"`
	ReadyCount    int       `json:"ready_count"`
	Resumes       int       `json:"resumes"`
	Disconnects   int       `json:"disconnects"`
	Reconnects    int       `json:"reconnect_attempts"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"-"`
	LastErrorAgoS float64   `json:"last_error_age_s,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	Gateway       string    `json:"gateway,omitempty"`
	SelfID        string    `json:"self_id,omitempty"`
	SelfName      string    `json:"self_name,omitempty"`
	SelfTag       string    `json:"self_tag,omitempty"`
	HeartbeatAgoS float64   `json:"heartbeat_age_s,omitempty"`
}

// Client wraps a discordgo session.
type Client struct {
	cfg *config.Config
	log *slog.Logger
	hub *bus.Hub
	rt  *router.Router

	mu   sync.RWMutex
	sess *discordgo.Session
	snap Snapshot

	startedAt time.Time
	closing   atomic.Bool

	// --- outbound sending (M1). See send.go for the policy these implement. ---
	lim *sendLimiter // sliding budget, per-channel gap, circuit breaker
	dm  sync.Map     // recipient user id -> DM channel id (stable, never evicted)

	// Test seams. They stay nil in production and exist so the rate-limit, retry
	// and rewind behaviour can be proven without a network: the Go sandbox in this
	// project has no compiler, so the logic must be testable by the operator.
	post     poster                                           // nil = ChannelMessageSendComplex
	dmCreate channelPoster                                    // nil = UserChannelCreate
	sleep    func(ctx context.Context, d time.Duration) error // nil = real timer, ctx-aware

	// read is the seam for the M3 REST read path (read.go): history, get, one
	// message, and the two listing calls channels needs. nil means the live
	// session's own calls, which is what production does.
	read *readSeam

	// hooks is the door the tools package uses to answer "what did Discord say"
	// without a network or a gateway session. It speaks bus.Event, not discordgo,
	// because that is the language both sides already share. Zero in production.
	hooks ReadHooks
}

// New builds a client; nothing is connected until Start. Inbound events are
// fanned out through hub to the per-agent queues router.Audience selects.
func New(cfg *config.Config, log *slog.Logger, hub *bus.Hub, rt *router.Router) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{cfg: cfg, log: log, hub: hub, rt: rt, lim: newSendLimiter(cfg)}
}

// Start creates the session, installs handlers and opens the gateway.
// It returns once the handshake has been initiated; readiness arrives
// asynchronously through the Ready event.
func (c *Client) Start(ctx context.Context) error {
	if c.cfg.Token == "" {
		return errors.New("no bot token configured")
	}
	sess, err := discordgo.New(c.cfg.Token)
	if err != nil {
		// discordgo.New only fails on a malformed token; never echo the token.
		return fmt.Errorf("discord session: %w", err)
	}
	sess.Identify.Intents = Intents(c.cfg.MessageContentEnabled())
	sess.LogLevel = dgoLogLevel(c.cfg.LogLevel)
	sess.ShouldReconnectOnError = true
	// 429 must reach us as an error. With the library's retry switched on, a rate
	// limit is swallowed in restapi.go (time.Sleep on retry_after, then the same
	// bucket again, ignoring any context), so our breaker would never see a hit and
	// a tool call could block for an unbounded time. We keep MaxRestRetries for the
	// one case that still uses it: HTTP 502 (restapi.go:270-278).
	sess.ShouldRetryOnRateLimit = false
	sess.MaxRestRetries = 2
	// One process, one gateway: keep the default single shard explicit.
	sess.ShardID = 0
	sess.ShardCount = 1

	// discordgo v0.29.0: AddHandler returns an *unregister* func, not an error.
	// A handler with the wrong shape is dropped with one LogError line and then
	// silently never fires, so the shape is pinned to typed vars and verified by
	// the compiler instead of at runtime.
	var (
		onReady         func(*discordgo.Session, *discordgo.Ready)         = c.onReady
		onResumed       func(*discordgo.Session, *discordgo.Resumed)       = c.onResumed
		onDisconnect    func(*discordgo.Session, *discordgo.Disconnect)    = c.onDisconnect
		onMessageCreate func(*discordgo.Session, *discordgo.MessageCreate) = c.onMessageCreate
		onChannelCreate func(*discordgo.Session, *discordgo.ChannelCreate) = c.onChannelCreate
	)

	discordgo.Logger = c.gatewayLog

	for _, h := range []any{onReady, onResumed, onDisconnect, onMessageCreate, onChannelCreate} {
		sess.AddHandler(h)
	}

	if err := sess.Open(); err != nil {
		return fmt.Errorf("gateway open: %w", err)
	}

	c.mu.Lock()
	c.sess = sess
	c.startedAt = time.Now()
	c.snap.Started = true
	c.snap.Gateway = "gateway.discord.gg"
	c.mu.Unlock()

	c.log.Info("gateway connecting",
		"agent", c.cfg.AgentID,
		"intents", strings.Join(IntentsList(c.cfg.MessageContentEnabled()), ","),
		"token", c.cfg.RedactedToken())
	if !c.cfg.MessageContentEnabled() {
		// A fact about OURS, not about the gateway: live shows Discord decides
		// whether `text` comes through by the app's Portal setting, not by what we
		// declare -- with the toggle on, a guild message naming nobody still arrived
		// with text; with it off, only @-mentions and DMs did. So this warns about
		// the declaration we skipped and stops promising an emptiness we do not own.
		c.log.Warn("message content intent is off: we do not declare the privileged Message Content bit at IDENTIFY; " +
			"'text' may still arrive -- that is decided by the app's Message Content setting in the Developer Portal, " +
			"observed both ways (see docs/DESIGN.md §7)")
	}

	// v0.29.0 keeps the URL of the *connected* socket private (Ready only gained
	// ResumeGatewayURL in later releases), so ask the public API once, off the
	// hot path. Cosmetic if it fails - the default above stays.
	go func() {
		g, err := sess.Gateway()
		if err != nil {
			c.log.Debug("gateway url lookup failed", "err", err.Error())
			return
		}
		c.mu.Lock()
		c.snap.Gateway = hostOf(g)
		c.mu.Unlock()
	}()

	go c.watchdog(ctx)
	return nil
}

// Close shuts the gateway down. Safe to call more than once.
func (c *Client) Close() error {
	if !c.closing.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.RLock()
	sess := c.sess
	c.mu.RUnlock()
	if sess == nil {
		return nil
	}
	// discordgo's Close() sends a close frame, waits 1s, then closes the socket.
	err := sess.Close()
	c.mu.Lock()
	c.snap.Connected = false
	c.snap.Ready = false
	c.mu.Unlock()
	return err
}

// Session exposes the underlying discordgo session (nil before Start).
// Callers must treat it as shared state and never close it themselves.
func (c *Client) Session() *discordgo.Session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sess
}

// Connected reports gateway readiness (READY seen and not closed).
func (c *Client) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap.Connected && c.snap.Ready
}

// SelfID is the bot's user id; empty before Ready.
func (c *Client) SelfID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap.SelfID
}

// Snapshot returns a copy of the gateway state for the status tool.
func (c *Client) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.snap
	s.ReadyAtS = tz.Format(s.ReadyAt)
	s.LastEventAgoS = tz.AgeS(s.LastEventAt)
	s.LastErrorAgoS = tz.AgeS(s.LastErrorAt)
	if s.Started && c.sess != nil {
		// LastHeartbeatAck is a public field discordgo writes from its own
		// goroutine; reading it unsynchronised is what the library expects.
		s.HeartbeatAgoS = tz.AgeS(c.sess.LastHeartbeatAck)
	}
	return s
}

// ---------------------------------------------------------------------------
// gateway handlers
// ---------------------------------------------------------------------------

func (c *Client) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	self, name, tag := "", "", ""
	if r != nil && r.User != nil {
		self = r.User.ID
		name = r.User.DisplayName()
		tag = r.User.String()
	}
	c.mu.Lock()
	c.snap.Connected = true
	c.snap.Ready = true
	c.snap.ReadyAt = time.Now()
	c.snap.LastEventAt = time.Now()
	c.snap.ReadyCount++
	if r != nil {
		c.snap.SessionID = r.SessionID
	}
	c.snap.SelfID, c.snap.SelfName, c.snap.SelfTag = self, name, tag
	first := c.snap.ReadyCount == 1
	guilds, privs := 0, 0
	if r != nil {
		guilds, privs = len(r.Guilds), len(r.PrivateChannels)
	}
	c.mu.Unlock()

	c.log.Info("gateway ready",
		"self", tag, "self_id", self,
		"session", c.Snapshot().SessionID,
		"guilds", guilds, "private_channels", privs,
		"first", first)

	c.publishSystem(fmt.Sprintf("gateway ready as %s", tag))
}

func (c *Client) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	c.mu.Lock()
	c.snap.Connected = true
	c.snap.Ready = true
	c.snap.LastEventAt = time.Now()
	c.snap.Resumes++
	c.mu.Unlock()
	c.log.Info("gateway session resumed")
	c.publishSystem("gateway session resumed")
}

// onDisconnect fires when the gateway socket is torn down, including the
// graceful Op7 (server reorder) / Op9 (invalid session) paths.
func (c *Client) onDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	c.mu.Lock()
	c.snap.Connected = false
	c.snap.Ready = false
	c.snap.LastEventAt = time.Now()
	if !c.closing.Load() {
		c.snap.Disconnects++
	}
	c.mu.Unlock()
	if !c.closing.Load() {
		c.log.Warn("gateway disconnected; discordgo will reconnect")
		c.publishSystem("gateway disconnected; reconnecting")
	}
}

func (c *Client) onChannelCreate(_ *discordgo.Session, ch *discordgo.ChannelCreate) {
	if ch == nil || ch.Channel == nil {
		return
	}
	c.mu.Lock()
	c.snap.LastEventAt = time.Now()
	c.mu.Unlock()
	if ch.Type == discordgo.ChannelTypeDM || ch.Type == discordgo.ChannelTypeGroupDM {
		c.log.Debug("channel created", "id", ch.ID, "type", int(ch.Type))
	}
}

func (c *Client) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m == nil || m.Message == nil {
		return
	}
	c.mu.Lock()
	c.snap.LastEventAt = time.Now()
	c.mu.Unlock()

	// The member object riding on this event is the cheapest nickname we will
	// ever get for this person, and the read path lives off it. Before normalize,
	// because the fact is about the author, not about whether anybody here is
	// allowed to read this message.
	c.rememberMember(s, m.Message)

	ev := c.normalize(s, m.Message)

	// Our own echo never enters a queue. The caller already has this message
	// (discord_send handed it over the moment we wrote it), so a copy here is a
	// fact re-read, not news. It used to be queued and then skipped by every
	// default poll, which left it sitting there counted as unread until the ring
	// wrapped around it; that was the whole of the self-message pollution problem.
	if ev.Author.Self {
		if c.hub != nil {
			c.hub.NoteSelfEcho()
		}
		c.log.Debug("self echo, not queued",
			"channel", ev.ChannelID, "message", ev.MessageID,
			"runes", len([]rune(ev.Text)), "attachments", len(ev.Attachments))
		return
	}

	// Who is allowed to read this? The event is then stored by reference in each
	// of those agents' queues, so nothing may touch ev below this line.
	aud := c.rt.Audience(ev.GuildID, ev.ChannelID, ev.DM, recipientIDs(ev))
	if len(aud) == 0 {
		c.log.Debug("event has no audience, dropped",
			"channel", ev.ChannelID, "dm", ev.DM, "author", ev.Author.ID)
		if c.hub != nil {
			c.hub.NoteSkipped()
		}
		return
	}
	if ev.ChannelName == "" {
		ev.ChannelName = aud[0].Name
	}

	c.hub.Fanout(ev, agentIDsOf(aud))
	c.log.Debug("event queued",
		"agents", len(aud), "target", ev.Target, "author", ev.Author.Name,
		"runes", len([]rune(ev.Text)), "attachments", len(ev.Attachments))
}

func agentIDsOf(aud []router.AudienceEntry) []string {
	out := make([]string, 0, len(aud))
	for _, a := range aud {
		out = append(out, a.Agent)
	}
	return out
}

// rememberMember puts the guild member that rides on MESSAGE_CREATE into the
// state cache, which discordgo does not do for itself: its onState handler for a
// message only calls MessageAdd (state.go, `case *MessageCreate`), so the partial
// member object a message carries is thrown away. That leaves GUILD_CREATE as the
// cache's only member source -- which Discord omits for guilds above
// large_threshold, and which we can never refresh, because we do not subscribe to
// GUILD_MEMBERS. Feeding it here is what makes `history` able to name a person the
// way `poll` already did, at the cost of a map write.
//
// Only a NON-EMPTY nick is written. The member object is partial and Go cannot
// tell "the field was not sent" from "the nickname was cleared" -- both land as ""
// -- while memberAdd REPLACES the stored member (`*m = *member`, keeping only
// JoinedAt). Writing an empty one would let a single message erase knowledge
// GUILD_CREATE paid for. The identity fields are filled in from the message for
// the same reason: memberAdd dereferences member.User.ID, and the member object
// of an event is not guaranteed to carry either guild_id or user.
//
// This runs regardless of --nick-lookup: keeping the cache warm costs a map write,
// so turning the read-side lookup back on needs no restart paying for it again.
func (c *Client) rememberMember(s *discordgo.Session, m *discordgo.Message) {
	if s == nil || s.State == nil || m == nil || m.Member == nil || m.Member.Nick == "" {
		return
	}
	guildID := m.GuildID
	if guildID == "" {
		guildID = m.Member.GuildID
	}
	if guildID == "" {
		return // nothing to key on, and a DM carries no member anyway
	}
	user := m.Member.User
	if user == nil {
		user = m.Author
	}
	if user == nil || user.ID == "" {
		return
	}
	mem := *m.Member // a copy: we hand discordgo a pointer it will keep
	mem.GuildID = guildID
	mem.User = user
	if err := s.State.MemberAdd(&mem); err != nil && err != discordgo.ErrStateNotFound {
		c.log.Debug("member cache write failed", "guild", guildID, "author", user.ID, "err", err)
	}
}

// normalize converts a discordgo message into a bus.Event. Only the in-memory
// state cache is consulted — never REST (a blocking call in the gateway read
// loop would delay heartbeats).
func (c *Client) normalize(s *discordgo.Session, m *discordgo.Message) *bus.Event {
	selfID := c.SelfID()

	ev := &bus.Event{
		Kind:      bus.KindMessage,
		Ts:        messageTime(m),
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		GuildID:   m.GuildID,
		DM:        m.GuildID == "",
		Text:      m.Content,
		// Canonical, agent-independent label: aliases differ per agent, so the
		// shared event carries only the id form and poll resolves the alias for
		// whoever is asking. A DM is overridden to "dm:<peer>" a few lines below,
		// because that is the spelling routing and discord_send both speak.
		Target: "channel:" + m.ChannelID,
	}
	ev.Time = tz.Format(ev.Ts)

	// author: webhook messages can carry a nil author
	if m.Author != nil {
		// Discord's guild display name is member.nick; only when there is no
		// nickname does it fall back to global_name/username, which is all
		// DisplayName() knows about. The MESSAGE_CREATE member object is partial,
		// so an empty nick means "no nickname set" and DisplayName is then right;
		// DMs carry no member at all and always take that path.
		name := m.Author.DisplayName()
		if m.Member != nil && m.Member.Nick != "" {
			name = m.Member.Nick
		}
		ev.Author = bus.Author{
			ID:   m.Author.ID,
			Name: name,
			Tag:  m.Author.String(),
			Bot:  m.Author.Bot,
			Self: m.Author.ID != "" && m.Author.ID == selfID,
		}
	} else if m.WebhookID != "" {
		ev.Author = bus.Author{ID: m.WebhookID, Name: "webhook", Bot: true}
	} else {
		ev.Author = bus.Author{ID: "?", Name: "?"}
	}

	// channel name and the DM peer come from the state cache
	if ch := c.lookupChannel(s, m.ChannelID); ch != nil {
		ev.ChannelName = ch.Name
		if ev.DM {
			if peer := dmPeer(ch, selfID); peer != nil {
				ev.Extra = setExtra(ev.Extra, "peer", peer.ID)
				if name := peer.DisplayName(); name != "" {
					ev.ChannelName = name
				}
			}
		}
	}

	// A DM is addressed by its PEER, not by the DM channel id. Routing writes a
	// DM target as "dm:<user-snowflake>" (a DM's channel id is per-conversation
	// noise nobody can guess), so putting that same string on the event means the
	// address poll prints is always one you can hand straight back to
	// discord_send — for a target listed in routing.json AND for a stranger whose
	// DM we opened the moment ago. The state cache is not reliable here: the
	// first message from a stranger can arrive before CHANNEL_CREATE lands, and a
	// DM reached through a mutual guild we are not in is never in it at all, so
	// when it misses we take the author. That is not a fabricated fact: in a 1:1
	// DM the author IS the peer; in a group DM it is one of them, and "dm:<id>"
	// routing matches on any participant anyway. A webhook's id is not a user, so
	// those events (rare in DMs) get the webhook id as peer and are still
	// sendable to the channel; "?" (no author, no webhook) invents nothing.
	if ev.DM {
		if ev.Extra["peer"] == "" && ev.Author.ID != "" && ev.Author.ID != "?" {
			ev.Extra = setExtra(ev.Extra, "peer", ev.Author.ID)
		}
		if peer := ev.Extra["peer"]; peer != "" {
			ev.Target = "dm:" + peer
		}
	}

	if runes := []rune(ev.Text); len(runes) > c.cfg.TextMaxRunes {
		ev.Text = string(runes[:c.cfg.TextMaxRunes])
		ev.Truncated = true
	}

	for _, u := range m.Mentions {
		if u != nil && u.ID == selfID && selfID != "" {
			ev.MentionsSelf = true
			break
		}
	}
	if !ev.MentionsSelf && selfID != "" && strings.Contains(m.Content, "<@"+selfID+">") {
		ev.MentionsSelf = true
	}

	for _, a := range m.Attachments {
		if a == nil {
			continue
		}
		kind := bus.ClassifyMedia(a.Filename, a.ContentType)
		ev.Attachments = append(ev.Attachments, bus.Attachment{
			ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, Kind: kind,
			Size: a.Size, Width: a.Width, Height: a.Height, DurationSecs: a.DurationSecs,
			URL: a.URL, ProxyURL: a.ProxyURL,
		})
		// The media policy verdict (inline/save/skip) is NOT recorded here: this
		// event is shared between agents, and how one poll renders an attachment
		// says nothing about another. poll computes it per call.
	}

	// Stickers and custom emoji keep their content outside `content`, so they become
	// synthesized attachments with a placeholder left in the text (see emoji.go).
	// This runs after the truncation above on purpose: a handle like [sticker:gaze]
	// or the img@N an emoji code turns into is worth more than the last few runes of
	// a long message, and it must not be cut off from the attachment it names.
	addStickers(ev, m.StickerItems)
	addEmoji(ev, c.cfg.MediaEmoji)

	for _, e := range m.Embeds {
		if e == nil {
			continue
		}
		ev.Embeds = append(ev.Embeds, bus.Embed{Type: string(e.Type), Title: e.Title, Description: e.Description, URL: e.URL})
	}

	return ev
}

func (c *Client) lookupChannel(s *discordgo.Session, id string) *discordgo.Channel {
	if s == nil || s.State == nil || id == "" {
		return nil
	}
	ch, err := s.State.Channel(id)
	if err != nil || ch == nil {
		return nil
	}
	return ch
}

func dmPeer(ch *discordgo.Channel, selfID string) *discordgo.User {
	for _, u := range ch.Recipients {
		if u != nil && u.ID != selfID {
			return u
		}
	}
	return nil
}

func (c *Client) publishSystem(text string) {
	if c.hub == nil {
		return
	}
	// A lifecycle notice is a wake-up, not a message. It used to be pushed into
	// every queue, where it occupied a slot, counted as unread, could evict a real
	// message on a full ring, and was then consumed-and-discarded by the next poll
	// — which is four costs for a thing nobody reads. It now only nudges the
	// waiting polls (so a disconnect surfaces as reason=disconnected instead of a
	// timeout on a dead line); the text goes to the log, which is its audience.
	c.hub.WakeAll()
	c.log.Debug("system notice", "text", text)
}

// watchdog keeps a quiet socket visible in the logs (Discord pings every
// ~41s; a healthy client acks constantly).
func (c *Client) watchdog(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap := c.Snapshot()
			if snap.Ready && time.Since(snap.LastEventAt) > 5*time.Minute {
				c.log.Debug("gateway quiet",
					"since_last_event_s", int(time.Since(snap.LastEventAt).Seconds()))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// discordgo logging bridge
// ---------------------------------------------------------------------------

// gatewayLog funnels discordgo's internal logger into slog (stderr). discordgo
// exposes no richer reconnect telemetry, so this is also where "trying to
// reconnect to gateway" becomes a counter and an error string.
func (c *Client) gatewayLog(msgL, caller int, format string, a ...interface{}) {
	_ = caller
	msg := fmt.Sprintf(format, a...)
	switch msgL {
	case discordgo.LogError:
		c.noteError(msg)
		c.log.Error("discordgo", "msg", msg)
	case discordgo.LogWarning:
		if strings.Contains(msg, "reconnect") {
			c.noteReconnect()
		}
		c.noteError(msg)
		c.log.Warn("discordgo", "msg", msg)
	case discordgo.LogInformational:
		if strings.Contains(msg, "trying to reconnect to gateway") {
			c.noteReconnect()
			c.log.Warn("gateway reconnect attempt", "msg", msg)
			return
		}
		if strings.HasPrefix(msg, "First Packet:") {
			// discordgo %#v-dumps the whole first event, and RawData is the READY
			// payload - every guild we share with the bot, spelled out as byte
			// literals - so this single line runs to several KB. At info the head is
			// enough to see what arrived (Operation/Sequence/Type); the full dump is
			// only for -log-level=debug. It stays a logged line instead of being
			// dropped, so a message that merely happens to share this prefix loses
			// at most its tail.
			if c.log.Enabled(context.Background(), slog.LevelDebug) {
				c.log.Debug("discordgo", "msg", msg)
				return
			}
			c.log.Info("discordgo", "msg", headLog(msg, dgoFirstPacketHead))
			return
		}
		c.log.Info("discordgo", "msg", msg)
	default:
		c.log.Debug("discordgo", "msg", msg)
	}
}

// dgoFirstPacketHead is how much of discordgo's "First Packet" dump survives at
// info level. See gatewayLog.
const dgoFirstPacketHead = 160

// headLog keeps the first n bytes of s - never cutting inside a UTF-8 sequence
// - and reports how much was dropped.
func headLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n] + fmt.Sprintf("…(+%d bytes)", len(s)-n)
}

func (c *Client) noteError(msg string) {
	c.mu.Lock()
	c.snap.LastError = truncate(msg, 300)
	c.snap.LastErrorAt = time.Now()
	c.mu.Unlock()
}

func (c *Client) noteReconnect() {
	c.mu.Lock()
	c.snap.Reconnects++
	c.mu.Unlock()
}

func dgoLogLevel(level string) int {
	switch strings.ToLower(level) {
	case "debug":
		return discordgo.LogDebug
	case "info":
		return discordgo.LogInformational
	case "warn", "warning":
		return discordgo.LogWarning
	default:
		return discordgo.LogError
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func recipientIDs(ev *bus.Event) []string {
	var ids []string
	if v := ev.Extra["peer"]; v != "" {
		ids = append(ids, v)
	}
	if ev.Author.ID != "" {
		ids = append(ids, ev.Author.ID)
	}
	return ids
}

func messageTime(m *discordgo.Message) time.Time {
	if !m.Timestamp.IsZero() {
		return m.Timestamp
	}
	if t, ok := config.SnowflakeTime(m.ID); ok {
		return t
	}
	return time.Now()
}

func setExtra(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func hostOf(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}
