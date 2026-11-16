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

// Intents is the minimal gateway subscription for this server:
// guild list (so channels resolve), messages in guilds we are in, DMs, and
// message content. Message Content is a PRIVILEGED intent that must also be
// switched on in the Discord Developer Portal, otherwise Content is "".
func Intents() discordgo.Intent {
	return discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent
}

// IntentsList is the same thing rendered for humans.
func IntentsList() []string {
	return []string{"Guilds", "GuildMessages", "DirectMessages", "MessageContent"}
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
	sess.Identify.Intents = Intents()
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
		"intents", strings.Join(IntentsList(), ","),
		"token", c.cfg.RedactedToken())

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

	ev := c.normalize(s, m.Message)

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
		// whoever is asking.
		Target: "channel:" + m.ChannelID,
	}
	ev.Time = tz.Format(ev.Ts)

	// author: webhook messages can carry a nil author
	if m.Author != nil {
		ev.Author = bus.Author{
			ID:   m.Author.ID,
			Name: m.Author.DisplayName(),
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
	now := time.Now()
	// System notices go to every queue but are never delivered to a caller: a
	// poll consumes and discards them so they cannot hold a slot forever.
	c.hub.PublishAll(&bus.Event{
		Kind: bus.KindSystem,
		Ts:   now,
		Time: tz.Format(now),
		Text: text,
	})
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
		c.log.Info("discordgo", "msg", msg)
	default:
		c.log.Debug("discordgo", "msg", msg)
	}
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
