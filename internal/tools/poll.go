// poll.go implements discord_poll: the blocking "give me what arrived" tool.
//
// The contract with the model is deliberately state-free. Messages sit in this
// agent's queue until a poll drains them; taking them out IS the acknowledgment,
// so there is no cursor to remember, no ack to send, and nothing to replay. A
// restart empties the queue (by design, see internal/bus), which is why
// catching up on older messages is the history tool's job (M3), not poll's.
//
// Anything left behind says so: whenever `remaining` is present, `waiting` and
// `oldest_s` ride along, whether or not this call also took messages (F-4). One
// key therefore means nothing arrived at all; `remaining` plus `waiting` mean the
// queue was NOT empty and this call's own filters refused what was in it.
//
// Two limits keep a reply inside a model context window:
//
//   - limit: at most N messages per call, oldest first.
//   - --poll-max-bytes: the reply is built event by event and the drain stops at
//     the first event that would not fit, so the leftovers wait for the next poll
//     IN ORDER (bus.Stop). Nothing is skipped over and nothing is lost.
//
// The byte budget is a fuse, not a meter: it counts exactly one thing, the bytes
// of the `text` fields this call ships. JSON overhead (keys, author, urls, notes)
// is deliberately not counted, so a reply can come back somewhat larger than the
// number; what it can never do is drop a 10 KB message straight into a harness
// that has no trimming of its own and blow the session up.
//
// A single event whose text is bigger than the whole budget would otherwise sit
// at the head of the queue forever, so its text is cut to the budget keeping the
// head, and a note says how big it really was. Nothing else is negotiable:
// attachments and embeds are never dropped over budget, which is what used to
// ship a message as an empty shell.
//
// What reaches the model is only what it needs in order to answer (see msgWire):
// target, author, the author's Discord id, text, timestamps, attachment urls.
// Everything else went to the log.
//
// Media runs in a second phase, after the drain. Queue.Drain calls the verdict
// function under the queue's lock, and a download must not hold that lock: the
// gateway would stop being able to Push. So the budget decides which messages
// and which attachments this call keeps from metadata alone, and only those are
// fetched afterwards. Base64 consequently never shows up in the JSON answer (it
// lives in the result's content array, not in this JSON), and no byte estimate
// pretends to price it: a model charges an image in tokens, not in JSON bytes.
package tools

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/media"
	"mcp-discord-roleplay/internal/router"
)

// pollMaxLimit is a hard ceiling on limit so one call cannot drain everything
// at once regardless of configuration.
const pollMaxLimit = 100

// pollDefaultLimit is what a caller gets when it does not say.
const pollDefaultLimit = 20

func pollTool(prefix string) *mcp.Tool {
	return &mcp.Tool{
		Name:  "poll",
		Title: "Wait for new Discord messages",
		Description: "Wait until this agent's queue holds messages, return them, and remove them from the queue. " +
			"This queue holds only what happened while you were away: messages this bot sent are never queued, because " +
			"the send call already handed them to you. " +
			"There is no cursor and no acknowledge step: what a poll hands you is yours, and what you do not take stays. " +
			"One poll answers one conversation: it takes the oldest queued message plus every other message from THAT channel, and leaves messages from other channels for the following polls. The top-level target names the conversation every message in this answer belongs to (rows do not repeat it), and discord_send takes that name back unchanged; to pick the channel yourself, pass targets. " +
			"reason=messages means you got something; reason=timeout is NOT a failure (the tool result's isError is false), it means nothing arrived within max_wait — call again; " +
			"reason=disconnected means the gateway was not listening during that wait, so nothing could have arrived; " +
			"reason=cancelled/shutdown means this answer was not delivered. " +
			"If remaining is present, that many messages are still queued — poll again right away. " +
			"Whenever waiting is present it names, by count, the filters that turned messages away in this pass (bot|mentions|target|channel|from|other_channel), and oldest_s says how long the oldest queued message has been waiting; they ride along with remaining, so an answer that took some messages still says why the rest stayed behind. waiting only counts what this pass scanned, so it can be smaller than remaining -- that is scope, not loss. " +
			"If dropped is present, messages were overwritten while nobody polled; poll more often. " +
			"A long message text may be cut to fit the server's byte budget, and a note then says how big it really was; only text is ever cut, attachments are always listed. " +
			"author_id is the speaker's Discord id: quote it as to:\"dm:<id>\" to answer that person in DMs even when they have no alias. " +
			"Each attachment reports what it came as: media=base64 means the bytes are appended to this result as a content block (content_index names it), " +
			"media=file means they were saved and local_path says where, media=url means metadata only and reason says why. " +
			"A custom emoji in the text is rewritten to <:name:img@N> where N is exactly that attachment's content_index — one number for the whole answer, so the code in the sentence and the picture are never two different addresses; when its bytes did not come, the code stays as Discord sent it. " +
			"This call blocks up to max_wait seconds, so your client's request timeout must be at least that long.",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "All arguments are optional.",
			Properties: map[string]mcp.Schema{
				"max_wait": {
					Type: mcp.TypeNumber,
					Description: "Seconds to wait for the first message when the queue is empty. " +
						"Default is the server's --poll-default-wait; anything outside the server's " +
						"[--poll-min-wait, --poll-max-wait] range is rejected with bad_max_wait. " +
						"Messages already in the queue return immediately without waiting.",
				},
				"limit": {
					Type:        mcp.TypeInteger,
					Description: "Maximum messages to return (default 20, ceiling 100). The rest stay queued.",
				},
				"targets": {
					Type:  mcp.TypeArray,
					Items: &mcp.Schema{Type: mcp.TypeString},
					Description: "Only messages from these aliases (see " + ToolName(prefix, "status") +
						" routing.targets). Omit to accept every target this agent may read.",
				},
				"channels": {
					Type:        mcp.TypeArray,
					Items:       &mcp.Schema{Type: mcp.TypeString},
					Description: "Only messages whose channel id, channel name, or routing alias matches (case-insensitive; a leading # and a channel: prefix are ignored).",
				},
				"from": {
					Type:        mcp.TypeArray,
					Items:       &mcp.Schema{Type: mcp.TypeString},
					Description: "Only messages from these user ids or display names (case-insensitive, leading @ optional).",
				},
				"mentions_only": {
					Type:        mcp.TypeBoolean,
					Description: "Only messages that @-mention the bot (default false).",
				},
				"ignore_bots": {
					Type: mcp.TypeBoolean,
					Description: "Skip messages authored by bots (default true). This covers other bots and webhooks; " +
						"this server's own messages are never queued, so they cannot echo back at you regardless of this flag.",
				},
			},
		},
	}
}

type pollArgs struct {
	MaxWait      *float64 `json:"max_wait"`
	Limit        *int     `json:"limit"`
	Targets      []string `json:"targets"`
	Channels     []string `json:"channels"`
	From         []string `json:"from"`
	MentionsOnly *bool    `json:"mentions_only"`
	IgnoreBots   *bool    `json:"ignore_bots"`
}

// pollMessage is the WORKING form of one queued event — it never goes on the
// wire, msgWire does, and the json tags below are decoration left so the two
// types can be read side by side (nothing marshals this one any more, which is
// the point: the budget is measured on the wire shape, and a field can be
// dropped from the answer without touching the bookkeeping).
//
// It is a separate type from bus.Event on purpose too: bus.Event is shared
// between agents and between polls (fan-out stores one pointer in several
// queues), so anything computed per call — trimmed text, media verdict — must
// live here and never back on the event.
type pollMessage struct {
	ID          string `json:"id,omitempty"`
	Target      string `json:"target,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
	DM          bool   `json:"dm,omitempty"`

	From bus.Author `json:"from"`
	Ts   string     `json:"ts"`
	AgeS float64    `json:"age_s,omitempty"`

	Text          string `json:"text,omitempty"`
	TextTruncated bool   `json:"text_truncated,omitempty"`
	TextFullBytes int    `json:"text_full_bytes,omitempty"`

	MentionsSelf bool `json:"mentions_self,omitempty"`

	// Reply is the message this one quotes, with its snippet already cut to
	// --reply-max-bytes (nil: this row answers nobody). Forwarded marks words that
	// came from elsewhere — see bus.Event.
	Reply     *bus.Reply `json:"reply,omitempty"`
	Forwarded bool       `json:"forwarded,omitempty"`

	Attachments []pollAttachment `json:"attachments,omitempty"`
	Embeds      []bus.Embed      `json:"embeds,omitempty"`
}

// pollAttachment is the per-call working view of an attachment: metadata plus
// how THIS call renders it. bus.Attachment stays shared and read-only, and the
// agent only ever sees the attWire projected from it.
type pollAttachment struct {
	ID           string  `json:"id"`
	Filename     string  `json:"filename,omitempty"`
	MIME         string  `json:"mime,omitempty"`
	Kind         string  `json:"kind"`
	Size         int     `json:"size,omitempty"`
	Width        int     `json:"width,omitempty"`
	Height       int     `json:"height,omitempty"`
	DurationSecs float64 `json:"duration_secs,omitempty"`
	URL          string  `json:"url"`

	// Media says what THIS call handed over: media.Base64 (a content block
	// follows, see ContentIndex), media.File (saved, see Path) or media.URL
	// (nothing but the link, and Reason says why). It starts as url in the first
	// phase and is only upgraded by applyMedia, so a value on the wire always
	// describes bytes the agent really got — never the policy behind them.
	Media  string `json:"media"`
	Reason string `json:"reason,omitempty"`

	// Bytes is what was fetched or written. Size above is Discord's own claim,
	// which can be 0 or disagree; both are kept because the gap is useful.
	Bytes int64 `json:"bytes,omitempty"`

	// Path is the name a saved file can be handed straight back to discord_send
	// as files[].path — the cache directory's real location is the operator's
	// business, this string is what makes the round trip work.
	Path string `json:"local_path,omitempty"`

	// ContentIndex is this attachment's block in the tool result's content
	// array. Index 0 is the JSON answer itself, so a real block is always >= 1;
	// omitted when there is no block.
	ContentIndex int `json:"content_index,omitempty"`
}

func (d *Deps) handlePoll(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	var a pollArgs
	if err := decodeArguments(call, &a); err != nil {
		return errResult("bad_arguments", err.Error(), nil), nil
	}
	if d.Hub == nil {
		return errResult("no_queue", "this server has no event hub, so no messages can arrive", nil), nil
	}
	p := d.principal(ctx)
	q, ok := d.Hub.Queue(p.AgentID)
	if !ok {
		return errResult("unknown_agent",
			"agent '"+p.AgentID+"' has no message queue. Poll only as an agent declared in routing.json, or as the server's --agent.",
			map[string]any{"agents": d.Hub.Agents()}), nil
	}

	// max_wait is validated against the operator's configured bounds, never a
	// hardcoded number: how long a caller may block is the operator's call.
	minW, maxW := float64(d.Cfg.PollMinWait), float64(d.Cfg.PollMaxWait)
	waitS := float64(d.Cfg.PollDefaultWait)
	if a.MaxWait != nil {
		v := *a.MaxWait
		if math.IsNaN(v) || math.IsInf(v, 0) || v < minW || v > maxW {
			return errResult("bad_max_wait",
				"max_wait must be within ["+ftoa(minW)+", "+ftoa(maxW)+"] seconds (server --poll-min-wait / --poll-max-wait)",
				map[string]any{"min": minW, "max": maxW, "given": v}), nil
		}
		waitS = v
	}

	limit := pollDefaultLimit
	if a.Limit != nil {
		limit = *a.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > pollMaxLimit {
			limit = pollMaxLimit
		}
	}

	f, err := d.newFilter(p, &a)
	if err != nil {
		return errResult("bad_target", err.Error(), nil), nil
	}

	// Jitter lands on the deadline only, never on the number we report, so many
	// agents on the same timer cannot synchronize on one instant.
	base := time.Duration(waitS * float64(time.Second))
	deadline := base + jitter(base, d.Cfg.PollJitterPct)
	timer := time.NewTimer(deadline)
	defer timer.Stop()

	start := time.Now()
	connected := d.gatewayUp()

	var (
		msgs      []pollMessage
		notes     []string
		remaining int
		used      int
	)

	// st holds the current Drain attempt's budget accounting; it is declared out
	// here because the cancellation path below (outside the loop) has to hand
	// back whatever the last pass took.
	var st *drainState

	for {
		// Read the wake-up channel BEFORE draining: a Push closes whichever
		// channel existed when it ran, so an event landing between the drain and
		// the select below still wakes us.
		wake := q.Notify()

		// Budget state is per attempt. A pass that only swallowed lifecycle
		// notices must not spend the budget of the pass that finds messages.
		st = &drainState{
			limit:  limit,
			budget: d.Cfg.PollMaxBytes,
		}
		_, rem := q.Drain(st.want(d, p, f))
		remaining = rem

		// What Drain handed back is already accounted for in st: lifecycle
		// notices were taken and discarded, messages were built, budgeted and
		// trimmed exactly once (never rebuilt, so the reply matches the cost the
		// budget was checked against).
		msgs, used, notes = st.msgsOut, st.used, st.notes

		if len(msgs) > 0 {
			break
		}
		select {
		case <-wake:
			continue
		case <-timer.C:
			d.notePoll(p.AgentID)
			return jsonResult(d.pollEnvelope(
				d.emptyHanded(q, st, "timeout", p.AgentID, start, waitS, connected))), nil
		case <-ctx.Done():
			// Nothing deliverable was taken (msgs is empty), so there is nothing
			// to put back.
			return jsonResult(d.pollEnvelope(
				d.emptyHanded(q, st, "cancelled", p.AgentID, start, waitS, connected))), nil
		case <-d.Stopping:
			return jsonResult(d.pollEnvelope(
				d.emptyHanded(q, st, "shutdown", p.AgentID, start, waitS, connected))), nil
		}
	}

	// The caller may have gone away while we were rendering. A response nobody
	// reads would silently swallow those messages, so hand them back at the
	// oldest end, in order, for the next poll.
	if ctx.Err() != nil {
		q.RequeueFront(st.kept)
		return jsonResult(d.pollEnvelope(
			d.emptyHanded(q, st, "cancelled", p.AgentID, start, waitS, connected))), nil
	}

	if st.stopped {
		// Kept deliberately, and it is not redundant with `waiting`: this note says
		// the call was CUT SHORT by a budget, while `waiting` says leftovers were
		// REJECTED by the caller's own filters. Two different reasons for leftovers,
		// and only one of them is the caller's doing.
		notes = append(notes, strconv.Itoa(remaining)+
			" message(s) are still queued; poll again right away")
	}
	d.notePoll(p.AgentID)

	// Second phase: the queue lock is gone, so this is where the bytes are
	// fetched. msgs is written through (it IS st.msgsOut), so the JSON built
	// below carries the verdicts, and the blocks go after it.
	blocks, mnotes := d.applyMediaWith(ctx, msgs, st.kept, (*media.Session).Handle)
	relabelPlaceholders(msgs, st.kept) // after the verdicts: the number IS the block index
	notes = append(notes, mnotes...)

	// A caller that vanished during the downloads would not read this answer
	// either, so the messages go back instead of being swallowed. Anything
	// already saved stays on disk: the next poll reuses it rather than refetching.
	if ctx.Err() != nil {
		q.RequeueFront(st.kept)
		return jsonResult(d.pollEnvelope(
			d.emptyHanded(q, st, "cancelled", p.AgentID, start, waitS, connected))), nil
	}

	var self string
	if d.DC != nil {
		snap := d.DC.Snapshot()
		self = snap.SelfName
		if self == "" {
			self = snap.SelfTag
		}
	}
	res := jsonResult(d.pollEnvelope(pollMeta{
		reason: "messages", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
		channel: st.chanLabel, dm: st.chanDM,
		maxWait: waitS, connected: connected, self: self,
		count: len(msgs), remaining: remaining, partial: st.stopped,
		bytes: used, notes: notes, messages: msgs,
		dropped: queueDropped(q),
		// F-4: the same two hints an empty answer gets. `waiting` covers only what
		// this pass scanned, so it may honestly be smaller than `remaining`; that is
		// a scope difference, not a lost message.
		waiting: st.renderHeld(),
		oldestS: oldestWaited(q),
	}))
	// The JSON answer occupies content[0]; every media block lands after it, in
	// the order the messages were read. That is what makes content_index
	// addressable — and why inline base64 never counts towards the byte budget.
	res.Content = append(res.Content, blocks...)
	return res, nil
}

// mediaPicker is the one decision applyMediaWith delegates: given a session and
// one attachment, produce its Handle. Poll passes (*media.Session).Handle and
// gets the configured --media-* policy; the read tools pass a closure around
// HandleAs so a caller who asked for "inline" on one message gets it without
// anybody pretending the deployment's default changed.
type mediaPicker func(*media.Session, bus.Attachment) media.Handle

// applyMediaWith is the second half of a poll, and of the read tools: it runs a
// media policy over the attachments this call actually kept and returns the
// content blocks they grew into, plus notes worth telling the model.
//
// kept is the caller's own record of which event produced which message, in the
// same order, so kept[i].Attachments is where the bytes for
// msgs[i].Attachments come from. Nothing trims the attachment lists any more
// (the byte budget only negotiates text), so the two line up exactly — which is
// still the point of splitting poll into two phases: a message left in the queue
// for the next poll never costs a download now.
func (d *Deps) applyMediaWith(ctx context.Context, msgs []pollMessage, kept []*bus.Event, pick mediaPicker) ([]mcp.ContentPart, []string) {
	if d.Media == nil || len(msgs) == 0 {
		return nil, nil
	}
	sess := d.Media.NewSession(ctx)
	defer sess.Close()

	var (
		saved, skipped int
		reasons        = map[string]int{}
	)
	for i := range msgs {
		if i >= len(kept) {
			break // defensive: the two are built in lockstep
		}
		ev := kept[i]
		for j := range msgs[i].Attachments {
			if j >= len(ev.Attachments) {
				break // ditto: the wire list is a prefix of the event's
			}
			before := len(sess.Content())
			h := pick(sess, ev.Attachments[j])
			att := &msgs[i].Attachments[j]
			att.Media, att.Reason, att.Bytes, att.Path = h.Media, h.Reason, h.Bytes, h.Path
			switch h.Media {
			case media.MediaFile:
				saved++
			case media.MediaBase64:
				// The block is the session's `before`-th, and the wire array has
				// the JSON answer in front of every block, hence the +1. Counting
				// here instead of trusting Handle.ContentIndex keeps this
				// function the owner of the result's layout.
				if len(sess.Content()) > before {
					att.ContentIndex = before + 1
				}
			default:
				skipped++
				if h.Reason != "" {
					reasons[h.Reason]++
				}
			}
		}
	}

	var notes []string
	if saved > 0 {
		notes = append(notes, "an attachment was saved into the media cache: local_path feeds "+
			"straight back into "+d.toolSend()+"'s files[].path, and polling the same "+
			"attachment again will not re-download it")
	}
	if skipped > 0 {
		keys := make([]string, 0, len(reasons))
		for r := range reasons {
			keys = append(keys, r)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, r := range keys {
			parts = append(parts, r+" x"+strconv.Itoa(reasons[r]))
		}
		notes = append(notes, strconv.Itoa(skipped)+" attachment(s) came with a url only "+
			"(reason: "+strings.Join(parts, ", ")+"); the files are still on Discord, "+
			"the url can be pasted as-is")
	}
	return sess.Content(), notes
}

// relabelPlaceholders turns the provisional emoji handles in a message's text into
// the one number the agent can use: the index of the content block that carries
// those bytes, which is exactly what content_index says on the attachment row.
//
// It has to run after applyMedia because the number is a property of THIS answer.
// How many inline attachments came before a given one depends on which messages
// made it past the byte budget and which of their attachments got bytes -- and none
// of that exists at ingress, which is when normalize wrote the handle. The price of
// that is a handle whose number follows the batch: the same message polled alone
// says img@1 and polled behind two others says img@4. It is still the only number
// in the answer, which is the point.
//
// An emoji whose bytes did not come (policy, budget, too big, failed fetch) loses
// its handle and shows the Discord code again: a pointer to a block that was never
// emitted is worse than no pointer, and the raw code carries the id -- the one
// thing that matches its attachment row.
//
// The rewrite edits pollMessage.Text, the per-call copy, never bus.Event.Text, which
// every queue that received this event shares. It can make the text a few bytes
// longer than what the budget counted (a raw code is longer than a handle), which
// the fuse tolerates by design: it prices text, not exactness.
func relabelPlaceholders(msgs []pollMessage, kept []*bus.Event) {
	for i := range msgs {
		if i >= len(kept) || kept[i] == nil || msgs[i].Text == "" {
			continue
		}
		pairs := make([]string, 0, len(msgs[i].Attachments))
		for j := range msgs[i].Attachments {
			if j >= len(kept[i].Attachments) {
				break // ditto: the wire list is a prefix of the event's
			}
			handle := kept[i].Attachments[j].Placeholder
			if handle == "" {
				continue // an upload or a sticker: no handle of ours sits in the text
			}
			repl := kept[i].Attachments[j].Raw
			if a := &msgs[i].Attachments[j]; a.Media == media.MediaBase64 && a.ContentIndex > 0 {
				repl = reindex(handle, a.ContentIndex)
			}
			if repl == "" {
				continue // never eat the handle for want of something to put back
			}
			pairs = append(pairs, handle, repl)
		}
		if len(pairs) == 0 {
			continue
		}
		// ONE Replacer pass rather than a ReplaceAll per attachment: two emoji can
		// carry the same name in different guilds (`<:gaze:111>` and `<:gaze:222>`),
		// and renumbering the first can otherwise produce the exact literal the second
		// is searching for -- which would then be rewritten twice and point at a file
		// it is not. A Replacer scans the input once and never revisits what it wrote.
		msgs[i].Text = strings.NewReplacer(pairs...).Replace(msgs[i].Text)
	}
}

// reindex rewrites the number at the tail of an `...:img@N>` handle and leaves the
// rest of the code exactly as it was, so the name the author gave the emoji stays
// visible to the model.
func reindex(handle string, n int) string {
	k := strings.LastIndexByte(handle, '@')
	if k < 0 {
		return handle
	}
	return handle[:k+1] + strconv.Itoa(n) + ">"
}

// --- filters ---------------------------------------------------------------

type pollFilter struct {
	targets    map[string]bool
	kinds      map[string]bool // kinds opened by a wildcard rule ("dm:*", "channel:*")
	channels   map[string]bool
	from       map[string]bool
	mentions   bool
	ignoreBots bool
}

func (d *Deps) newFilter(p router.Principal, a *pollArgs) (*pollFilter, error) {
	f := &pollFilter{
		targets:    map[string]bool{},
		kinds:      map[string]bool{},
		channels:   map[string]bool{},
		from:       map[string]bool{},
		ignoreBots: true,
	}
	if a.MentionsOnly != nil {
		f.mentions = *a.MentionsOnly
	}
	if a.IgnoreBots != nil {
		f.ignoreBots = *a.IgnoreBots
	}
	for _, ref := range a.Targets {
		s := strings.TrimSpace(ref)
		if s == "" {
			continue
		}
		ru, ok := d.Router.Resolve(s)
		if !ok || ru.Kind == router.KindMe {
			// Unknown alias: keep the literal so a raw canonical target
			// ("channel:123") still works without routing.json.
			f.targets[strings.ToLower(s)] = true
			continue
		}
		if !d.Router.MayRead(p, ru) {
			return nil, fmt.Errorf("target %q is read-only for this agent: it cannot be polled", ref)
		}
		if ru.Wildcard {
			// "dm:*" names a class, not a channel: match by kind, because no event
			// is ever addressed to a channel called "*".
			f.kinds[ru.Kind] = true
		}
		f.targets[strings.ToLower(ru.Target)] = true
		f.targets[strings.ToLower(ru.Alias)] = true
	}
	for _, c := range a.Channels {
		s := strings.TrimSpace(c)
		if s == "" {
			continue
		}
		f.channels[channelKey(s)] = true
		// An alias has to work here too. Every other tool takes one (`send`,
		// `history`, `get`), and a filter that matches nothing does not report an
		// unknown name -- it just says the messages were turned away by `channel`,
		// which reads exactly like "nobody talked". Live it bit us: the channel's
		// Discord name is 一般 while our alias is test, so {"channels":["test"]}
		// emptied the answer. Resolve it and register everything the answer could
		// be addressed by; an unknown name still just stays a literal, as above.
		if ru, ok := d.Router.Resolve(s); ok && !ru.Wildcard {
			f.channels[channelKey(ru.Alias)] = true
			f.channels[channelKey(ru.Target)] = true
		}
	}
	for _, u := range a.From {
		s := strings.TrimSpace(u)
		if s != "" {
			f.from[strings.ToLower(strings.TrimPrefix(s, "@"))] = true
		}
	}
	return f, nil
}

// eventTargets lists the canonical names an event answers to, so a filter can
// match a DM by the peer's user id as well as by the DM channel id.
func eventTargets(e *bus.Event) []string {
	out := make([]string, 0, 4)
	if e.Target != "" {
		out = append(out, strings.ToLower(e.Target))
	}
	if e.ChannelID != "" {
		out = append(out, "channel:"+strings.ToLower(e.ChannelID))
	}
	if e.DM {
		if peer := e.Extra["peer"]; peer != "" {
			out = append(out, "dm:"+strings.ToLower(peer))
		}
	}
	if e.ChannelName != "" {
		out = append(out, strings.ToLower(e.ChannelName))
	}
	// normalize() already canonicalizes a DM's Target to "dm:<peer>", so the
	// explicit dm: entry above is now usually a duplicate (it stays as the
	// fallback for events built elsewhere, e.g. in tests or by future producers).
	// Collapse them: this list is also what the audit log prints, and the same
	// address twice reads like a bug.
	seen := make(map[string]bool, len(out))
	uniq := out[:0]
	for _, t := range out {
		if seen[t] {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
	}
	return uniq
}

// channelKey normalizes one side of a `channels` filter: lowercase, trimmed of
// the decorations both a caller and an event can carry ("#rp-main",
// "channel:111..."). One function for both sides is the point -- if the filter
// trimmed something the event did not (or the reverse), the tool description and
// the code would disagree about what matches.
func channelKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimPrefix(s, "channel:")
	return s
}

// channelKeys lists what a `channels` filter may match on: the channel id, the
// name Discord itself gave the channel, and the target the router picked for the
// event (our alias live, e.g. "test", or the canonical "channel:<id>" as tests
// build it). The last one is what makes an alias -- and a DM alias, whose id is
// a peer id no channel id can equal -- work at all.
func channelKeys(e *bus.Event) []string {
	out := make([]string, 0, 3)
	seen := make(map[string]bool, 3)
	for _, raw := range []string{e.ChannelID, e.ChannelName, e.Target} {
		k := channelKey(raw)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// eventKind names the target kind an event belongs to, which is what a wildcard
// filter ("dm:*") matches on.
func eventKind(e *bus.Event) string {
	if e.DM {
		return router.KindDM
	}
	return router.KindChannel
}

// reject names the first gate that turns this event away, or "" when it passes.
// The word it returns is what a caller sees in the `waiting` field of an empty
// answer, so each name has to be something the model can act on: `bot`
// (ignore_bots — other bots and webhooks), `mentions` (mentions_only), and
// `target`/`channel`/`from` (this call narrowed the source itself). A self
// message never reaches here any more: the gateway client does not queue our own
// echo at all, because discord_send already handed it to the caller.
func (f *pollFilter) reject(e *bus.Event) string {
	if f.ignoreBots && e.Author.Bot {
		return "bot"
	}
	if f.mentions && !e.MentionsSelf {
		return "mentions"
	}
	if len(f.targets) > 0 || len(f.kinds) > 0 {
		hit := false
		for _, t := range eventTargets(e) {
			if f.targets[t] {
				hit = true
				break
			}
		}
		if !hit && f.kinds[eventKind(e)] {
			hit = true
		}
		if !hit {
			return "target"
		}
	}
	if len(f.channels) > 0 {
		hit := false
		for _, t := range channelKeys(e) {
			if f.channels[t] {
				hit = true
				break
			}
		}
		if !hit {
			return "channel"
		}
	}
	if len(f.from) > 0 {
		cands := []string{strings.ToLower(e.Author.ID), strings.ToLower(e.Author.Name)}
		if e.Author.Tag != "" {
			cands = append(cands, strings.ToLower(e.Author.Tag),
				strings.ToLower(strings.SplitN(e.Author.Tag, "#", 2)[0]))
		}
		hit := false
		for _, c := range cands {
			if c != "" && f.from[c] {
				hit = true
				break
			}
		}
		if !hit {
			return "from"
		}
	}
	return ""
}

// keep is the yes/no view of reject, for callers that do not care which gate fired.
func (f *pollFilter) keep(e *bus.Event) bool { return f.reject(e) == "" }

// --- budgeted drain --------------------------------------------------------

// drainState carries one Drain attempt's budget accounting.
type drainState struct {
	limit  int
	budget int // bytes, -1 = uncapped (config rejects 0, so there is only one sentinel)
	used   int
	count  int
	notes  []string

	// msgsOut / kept are built in lockstep with the verdicts: the wire form and
	// the event it came from, in queue order. kept is what RequeueFront puts back
	// if the caller vanishes before the reply is written.
	msgsOut []pollMessage
	kept    []*bus.Event

	// stopped records that this pass ended on limit/budget rather than on an
	// empty queue. It is what makes partial:true mean "this call was cut short"
	// instead of "the queue happens to be non-empty", which would be true for any
	// filtered poll and would push the model into pointless polls.
	stopped bool

	// held tallies what this pass skipped, by the gate that rejected it, so an
	// empty-handed poll can say "2 bot" instead of letting the caller guess
	// whether the channel was quiet or its own filters were narrow. It only means
	// something when the pass scanned the whole queue: a pass that ended on Stop
	// never reached the tail, so it reports nothing.
	held map[string]int

	// chanKey latches the conversation of the first message this pass accepts, and
	// every later event from a different one is held back: one poll answers one
	// channel, because a batch that interleaves two conversations is the shape a
	// model most reliably replies to the wrong half of. It is per attempt, so a
	// limit/budget cut in the middle of a channel just means the next poll
	// continues that same channel rather than switching. chanLabel is what the
	// envelope says out loud -- the caller's own alias for it -- and chanDM whether
	// that conversation is a DM.
	chanKey   string
	chanLabel string
	chanDM    bool
}

// want is the verdict function handed to Queue.Drain: it decides, strictly in
// queue order, what this call takes.
func (st *drainState) want(d *Deps, p router.Principal, f *pollFilter) func(*bus.Event) bus.Verdict {
	return func(e *bus.Event) bus.Verdict {
		if e.Kind == bus.KindSystem {
			// Defensive: a lifecycle notice wakes a queue without being stored, so
			// nothing puts one here any more. If a future caller does, treat it the
			// way notices have always been treated — read it and move on.
			return bus.Take
		}
		if why := f.reject(e); why != "" {
			st.hold(why)
			return bus.Skip
		}
		// One channel per answer: the first accepted message pins the conversation,
		// and anything from elsewhere is Skipped -- left in the queue, in order,
		// where a later poll reaches it. Not Stop: Stop would also strand the
		// messages behind it that might well belong to this channel.
		if key := pollChannelKey(e); st.chanKey == "" {
			st.chanKey = key
		} else if key != st.chanKey {
			st.hold("other_channel")
			return bus.Skip
		}
		if st.count >= st.limit {
			st.stopped = true
			st.noteLimit()
			return bus.Stop
		}
		m := d.buildMessage(p, e)
		cost := messageCost(m)
		if !config.Unlimited(st.budget) && st.used+cost > st.budget {
			if st.count > 0 {
				st.stopped = true
				st.noteBudgetFull()
				return bus.Stop
			}
			// This is the only thing we could return, and refusing forever would
			// deadlock the queue head: shrink it into the budget and say so.
			fitMessage(&m, st.budget, st)
			cost = messageCost(m)
		}
		// The conversation is named once for the whole batch, on the envelope, so no
		// row pays for a fact that cannot change inside this answer.
		if st.chanLabel == "" {
			st.chanLabel, st.chanDM = m.Target, m.DM
		}
		m.Target, m.DM = "", false
		st.msgsOut = append(st.msgsOut, m)
		st.kept = append(st.kept, e)
		st.used += cost
		st.count++
		return bus.Take
	}
}

// pollChannelKey is the identity a poll batches by. It uses the canonical target
// (channel:<id> / dm:<peer>, normalised on the way into the queue) rather than
// the label a row carries, because that label is per-principal: two agents may
// call the same channel by different names, and one conversation must not split
// into two batches because of it. When an event has no canonical target the
// channel id is the next best thing, and an event with neither gets "", which
// groups those strays together -- as good a guess as any.
func pollChannelKey(e *bus.Event) string {
	if t := strings.ToLower(e.Target); t != "" {
		return t
	}
	if e.DM {
		if peer := strings.ToLower(e.Extra["peer"]); peer != "" {
			return "dm:" + peer
		}
	}
	if id := strings.ToLower(e.ChannelID); id != "" {
		return "channel:" + id
	}
	return ""
}

// noteBudgetFull records, at most once per attempt, that the drain stopped
// because of the byte budget rather than because the queue was empty.
func (st *drainState) noteBudgetFull() {
	const msg = "later messages exceeded this poll's text budget and stay queued; the next poll gets them first"
	for _, n := range st.notes {
		if n == msg {
			return
		}
	}
	st.notes = append(st.notes, msg)
}

// noteLimit records that this call reached its limit; the rest stay queued.
func (st *drainState) noteLimit() {
	const msg = "this poll hit its limit; the rest are still queued, poll again right away"
	for _, n := range st.notes {
		if n == msg {
			return
		}
	}
	st.notes = append(st.notes, msg)
}

// hold records one rejection under the gate that caused it.
func (st *drainState) hold(why string) {
	if st.held == nil {
		st.held = map[string]int{}
	}
	st.held[why]++
}

// renderHeld turns the tally into one short, stable string: busiest gate first,
// ties broken by name, e.g. "2 bot, 1 mentions". Empty when nothing was skipped.
func (st *drainState) renderHeld() string {
	if len(st.held) == 0 {
		return ""
	}
	type pair struct {
		why string
		n   int
	}
	list := make([]pair, 0, len(st.held))
	for why, n := range st.held {
		list = append(list, pair{why, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].why < list[j].why
	})
	parts := make([]string, 0, len(list))
	for _, p := range list {
		parts = append(parts, strconv.Itoa(p.n)+" "+p.why)
	}
	return strings.Join(parts, ", ")
}

// targetLabel names the conversation this principal may answer with. An alias
// wins because a model should not have to remember snowflakes; when no rule
// claims the event -- or only a WILDCARD rule does, whose alias is a policy and
// not an address -- it falls back to a concrete target id that discord_send
// accepts unchanged: dm:<peer> for a DM, otherwise the event's own label.
func (d *Deps) targetLabel(p router.Principal, e *bus.Event) string {
	if d.Router != nil {
		if alias := d.Router.AliasForTarget(p, eventTargets(e)...); alias != "" {
			return alias
		}
	}
	if e.DM {
		if peer := e.Extra["peer"]; peer != "" {
			return "dm:" + strings.ToLower(peer)
		}
	}
	return e.Target
}

// --- rendering -------------------------------------------------------------

// buildMessage turns a queued event into its wire form for this call.
//
// The target is relabelled to the alias THIS principal knows, because that is
// the name it has to answer with, and a model should not have to remember
// snowflakes. bus.Event itself is never touched: the same pointer sits in other
// agents' queues, and their alias may be different (or none).
func (d *Deps) buildMessage(p router.Principal, e *bus.Event) pollMessage {
	target := d.targetLabel(p, e)
	m := pollMessage{
		ID:            e.MessageID,
		Target:        target,
		ChannelID:     e.ChannelID,
		ChannelName:   e.ChannelName,
		DM:            e.DM,
		From:          e.Author,
		Ts:            e.Time,
		Text:          e.Text,
		TextTruncated: e.Truncated,
		MentionsSelf:  e.MentionsSelf,
		Reply:         e.Reply,
		Forwarded:     e.Forwarded,
		Attachments:   make([]pollAttachment, 0, len(e.Attachments)),
		Embeds:        e.Embeds,
	}
	if m.TextTruncated {
		// Already shortened on the way in (--text-max-runes). Report the length
		// we hold so a reader knows the original was longer still.
		m.TextFullBytes = len(e.Text)
	}
	if !e.Ts.IsZero() {
		m.AgeS = round1(time.Since(e.Ts).Seconds())
	}
	if m.Reply != nil {
		// The event is shared and must stay whole; the cut happens on this call's
		// copy, exactly like a trimmed text does.
		copied := *m.Reply
		copied.Text = cutRunes(copied.Text, d.Cfg.ReplyMaxBytes)
		m.Reply = &copied
	}
	for _, a := range e.Attachments {
		m.Attachments = append(m.Attachments, pollAttachment{
			ID: a.ID, Filename: a.Filename, MIME: a.ContentType, Kind: a.Kind,
			Size: a.Size, Width: a.Width, Height: a.Height, DurationSecs: a.DurationSecs,
			URL: a.URL, Media: media.MediaURL,
		})
	}
	return m
}

// messageCost is what one message contributes to --poll-max-bytes: the bytes of
// its `text` plus the quoted line it carries, nothing else. The flag is a fuse
// against a single oversized message, not a size meter for the reply: JSON
// overhead, attachment entries and base64 content blocks are priced elsewhere
// (--inline-max-count and friends). Counting them here is what used to force a
// fresh Marshal per trimming step. A quote is text in everything but name, so it
// pays its way in or the fuse stops meaning what it says.
func messageCost(m pollMessage) int {
	cost := len(m.Text)
	if m.Reply != nil {
		cost += len(m.Reply.Text)
	}
	return cost
}

// cutRunes keeps the head of s within max BYTES, never splitting a rune: max < 0
// means no cap, 0 means "drop it", which is how --reply-max-bytes=0 turns the
// quoted text off while keeping the id and the name.
func cutRunes(s string, max int) string {
	if max < 0 || len(s) <= max {
		return s
	}
	if max == 0 {
		return ""
	}
	cut := 0
	for cut < len(s) {
		_, size := utf8.DecodeRuneInString(s[cut:])
		if cut+size > max {
			break
		}
		cut += size
	}
	return s[:cut]
}

// fitMessage cuts m.Text down to room bytes, keeping its head, and notes how
// big the text really was. Only the first message of a drain can reach here (the
// drain stops before every later one), and only the words are negotiable — the
// quote is negotiable before the text is, because it summarises a message the
// caller can still go and read in full. Dropping attachments or embeds to save
// bytes was what let a message ship as an empty shell. The cut lands on a rune
// boundary, so the reply never carries half a UTF-8 character.
func fitMessage(m *pollMessage, room int, st *drainState) {
	if messageCost(*m) <= room {
		return
	}
	if m.Reply != nil && m.Reply.Text != "" {
		// The quote is the first thing to go: it is a courtesy summary of a message
		// the caller can fetch in full, while this row's own text is what it came
		// to read. The id and the name survive — only the words cost too much.
		m.Reply.Text = ""
		st.notes = append(st.notes, "the quoted message's text was dropped to keep this message inside --poll-max-bytes")
	}
	if len(m.Text) <= room {
		return
	}
	full := len(m.Text)
	if m.TextFullBytes > full {
		full = m.TextFullBytes
	}
	cut := 0
	for cut < len(m.Text) {
		_, size := utf8.DecodeRuneInString(m.Text[cut:])
		if cut+size > room {
			break
		}
		cut += size
	}
	m.Text = m.Text[:cut]
	m.TextTruncated = true
	m.TextFullBytes = full
	st.notes = append(st.notes, "this message's text was too long (originally "+strconv.Itoa(full)+
		" bytes) and was cut to "+strconv.Itoa(cut)+" bytes, keeping the start")
}

// --- wire shapes -----------------------------------------------------------
//
// pollMessage above is the WORKING form: budget accounting, trimming and the
// media verdict all live on it, and it keeps fields the model never sees (its
// json tags are therefore dead weight kept only for readability). These types
// below are the only thing that gets marshalled, which buys three things: the
// model never sees a field nobody asked for, bus.Author/bus.Embed shapes can no
// longer leak onto the wire, and adding a field to pollMessage costs the model
// nothing until it is deliberately added here too. There is no `self` field: a
// message this bot sent is dropped before it is ever queued, so nothing that
// arrives here could need it (the send reply is what tells you what you said).

type embedWire struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
}

type attWire struct {
	Name         string  `json:"name,omitempty"`
	Kind         string  `json:"kind"`
	URL          string  `json:"url"`
	DurationSecs float64 `json:"duration_secs,omitempty"`
	Media        string  `json:"media"`
	Reason       string  `json:"reason,omitempty"`
	Path         string  `json:"local_path,omitempty"`
	ContentIndex int     `json:"content_index,omitempty"`
}

type msgWire struct {
	// ID is the Discord message id: the handle the read tools take. It costs a
	// poll roughly twenty bytes a message and buys the ability to follow up on a
	// row whose attachment came back as a bare url.
	ID           string      `json:"id,omitempty"`
	Target       string      `json:"target,omitempty"`
	Author       string      `json:"author"`
	AuthorID     string      `json:"author_id,omitempty"`
	Bot          bool        `json:"bot,omitempty"`
	DM           bool        `json:"dm,omitempty"`
	Ts           string      `json:"ts,omitempty"`
	AgeS         float64     `json:"age_s,omitempty"`
	Text         string      `json:"text,omitempty"`
	TextCut      bool        `json:"text_truncated,omitempty"`
	MentionsSelf bool        `json:"mentions_self,omitempty"`
	Reply        *replyWire  `json:"reply_to,omitempty"`
	Forwarded    bool        `json:"forwarded,omitempty"`
	Attachments  []attWire   `json:"attachments,omitempty"`
	Embeds       []embedWire `json:"embeds,omitempty"`
}

// replyWire is the quoted message as one row can afford to show it: who it was
// said to and a head of what they said. An entry carrying only an id means
// Discord did not hand us the quoted message — it may simply not have gone to
// look, or it may be gone; we report the id and let the caller decide, because
// dc_get takes it either way.
type replyWire struct {
	ID     string `json:"id"`
	Author string `json:"author,omitempty"`
	Text   string `json:"text,omitempty"`
}

// wire projects the working form onto what the model receives.
func (m pollMessage) wire() msgWire {
	out := msgWire{
		ID:           m.ID,
		Target:       m.Target,
		Author:       m.From.Name,
		AuthorID:     m.From.ID,
		Bot:          m.From.Bot,
		DM:           m.DM,
		Ts:           m.Ts,
		AgeS:         m.AgeS,
		Text:         m.Text,
		TextCut:      m.TextTruncated,
		MentionsSelf: m.MentionsSelf,
		Forwarded:    m.Forwarded,
	}
	if m.Reply != nil {
		out.Reply = &replyWire{ID: m.Reply.ID, Author: m.Reply.Author, Text: m.Reply.Text}
	}
	if out.Author == "" {
		// An unattributed line is worse than an ugly one: fall back to whatever
		// tag Discord gave us rather than presenting anonymous text.
		out.Author = m.From.Tag
	}
	if out.AuthorID == "?" {
		// buildEvent's placeholder for an author Discord never named. It is not a
		// usable id, and echoing it back as to:"dm:?" would be a trap.
		out.AuthorID = ""
	}
	if len(m.Attachments) > 0 {
		out.Attachments = make([]attWire, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			out.Attachments = append(out.Attachments, attWire{
				Name: a.Filename, Kind: a.Kind, URL: a.URL,
				DurationSecs: a.DurationSecs, Media: a.Media, Reason: a.Reason,
				Path: a.Path, ContentIndex: a.ContentIndex,
			})
		}
	}
	if len(m.Embeds) > 0 {
		out.Embeds = make([]embedWire, 0, len(m.Embeds))
		for _, e := range m.Embeds {
			out.Embeds = append(out.Embeds, embedWire{
				Title: e.Title, Description: e.Description, URL: e.URL,
			})
		}
	}
	return out
}

func wireMessages(msgs []pollMessage) []msgWire {
	out := make([]msgWire, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.wire())
	}
	return out
}

// --- envelope --------------------------------------------------------------

type pollMeta struct {
	reason    string
	agent     string
	waitedMS  int64
	maxWait   float64
	connected bool
	self      string
	count     int
	remaining int
	partial   bool
	bytes     int
	notes     []string
	messages  []pollMessage
	dropped   uint64

	// waiting and oldestS describe what is STILL queued on any answer that leaves
	// something behind (F-4), so the caller learns whether the channel was quiet or
	// its own filters were narrow -- even when this very call also handed messages
	// over. Both stay zero when the queue is empty.
	waiting string
	oldestS float64

	// channel and dm name the conversation the messages in this answer came from.
	// They are only ever set on an answer that has messages: a batch is one
	// conversation now, so it is an envelope fact, not a per-row one.
	channel string
	dm      bool
}

// pollWire is the envelope, and it is the whole budget: a quiet poll costs one
// key -- reason, which is also what says whether anything arrived -- and every
// other field only earns its place by being non-empty. Success is not a field:
// a call that failed never reaches this struct, it goes out as an errResult
// with isError set.
type pollWire struct {
	Reason string `json:"reason"`
	// Target is the conversation every message in this answer belongs to, named the
	// way this caller addresses it (an alias when it has one, otherwise a target id
	// discord_send still accepts). Rows do not repeat it. Absent when there are no
	// messages, because there is no batch to name.
	Target    string    `json:"target,omitempty"`
	DM        bool      `json:"dm,omitempty"`
	Messages  []msgWire `json:"messages,omitempty"`
	Remaining int       `json:"remaining,omitempty"` // absent means caught up
	Dropped   uint64    `json:"dropped,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
	// Waiting names, by count, the filters that turned messages away in this pass
	// ("2 bot, 1 mentions"). It appears whenever the queue was NOT empty at the end
	// of the call, empty-handed or not (F-4) -- which is the whole point: without it
	// a caller cannot tell "nobody spoke" from "nobody you asked about spoke", and
	// would have to poll again with a different filter to find out. Its counts cover
	// only what this pass scanned, so they can be smaller than Remaining. OldestS
	// rides along for the same reason -- three messages that have waited four minutes
	// are a different problem from three that arrived a second ago -- and it measures
	// the oldest queued message whoever that one was waiting for, so it need not line
	// up with Waiting's tally.
	Waiting string  `json:"waiting,omitempty"`
	OldestS float64 `json:"oldest_s,omitempty"`
}

// pollEnvelope builds the reply from the bookkeeping. What pollMeta carries and
// is not here — agent, timings, byte counts, the persona's own name — is log
// material: the empty answer stays empty on purpose, because every byte of it
// goes back into the model's context on every turn.
func (d *Deps) pollEnvelope(m pollMeta) pollWire {
	reason := m.reason
	if m.count == 0 && reason == "timeout" && !m.connected {
		// "timeout" reads as "nobody spoke". On a dead gateway nobody could
		// have, and the model should not keep waiting on it. cancelled and
		// shutdown keep their own words: they say what ended the wait.
		reason = "disconnected"
	}
	w := pollWire{Reason: reason}
	// With messages there is the batch, the notes, and what was overwritten. Without
	// them the two cases -- nobody spoke, or nobody you asked about spoke -- are told
	// apart by whether the leftovers block below fires at all.
	if m.count > 0 {
		w.Target, w.DM = m.channel, m.dm
		w.Messages = wireMessages(m.messages)
		w.Remaining = m.remaining
		w.Dropped = m.dropped
		w.Notes = m.notes
	} else if m.remaining > 0 {
		w.Remaining = m.remaining
		w.Dropped = m.dropped
	}
	// F-4: what stayed behind is worth reporting on either kind of answer, so the
	// hints are gated on the queue and not on the caller's hands. A caller who got
	// one message of three still needs to know why the other two are not theirs yet.
	// An empty queue costs exactly one key, as before.
	if w.Remaining > 0 {
		w.Waiting = m.waiting
		w.OldestS = m.oldestS
	}
	d.logPoll(m)
	return w
}

// emptyHanded builds the metadata for a pass that came back with no messages.
// The numbers describe what is STILL queued, so they are read after the drain
// released the lock: `Oldest` is a peek that costs the caller nothing, and the
// skip tally is the one from the most recent full scan (the only thing that can
// have changed since is a new arrival, which the next poll will describe).
//
// A queue that is actually empty gets nothing but the reason, which is why this
// is worth computing at all: the idle case stays one key and ~20 bytes.
func (d *Deps) emptyHanded(q *bus.Queue, st *drainState, reason, agent string, start time.Time, waitS float64, connected bool) pollMeta {
	m := pollMeta{
		reason: reason, agent: agent, waitedMS: durMS(time.Since(start)),
		maxWait: waitS, connected: connected,
	}
	if q == nil {
		return m
	}
	if s := q.Stats(); s.Size > 0 {
		m.remaining = s.Size
		m.dropped = s.Dropped
		if st != nil {
			m.waiting = st.renderHeld()
		}
		m.oldestS = oldestWaited(q)
	}
	return m
}

// oldestWaited answers "how long has the oldest thing in this queue been waiting",
// for a queue the caller is already done with. It is a peek and not a claim: the
// answer describes whoever happens to be queued, including messages this pass
// never looked at, so it need not line up with waiting's counts. nil-safe, because
// both the empty-handed path and the loaded one call it.
func oldestWaited(q *bus.Queue) float64 {
	if q == nil {
		return 0
	}
	e := q.Oldest()
	if e == nil || e.Ts.IsZero() {
		return 0
	}
	return round1(time.Since(e.Ts).Seconds())
}

// logPoll writes the one bookkeeping line a poll leaves behind: every number
// that used to ship to the model and is now only worth showing an operator.
func (d *Deps) logPoll(m pollMeta) {
	lg := d.debug()
	if !lg.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	fields := []any{"agent", m.agent, "reason", m.reason, "target", m.channel, "dm", m.dm,
		"waited_ms", m.waitedMS, "max_wait", m.maxWait, "connected", m.connected, "self", m.self,
		"count", m.count, "remaining", m.remaining, "partial", m.partial,
		"bytes", m.bytes, "dropped", m.dropped, "notes", len(m.notes)}
	if m.count > 0 {
		// The answer itself, so an operator can see what the model was handed
		// without turning the wire back into a struct.
		fields = append(fields, "result", wireMessages(m.messages))
	}
	lg.Debug("poll", fields...)
}

func queueDropped(q *bus.Queue) uint64 {
	if q == nil {
		return 0
	}
	return q.Stats().Dropped
}

// --- small helpers ---------------------------------------------------------

func (d *Deps) notePoll(agent string) {
	d.pollMu.Lock()
	if d.pollAt == nil {
		d.pollAt = map[string]time.Time{}
	}
	d.pollAt[agent] = time.Now()
	d.pollMu.Unlock()
}

func (d *Deps) lastPoll(agent string) (time.Time, bool) {
	d.pollMu.Lock()
	defer d.pollMu.Unlock()
	t, ok := d.pollAt[agent]
	return t, ok
}

// jitter spreads a deadline by 0..pct percent of itself. It only ever makes the
// wait later, never shorter, so a caller that asked for 45s always gets at
// least 45s.
func jitter(base time.Duration, pct int) time.Duration {
	if pct <= 0 || base <= 0 {
		return 0
	}
	if pct > 50 {
		pct = 50
	}
	span := int64(float64(base) * float64(pct) / 100.0)
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(span + 1))
}

func durMS(d time.Duration) int64 { return int64(d / time.Millisecond) }

func ftoa(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func round1(v float64) float64 { return math.Round(v*10) / 10 }
