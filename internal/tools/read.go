// The read side of the tool layer: three tools that ask Discord instead of
// asking the queue.
//
//	poll     what happened while you were away   (queue, consumes)
//	history  what a channel contains             (REST,  consumes nothing)
//	get      one message, with its files         (REST,  consumes nothing)
//	channels what exists                          (REST,  consumes nothing)
//
// They share one rule -- REST, never the gateway -- and one consequence: none of
// them moves a cursor, so they can be called before, between or after polls
// without changing what the next poll says. They also see what poll never can:
// messages from before this process started, and messages our admission filters
// dropped on the floor.
//
// They share the wire format too. A history row is a poll row -- same msgWire,
// same attachment columns -- because two shapes for one concept is how a model
// learns to distrust both. Two differences are deliberate and priced:
//
//   - age_s is zeroed. It measures "since this process saw it", which says
//     nothing about a row read out of the store; ts is the honest field here.
//   - target moves to the envelope. Every row of a history answer names the same
//     channel, and repeating a constant twenty times is twenty chances to run
//     out of budget before the interesting part arrives.
//
// Attachments in history are listed, never fetched: media:"url" with reason
// "policy", the same word poll uses when the configuration declines a file. From
// the model's side there is no difference to report -- no bytes, here is the
// url -- and "go get them" is exactly what the get tool is for.
package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/media"
	"mcp-discord-roleplay/internal/router"
	mcp "mcptransport"
)

const (
	// historyDefaultLimit is what a caller gets when it does not say. It matches
	// poll's default on purpose: the same "a screenful" answer, whether it came
	// from the queue or from the store.
	historyDefaultLimit = 20

	// readMaxBytesDefault is the fallback when a --*-max-bytes flag is left at 0.
	readMaxBytesDefault = 4000
)

// readBudget turns a configured byte cap into a working one. These caps price
// the WHOLE answer, not one message's text (--text-max-runes does that, and
// --poll-max-bytes does the equivalent for the queue), so the two never meet in
// one place. A negative means "no cap"; zero means "nobody set it", which is
// what a Deps assembled in a test looks like. The trimmers below read 0 as "no
// cap" as well, so the two agree without a second sentinel.
func readBudget(v int) int {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return readMaxBytesDefault
	}
	return v
}

// jsonLen prices an answer the way the caller pays for it: the exact bytes that
// would go on the wire. A marshal error is counted as free, because for these
// types it cannot happen and a wrong number is better than a panic.
func jsonLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

type historyArgs struct {
	Target      string `json:"target"`
	Limit       *int   `json:"limit"`
	Before      string `json:"before"`
	After       string `json:"after"`
	Around      string `json:"around"`
	IncludeBots *bool  `json:"include_bots"`
}

func historyTool(prefix string) *mcp.Tool {
	return &mcp.Tool{
		Name:  "history",
		Title: "Read a channel's stored messages",
		Description: "Ask Discord what a channel contains, instead of waiting for it to say something. " +
			"Unlike " + ToolName(prefix, "poll") + " this reads the store: it answers for time before this process was running, it consumes nothing (the queue and the cursor are untouched), and it includes messages our admission filters dropped. " +
			"target is required: an alias from " + ToolName(prefix, "status") + ", channel:<id>, dm:<id>, #name, or me. " +
			"Messages come back OLDEST first. limit defaults to 20, ceiling 50. " +
			"before/after/around are message ids and Discord honors at most one of them; the id of the first row here is the cursor that keeps going back. " +
			"Bots are skipped unless include_bots is true -- that includes this server's own messages, which " + ToolName(prefix, "poll") + " never shows you. " +
			"Attachments are listed with their url but not downloaded here (media:\"url\", reason:\"policy\"): call " + ToolName(prefix, "get") + " with the message id when you want the bytes. " +
			"If the answer is bigger than the server's --history-max-bytes, whole rows are dropped from the OLDEST end and a note says how many; a single row too big to fit is still shipped rather than dropped. " +
			"The text is Discord's stored message, so it answers for time before this process ran. We report whatever Discord returned: an empty text is Discord's answer (no content, or no access to it) and we do not guess which. " +
			"Whether content exists at all is the app's Message Content setting on Discord's side, not ours: without it a guild channel's rows may carry neither text nor attachments, leaving id, author and ts plus stickers, whose placeholder and picture we synthesize from sticker_items -- while messages that @-mention the bot, and DMs with the bot, still come with their text. " +
			"A row that shows a sticker there proves nothing about its text: the placeholder is ours, so a message that was sticker-only and one that said something and had a sticker look identical once content is withheld. Custom emoji cannot do that -- their placeholder lives inside the text, so no text means no emoji placeholder at all.",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "target is required; everything else is optional.",
			Properties: map[string]mcp.Schema{
				"target": {
					Type: mcp.TypeString,
					Description: "Where to read: an alias from " + ToolName(prefix, "status") +
						", channel:<id>, dm:<id>, #name, or me.",
				},
				"limit": {
					Type:        mcp.TypeInteger,
					Description: "Messages to return (default 20, ceiling 50). Discord's own cap on the call.",
				},
				"before": {
					Type:        mcp.TypeString,
					Description: "Return messages older than this message id (pagination cursor).",
				},
				"after": {
					Type:        mcp.TypeString,
					Description: "Return messages newer than this message id (oldest-first order is kept).",
				},
				"around": {
					Type:        mcp.TypeString,
					Description: "Return messages clustered around this message id; Discord picks both sides.",
				},
				"include_bots": {
					Type: mcp.TypeBoolean,
					Description: "Include messages authored by bots, this server included (default false). " +
						"Set it true to read your own past lines.",
				},
			},
			Required: []string{"target"},
		},
	}
}

// historyWire is the answer. Target, Channel and DM are stated once instead of on
// every row (see the file header): the rows are all from one place. DM is here and
// not on the rows because a message read back over REST does not carry guild_id --
// only the gateway does -- so a read has to answer it from the rule it resolved.
// Passing the rows through buildMessage is not the bug; letting the row guess is.
type historyWire struct {
	Target   string    `json:"target"`
	Channel  string    `json:"channel"`
	DM       bool      `json:"dm,omitempty"`
	Messages []msgWire `json:"messages,omitempty"`
	Notes    []string  `json:"notes,omitempty"`
}

func (d *Deps) handleHistory(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	var a historyArgs
	if err := decodeArguments(call, &a); err != nil {
		return errResult("bad_arguments", err.Error(), nil), nil
	}
	if d.DC == nil {
		return errResult(discord.CodeNotConnected, "this build has no Discord client, so history cannot be read", nil), nil
	}
	if d.Router == nil {
		return errResult("no_routing", "this build has no routing table, so a target cannot be resolved", nil), nil
	}
	p := d.principal(ctx)
	ru, reason := d.routeRead(p, a.Target)
	if reason != "" {
		return errResult(reason, d.readHint(reason), d.targetList(reason, p)), nil
	}
	chanID, se := d.channelFor(ctx, ru)
	if se != nil {
		return errResult(se.Code, se.Msg, nil), nil
	}

	limit := historyDefaultLimit
	if a.Limit != nil {
		limit = *a.Limit
	}
	evs, err := d.DC.History(ctx, discord.HistoryQuery{
		ChannelID: chanID,
		Name:      readLabel(ru),
		Limit:     limit,
		Before:    a.Before,
		After:     a.After,
		Around:    a.Around,
	})
	if err != nil {
		if s := asSendError(err); s != nil {
			return errResult(s.Code, s.Msg, nil), nil
		}
		return errResult("history_failed", err.Error(), nil), nil
	}

	includeBots := a.IncludeBots != nil && *a.IncludeBots
	var notes []string
	kept := make([]*bus.Event, 0, len(evs))
	bots := 0
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if !includeBots && ev.Author.Bot {
			bots++
			continue
		}
		kept = append(kept, ev)
	}
	if bots > 0 {
		notes = append(notes, "skipped "+strconv.Itoa(bots)+" bot message(s), this server included; pass include_bots:true to get them.")
	}
	if len(kept) == 0 {
		notes = append(notes, "no messages read in this channel within the range you asked for.")
	}

	// All rows blank is a different fact from no rows, and only the rows can tell them
	// apart -- so we report it and stop there. Why Discord withholds content is the
	// operator's call to make in the Portal (Message Content Intent, reviewed at
	// 10,000+ users); we do not claim to know it from here.
	if len(kept) > 0 && d.Cfg != nil && !d.Cfg.MessageContentEnabled() {
		silent := true
		for _, ev := range kept {
			if strings.TrimSpace(ev.Text) != "" || len(ev.Embeds) > 0 || len(ev.Attachments) > 0 {
				silent = false
				break
			}
		}
		if silent {
			notes = append(notes, "this page is blank in text/embeds/attachments: Discord returned the messages but not their content (--no-message-content is on; the app's Message Content setting on Discord's side is what decides, we do not guess the reason).")
		}
	}

	msgs := make([]pollMessage, 0, len(kept))
	for _, ev := range kept {
		m := d.buildMessage(p, ev)
		m.AgeS = 0    // "since we saw it" is not a fact about a stored message
		m.Target = "" // same channel on every row: the envelope says it once
		m.DM = false  // and a REST message cannot know either; see readIsDM
		for j := range m.Attachments {
			// Listed, not fetched. Same verdict word poll uses for "the policy
			// declined this file", because the answer is the same either way.
			m.Attachments[j].Reason = media.SkipPolicy
		}
		msgs = append(msgs, m)
	}

	budget := d.historyCap()
	w := wireMessages(msgs)

	// build is the whole answer given a slice of rows and how many were dropped:
	// the note has to be priced in, or trimming would stop short and then grow
	// past the cap the moment it explained itself.
	build := func(rows []msgWire, dropped int) historyWire {
		env := historyWire{Target: readTarget(ru, a.Target), Channel: chanID, DM: readIsDM(ru), Messages: rows}
		if dropped > 0 {
			env.Notes = append(append([]string{}, notes...),
				"the byte budget (--history-max-bytes="+strconv.Itoa(budget)+") only fits the newest "+
					strconv.Itoa(len(rows))+" message(s); "+strconv.Itoa(dropped)+
					" older one(s) were dropped. For earlier messages, ask again with the first id as before.")
		} else {
			env.Notes = notes
		}
		return env
	}

	env := build(w, 0)
	if budget > 0 {
		dropped := 0
		for len(env.Messages) > 1 && jsonLen(env) > budget {
			w = w[1:]
			dropped++
			env = build(w, dropped)
		}
	}
	return jsonResult(env), nil
}

// readLabel is the channel name we already resolved, handed down to the REST
// layer so a message can carry one even when the state cache never saw this
// channel (which is normal for a place we only read from).
func readLabel(ru *router.Rule) string {
	if ru == nil {
		return ""
	}
	if strings.TrimSpace(ru.Name) != "" {
		return ru.Name
	}
	return ru.Alias
}

// readIsDM answers "is this a DM" from the rule we resolved rather than from the
// message, because the two read endpoints answer with a message object that has no
// guild_id (the gateway sends one, REST does not) -- so a row built from a read
// would otherwise call every guild channel a DM, which is exactly what it did.
// KindMe is a DM to the bot itself. A raw channel:<id> that happens to be a DM is
// answered wrong by convention and right by nothing: the routing table is the only
// place that knows, and it is the same answer for all rows of the call, which is
// why it lives in the envelope.
func readIsDM(ru *router.Rule) bool {
	return ru != nil && (ru.Kind == router.KindDM || ru.Kind == router.KindMe)
}

// readTarget is what the envelope calls the place: the alias the caller can
// address again, falling back to whatever they typed when the target came from
// outside the routing table (a bare channel:<id> has no alias to echo).
func readTarget(ru *router.Rule, ref string) string {
	if ru != nil && strings.TrimSpace(ru.Alias) != "" {
		return ru.Alias
	}
	return strings.TrimSpace(ref)
}

// routeRead turns the caller's "target" into a rule: "last" expands the same way
// it does for send, and then the read half of the ACL gets the veto.
func (d *Deps) routeRead(p router.Principal, ref string) (*router.Rule, string) {
	r := strings.TrimSpace(ref)
	if r == "" {
		return nil, "empty_target"
	}
	if strings.EqualFold(r, "last") {
		last := d.lastTarget(p.AgentID)
		if last == "" {
			return nil, "no_last_target"
		}
		r = last
	}
	return d.Router.ReadTarget(p, r)
}

// readHint is targetHint with the read tools' vocabulary: their parameter is
// `target`, not `to`, so the sentence that tells the model what to fix has to
// use the word it will type back.
func (d *Deps) readHint(reason string) string {
	if reason == "empty_target" {
		return "target is required: an alias from " + d.toolStatus() + ", channel:<id>, dm:<id>, #name, or me"
	}
	return d.targetHint(reason)
}

// historyCap and channelsCap read the two byte budgets, tolerating a Deps built
// without a config (unit tests) by falling back to the documented default.
func (d *Deps) historyCap() int {
	if d.Cfg == nil {
		return readBudget(0)
	}
	return readBudget(d.Cfg.HistoryMaxBytes)
}

func (d *Deps) channelsCap() int {
	if d.Cfg == nil {
		return readBudget(0)
	}
	return readBudget(d.Cfg.ChannelsMaxBytes)
}

// ---------------------------------------------------------------------------
// get
// ---------------------------------------------------------------------------

type getArgs struct {
	Target  string `json:"target"`
	Message string `json:"message"`
	Media   string `json:"media"`
}

func getTool(prefix string) *mcp.Tool {
	return &mcp.Tool{
		Name:  "get",
		Title: "Read one message, with its files",
		Description: "Read one message by id straight from Discord. " +
			"This is the follow-up tool: " + ToolName(prefix, "poll") + " and " + ToolName(prefix, "history") +
			" list attachments as urls, and this is the one that turns a url into bytes. " +
			"target is where the message lives (an alias, channel:<id>, dm:<id>, #name, or me) and message is its id -- both from a row you were just given; a message id from another channel is not found here. " +
			"media picks what happens to the attachments: inline (base64 content block, content_index names it), save (written into the file box, local_path says where), none (metadata only, url stays), or omitted to follow the server's --media-* policy. " +
			"A message that was deleted, or one Discord simply did not give us, comes back as an error with Discord's own code; nothing is invented to fill the gap. " +
			"A row with neither text nor attachments can also be Discord's answer, not an empty message: see " + ToolName(prefix, "history") + "'s note.",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "target and message are required.",
			Properties: map[string]mcp.Schema{
				"target": {
					Type:        mcp.TypeString,
					Description: "Which channel to read from: an alias, channel:<id>, dm:<id>, #name, or me.",
				},
				"message": {
					Type: mcp.TypeString,
					Description: "The Discord message id (the id field on a " + ToolName(prefix, "poll") +
						" or " + ToolName(prefix, "history") + " row).",
				},
				"media": {
					Type:        mcp.TypeString,
					Enum:        []string{"inline", "save", "none"},
					Description: "What to do with the attachments. Omit it to follow the server's configured policy.",
				},
			},
			Required: []string{"target", "message"},
		},
	}
}

type getWire struct {
	Target  string   `json:"target"`
	Channel string   `json:"channel"`
	DM      bool     `json:"dm,omitempty"`
	Message msgWire  `json:"message"`
	Notes   []string `json:"notes,omitempty"`
}

func (d *Deps) handleGet(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	var a getArgs
	if err := decodeArguments(call, &a); err != nil {
		return errResult("bad_arguments", err.Error(), nil), nil
	}
	if d.DC == nil {
		return errResult(discord.CodeNotConnected, "this build has no Discord client, so a message cannot be read", nil), nil
	}
	if d.Router == nil {
		return errResult("no_routing", "this build has no routing table, so a target cannot be resolved", nil), nil
	}
	mode := strings.ToLower(strings.TrimSpace(a.Media))
	var pick mediaPicker
	switch mode {
	case "", "auto":
		pick = (*media.Session).Handle // the deployment's --media-* policy
	case "inline":
		pick = func(s *media.Session, at bus.Attachment) media.Handle { return s.HandleAs(at, media.ModeInline) }
	case "save":
		pick = func(s *media.Session, at bus.Attachment) media.Handle { return s.HandleAs(at, media.ModeSave) }
	case "none":
		pick = func(s *media.Session, at bus.Attachment) media.Handle { return s.HandleAs(at, media.ModeSkip) }
	default:
		return errResult("bad_media", `media must be "inline", "save" or "none" (or omitted to follow the server's policy)`, nil), nil
	}

	p := d.principal(ctx)
	ru, reason := d.routeRead(p, a.Target)
	if reason != "" {
		return errResult(reason, d.readHint(reason), d.targetList(reason, p)), nil
	}
	if strings.TrimSpace(a.Message) == "" {
		return errResult("bad_arguments", "message is required: the Discord message id", nil), nil
	}
	chanID, se := d.channelFor(ctx, ru)
	if se != nil {
		return errResult(se.Code, se.Msg, nil), nil
	}

	ev, err := d.DC.GetOne(ctx, chanID, strings.TrimSpace(a.Message), readLabel(ru))
	if err != nil {
		if s := asSendError(err); s != nil {
			return errResult(s.Code, s.Msg, nil), nil
		}
		return errResult("get_failed", err.Error(), nil), nil
	}
	if ev == nil {
		// Discord answered with nothing. Say so instead of shipping an empty row
		// the model has to interpret.
		return errResult("not_found", "discord returned no message for that id", nil), nil
	}

	m := d.buildMessage(p, ev)
	m.AgeS = 0    // ditto: this row has a ts, and that is the whole truth
	m.Target = "" // and the envelope says where it came from, once is enough
	m.DM = false  // ditto, and it could not have known: REST omits guild_id
	msgs := []pollMessage{m}
	kept := []*bus.Event{ev}
	blocks, mnotes := d.applyMediaWith(ctx, msgs, kept, pick)
	relabelPlaceholders(msgs, kept) // after the verdicts: the number IS the block index

	// get has no byte cap of its own, and that is not an oversight: one message's
	// text is already bounded by --text-max-runes, so the only thing that could
	// overflow here is the media, which never counts towards the JSON budget.
	env := getWire{Target: readTarget(ru, a.Target), Channel: chanID, DM: readIsDM(ru), Message: msgs[0].wire(), Notes: mnotes}
	res := jsonResult(env)
	res.Content = append(res.Content, blocks...)
	return res, nil
}

// ---------------------------------------------------------------------------
// channels
// ---------------------------------------------------------------------------

type channelsArgs struct {
	Guild string `json:"guild"`
	Type  string `json:"type"`
}

func channelsTool(prefix string) *mcp.Tool {
	return &mcp.Tool{
		Name:  "channels",
		Title: "List the channels this bot can see",
		Description: "Ask Discord what exists: the guilds this bot is in and the channels inside them. " +
			"This is Discord's directory, not this agent's permission list -- being able to see a channel says nothing about whether " +
			ToolName(prefix, "send") + " may write to it, so the answer keeps the two blocks apart: guilds[] from Discord, routing{} from our own configuration (the aliases you can actually address, and which of them are read_only). " +
			"guild narrows to one guild by id or by name (case insensitive); type filters channels by kind and takes the words the answer uses (text, voice, category, news, thread, forum, stage, dm, ...). " +
			"Direct messages are not in the Discord half: v0.29.0 has no wrapper for that endpoint, and which DMs matter here is a routing question, so they appear in routing{} instead. " +
			"Threads are not listed either (they are per-channel state, not a directory), and a guild we could not list comes back with no channels and a note. " +
			"If the answer is bigger than --channels-max-bytes, channels are dropped from the tail and a note says so: narrow with guild or type.",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "All arguments are optional.",
			Properties: map[string]mcp.Schema{
				"guild": {
					Type:        mcp.TypeString,
					Description: "Only this guild, by id or by name (case insensitive). Omit to list every guild this bot is in.",
				},
				"type": {
					Type:        mcp.TypeString,
					Description: "Only channels of this kind. One of the type words the answer uses (text, voice, category, news, store, thread, private-thread, news-thread, stage, directory, forum, media, group-dm, dm, other).",
				},
			},
		},
	}
}

type channelWire struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
}

type guildWire struct {
	ID       string        `json:"id"`
	Name     string        `json:"name,omitempty"`
	Channels []channelWire `json:"channels,omitempty"`
}

type channelsWire struct {
	Guilds  []guildWire  `json:"guilds,omitempty"`
	Routing *routingWire `json:"routing,omitempty"`
	Notes   []string     `json:"notes,omitempty"`
}

// routingWire is the local half of the answer: what this agent may address.
// Written in the same words discord_status uses, so "log is read_only" means one
// thing everywhere.
type routingWire struct {
	Targets  []string `json:"targets,omitempty"`   // this caller may send here
	ReadOnly []string `json:"read_only,omitempty"` // receives, but refused on send
}

// channelTypeWords is the filter vocabulary, kept next to the tool that
// publishes it. It is a subset on purpose: "other" and the odd kinds are
// listable but not worth filtering on.
var channelTypeWords = []string{
	"text", "voice", "category", "news", "thread", "private-thread", "forum", "stage", "dm",
}

func (d *Deps) handleChannels(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	var a channelsArgs
	if err := decodeArguments(call, &a); err != nil {
		return errResult("bad_arguments", err.Error(), nil), nil
	}
	if d.DC == nil {
		return errResult(discord.CodeNotConnected, "this build has no Discord client, so channels cannot be listed", nil), nil
	}
	wantType := strings.ToLower(strings.TrimSpace(a.Type))
	if wantType != "" && !knownChannelType(wantType) {
		return errResult("bad_type", "type must be one of: "+strings.Join(channelTypeWords, ", "), map[string]any{
			"got": wantType,
		}), nil
	}

	guilds, err := d.DC.ListChannels(ctx, a.Guild)
	if err != nil {
		if s := asSendError(err); s != nil {
			return errResult(s.Code, s.Msg, nil), nil
		}
		return errResult("channels_failed", err.Error(), nil), nil
	}
	if strings.TrimSpace(a.Guild) != "" && len(guilds) == 0 {
		return errResult("no_such_guild", "this bot is not in a guild called "+strings.TrimSpace(a.Guild)+
			"; call "+d.toolChannels()+" without guild to see the ones it is in.", nil), nil
	}

	var notes []string
	total := 0
	rows := make([]guildWire, 0, len(guilds))
	for _, g := range guilds {
		gw := guildWire{ID: g.ID, Name: g.Name}
		for _, ch := range g.Channels {
			if wantType != "" && ch.Type != wantType {
				continue
			}
			gw.Channels = append(gw.Channels, channelWire{ID: ch.ID, Name: ch.Name, Type: ch.Type})
			total++
		}
		if len(g.Channels) == 0 {
			notes = append(notes, "guild "+g.Name+" ("+g.ID+") gave us no channels at all (permissions, or it is simply empty).")
		}
		rows = append(rows, gw)
	}
	if wantType != "" && total == 0 {
		notes = append(notes, "no channel of kind "+wantType+".")
	}

	env := channelsWire{Guilds: rows, Notes: notes}
	if d.Router != nil {
		writable, readOnly := d.targetAliases(d.principal(ctx))
		env.Routing = &routingWire{Targets: writable, ReadOnly: readOnly}
	}

	budget := d.channelsCap()
	if budget > 0 {
		// Trim from the tail of the last guild that still has channels: the front
		// of the answer is the guild the caller is most likely to care about,
		// because that is the order Discord gave them to us in.
		dropped := 0
		for jsonLen(env) > budget {
			if !dropTailChannel(&env) {
				break // nothing left to drop: ship it oversized and say so
			}
			dropped++
		}
		if dropped > 0 {
			env.Notes = append(env.Notes, "the byte budget (--channels-max-bytes="+strconv.Itoa(budget)+
				") did not fit the list; "+strconv.Itoa(dropped)+" channel(s) were dropped from the tail -- narrow with guild or type and ask again.")
		}
	}
	return jsonResult(env), nil
}

// knownChannelType accepts the filter words the answer can produce. Anything
// discord.ChannelTypeWord can emit is fair game, so the filter is checked
// against a small published list and everything else is a typo.
func knownChannelType(w string) bool {
	for _, t := range channelTypeWords {
		if t == w {
			return true
		}
	}
	return false
}

// dropTailChannel removes the last channel of the last guild that still has
// one, and reports whether anything went.
func dropTailChannel(env *channelsWire) bool {
	for i := len(env.Guilds) - 1; i >= 0; i-- {
		if len(env.Guilds[i].Channels) > 0 {
			env.Guilds[i].Channels = env.Guilds[i].Channels[:len(env.Guilds[i].Channels)-1]
			return true
		}
	}
	return false
}
