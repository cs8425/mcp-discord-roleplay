// poll.go implements discord_poll: the blocking "give me what arrived" tool.
//
// The contract with the model is deliberately state-free. Messages sit in this
// agent's queue until a poll drains them; taking them out IS the acknowledgment,
// so there is no cursor to remember, no ack to send, and nothing to replay. A
// restart empties the queue (by design, see internal/bus), which is why
// catching up on older messages is discord_history's job, not poll's.
//
// Two limits keep a reply inside a model context window:
//
//   - limit: at most N messages per call, oldest first.
//   - --poll-max-bytes: the reply is built event by event and the drain stops at
//     the first event that would not fit, so the leftovers wait for the next poll
//     IN ORDER (bus.Stop). Nothing is skipped over and nothing is lost.
//
// If a single event is bigger than the whole budget it would sit at the head of
// the queue forever. Instead it is trimmed into the budget: the text keeps its
// head and a note says how big it really was; if that is still not enough,
// trailing attachments are counted out, again by note. What reaches the model is
// only what it needs in order to answer (see msgWire): target, author, the
// author's Discord id, text, timestamps, attachment urls. The author id is the
// one id that earns its bytes: it is what a to:"dm:<id>" reply is built from.
// Everything else went to the log.
//
// Media runs in a second phase, after the drain. Queue.Drain calls the verdict
// function under the queue's lock, and a download must not hold that lock: the
// gateway would stop being able to Push. So the budget decides which messages
// and which attachments this call keeps from metadata alone, and only those are
// fetched afterwards. Base64 consequently never shows up in the JSON answer (it
// lives in the result's content array, not in this JSON); the room it will take
// in the model's context is held back by the per-attachment estimate instead.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/media"
	"mcp-discord-roleplay/internal/router"
)

// Attachment cost estimates. A poll cannot know an inline image's real base64
// size before the fetcher downloads it, so it reserves a placeholder for every
// attachment the policy would inline and a smaller one for a metadata-only
// listing. The actual payload stays governed by --inline-max-count /
// --inline-max-bytes-image / --inline-max-total-bytes; the two budgets are
// separate on purpose.
const (
	estInlineAttachment = 1000
	estMetaAttachment   = 300
	estEmbed            = 200
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
			"There is no cursor and no acknowledge step: what a poll hands you is yours, and what you do not take stays. " +
			"reason=messages means you got something; reason=timeout (ok:true) means nothing arrived within max_wait, call again; " +
			"reason=disconnected means the gateway was not listening during that wait, so nothing could have arrived; " +
			"reason=cancelled/shutdown means this answer was not delivered. " +
			"If remaining is present, that many messages are still queued — poll again right away. " +
			"If dropped is present, messages were overwritten while nobody polled; poll more often. " +
			"author_id is the speaker's Discord id: quote it as to:\"dm:<id>\" to answer that person in DMs even when they have no alias. " +
			"Each attachment reports what it came as: media=base64 means the bytes are appended to this result as a content block (content_index names it), " +
			"media=file means they were saved and local_path says where, media=url means metadata only and reason says why. " +
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
					Description: "Only messages whose channel id or channel name matches (case-insensitive, leading # optional).",
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
					Type:        mcp.TypeBoolean,
					Description: "Skip messages authored by bots, this bot included (default true).",
				},
				"include_self": {
					Type:        mcp.TypeBoolean,
					Description: "Also return messages this bot sent (default false; implies ignore_bots=false).",
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
	IncludeSelf  *bool    `json:"include_self"`
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

	Attachments      []pollAttachment `json:"attachments,omitempty"`
	AttachmentsTotal int              `json:"attachments_total,omitempty"`
	AttachmentsOmit  int              `json:"attachments_omitted,omitempty"`
	Embeds           []bus.Embed      `json:"embeds,omitempty"`
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

	// est is the room this attachment holds back in the reply budget for the
	// payload it may grow into (an inline image is not in the JSON, so the JSON
	// length alone would under-count it). Per attachment on purpose: fitMessage
	// drops attachments from the tail, and the room has to go with them.
	// Unexported, so it never reaches the wire.
	est int
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
			return jsonResult(d.pollEnvelope(pollMeta{
				reason: "timeout", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
				maxWait: waitS, connected: connected,
			})), nil
		case <-ctx.Done():
			// Nothing deliverable was taken (msgs is empty), so there is nothing
			// to put back.
			return jsonResult(d.pollEnvelope(pollMeta{
				reason: "cancelled", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
				maxWait: waitS, connected: connected,
			})), nil
		case <-d.Stopping:
			return jsonResult(d.pollEnvelope(pollMeta{
				reason: "shutdown", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
				maxWait: waitS, connected: connected,
			})), nil
		}
	}

	// The caller may have gone away while we were rendering. A response nobody
	// reads would silently swallow those messages, so hand them back at the
	// oldest end, in order, for the next poll.
	if ctx.Err() != nil {
		q.RequeueFront(st.kept)
		return jsonResult(d.pollEnvelope(pollMeta{
			reason: "cancelled", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
			maxWait: waitS, connected: connected,
		})), nil
	}

	if st.stopped {
		// Only worth saying when this call was cut short: leftovers that this
		// poll's own filter rejects are none of the caller's business yet.
		notes = append(notes, "你的佇列裡還有 "+strconv.Itoa(remaining)+
			" 則（含可能的系統訊息），請立刻再 poll 一次")
	}
	d.notePoll(p.AgentID)

	// Second phase: the queue lock is gone, so this is where the bytes are
	// fetched. msgs is written through (it IS st.msgsOut), so the JSON built
	// below carries the verdicts, and the blocks go after it.
	blocks, mnotes := d.applyMedia(ctx, msgs, st.kept)
	notes = append(notes, mnotes...)

	// A caller that vanished during the downloads would not read this answer
	// either, so the messages go back instead of being swallowed. Anything
	// already saved stays on disk: the next poll reuses it rather than refetching.
	if ctx.Err() != nil {
		q.RequeueFront(st.kept)
		return jsonResult(d.pollEnvelope(pollMeta{
			reason: "cancelled", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
			maxWait: waitS, connected: connected,
		})), nil
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
		maxWait: waitS, connected: connected, self: self,
		count: len(msgs), remaining: remaining, partial: st.stopped,
		bytes: used, notes: notes, messages: msgs,
		dropped: queueDropped(q),
	}))
	// The JSON answer occupies content[0]; every media block lands after it, in
	// the order the messages were read. That is what makes content_index
	// addressable — and why inline base64 never counts towards the byte budget.
	res.Content = append(res.Content, blocks...)
	return res, nil
}

// applyMedia is the second half of a poll: it runs the media policy over the
// attachments this call actually kept and returns the content blocks they grew
// into, plus notes worth telling the model.
//
// kept is the drain's own record of which event produced which message, in the
// same order, so kept[i].Attachments is where the bytes for
// msgs[i].Attachments come from. fitMessage may have shortened that list, but
// only from the tail, so the two line up on a prefix — which is the whole point
// of splitting poll into two phases: an attachment trimmed out of the budget
// never costs a download.
func (d *Deps) applyMedia(ctx context.Context, msgs []pollMessage, kept []*bus.Event) ([]mcp.ContentPart, []string) {
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
			h := sess.Handle(ev.Attachments[j])
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
		notes = append(notes, "有附件存進了媒體快取：local_path 可以直接餵回 "+d.toolSend()+" 的 files[].path，"+
			"同一個附件再 poll 不會重抓")
	}
	if skipped > 0 {
		keys := make([]string, 0, len(reasons))
		for r := range reasons {
			keys = append(keys, r)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, r := range keys {
			parts = append(parts, r+"×"+strconv.Itoa(reasons[r]))
		}
		notes = append(notes, "有 "+strconv.Itoa(skipped)+" 個附件只給了 url（reason："+
			strings.Join(parts, "、")+"）；檔案還在 Discord 上，url 可以直接貼")
	}
	return sess.Content(), notes
}

// --- filters ---------------------------------------------------------------

type pollFilter struct {
	targets     map[string]bool
	kinds       map[string]bool // kinds opened by a wildcard rule ("dm:*", "channel:*")
	channels    map[string]bool
	from        map[string]bool
	mentions    bool
	ignoreBots  bool
	includeSelf bool
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
	if a.IncludeSelf != nil {
		f.includeSelf = *a.IncludeSelf
	}
	// Wanting your own messages while skipping bots is self-contradictory;
	// include_self wins because it can only be said on purpose.
	if f.includeSelf {
		f.ignoreBots = false
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
		if s != "" {
			f.channels[strings.ToLower(strings.TrimPrefix(s, "#"))] = true
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

func (f *pollFilter) keep(e *bus.Event) bool {
	if !f.includeSelf && e.Author.Self {
		return false
	}
	if f.ignoreBots && e.Author.Bot {
		return false
	}
	if f.mentions && !e.MentionsSelf {
		return false
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
			return false
		}
	}
	if len(f.channels) > 0 {
		hit := false
		for _, t := range []string{strings.ToLower(e.ChannelID), strings.ToLower(e.ChannelName)} {
			if t != "" && f.channels[t] {
				hit = true
				break
			}
		}
		if !hit {
			return false
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
			return false
		}
	}
	return true
}

// --- budgeted drain --------------------------------------------------------

// drainState carries one Drain attempt's budget accounting.
type drainState struct {
	limit  int
	budget int // bytes, 0 = uncapped
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
}

// want is the verdict function handed to Queue.Drain: it decides, strictly in
// queue order, what this call takes.
func (st *drainState) want(d *Deps, p router.Principal, f *pollFilter) func(*bus.Event) bus.Verdict {
	return func(e *bus.Event) bus.Verdict {
		if e.Kind == bus.KindSystem {
			return bus.Take // consume and discard: nobody should have to read these
		}
		if !f.keep(e) {
			return bus.Skip
		}
		if st.count >= st.limit {
			st.stopped = true
			st.noteLimit()
			return bus.Stop
		}
		m := d.buildMessage(p, e)
		cost := messageCost(m)
		if st.budget > 0 && st.used+cost > st.budget {
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
		st.msgsOut = append(st.msgsOut, m)
		st.kept = append(st.kept, e)
		st.used += cost
		st.count++
		return bus.Take
	}
}

// noteBudgetFull records, at most once per attempt, that the drain stopped
// because of the byte budget rather than because the queue was empty.
func (st *drainState) noteBudgetFull() {
	const msg = "後面的訊息超過這次回覆的位元組預算，已留在佇列，下一次 poll 會先拿到"
	for _, n := range st.notes {
		if n == msg {
			return
		}
	}
	st.notes = append(st.notes, msg)
}

// noteLimit records that this call reached its limit; the rest stay queued.
func (st *drainState) noteLimit() {
	const msg = "已達這次 poll 的 limit，剩下的還在佇列，請立刻再 poll 一次"
	for _, n := range st.notes {
		if n == msg {
			return
		}
	}
	st.notes = append(st.notes, msg)
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
		ID:               e.MessageID,
		Target:           target,
		ChannelID:        e.ChannelID,
		ChannelName:      e.ChannelName,
		DM:               e.DM,
		From:             e.Author,
		Ts:               e.Time,
		Text:             e.Text,
		TextTruncated:    e.Truncated,
		MentionsSelf:     e.MentionsSelf,
		Attachments:      make([]pollAttachment, 0, len(e.Attachments)),
		AttachmentsTotal: len(e.Attachments),
		Embeds:           e.Embeds,
	}
	if m.TextTruncated {
		// Already shortened on the way in (--text-max-runes). Report the length
		// we hold so a reader knows the original was longer still.
		m.TextFullBytes = len(e.Text)
	}
	if !e.Ts.IsZero() {
		m.AgeS = round1(time.Since(e.Ts).Seconds())
	}
	for _, a := range e.Attachments {
		// Hold back room for the payload the fetcher may attach to an inlinable
		// kind, so a 4-image message cannot sneak past the byte budget. Saved and
		// skipped attachments only grow by a path or a reason, which is what the
		// smaller estimate covers.
		est := estMetaAttachment
		if policy := string(d.Cfg.MediaFor(a.Kind)); policy == string(config.MediaInline) &&
			(a.Kind == bus.KindImage || a.Kind == bus.KindAudio) {
			est = estInlineAttachment
		}
		m.Attachments = append(m.Attachments, pollAttachment{
			ID: a.ID, Filename: a.Filename, MIME: a.ContentType, Kind: a.Kind,
			Size: a.Size, Width: a.Width, Height: a.Height, DurationSecs: a.DurationSecs,
			URL: a.URL, Media: media.MediaURL, est: est,
		})
	}
	return m
}

// messageCost is what this message contributes to the reply: its JSON, plus the
// payload room of the attachments it STILL lists, plus a flat cost per embed.
// The room is derived from the slice instead of stored beside it, because
// fitMessage shrinks that slice — an aggregate kept on the message would not
// shrink with it, and a message carrying many inlinable files could then never
// fit at all: every attachment omitted, not even one listed.
func messageCost(m pollMessage) int {
	b, err := json.Marshal(m.wire())
	if err != nil {
		// bus.Event is plain data, so this cannot realistically fail; be honest
		// and treat an unencodable message as huge so it never crowds out others.
		return math.MaxInt32
	}
	room := 0
	for _, a := range m.Attachments {
		room += a.est
	}
	return len(b) + room + len(m.Embeds)*estEmbed
}

// fitMessage shrinks m until it costs at most room bytes, in the order the
// package doc describes: text first, then trailing attachments, then embeds.
// Identifiers and URLs are never touched.
func fitMessage(m *pollMessage, room int, st *drainState) {
	if messageCost(*m) <= room {
		return
	}
	orig := len(m.Text)
	full := orig
	if m.TextFullBytes > full {
		full = m.TextFullBytes
	}

	// 1. Cut the text, keeping its head, on a rune boundary so the reply never
	// contains half a UTF-8 character.
	for i := 0; i < 64 && messageCost(*m) > room; i++ {
		r := []rune(m.Text)
		if len(r) == 0 {
			break
		}
		over := messageCost(*m) - room
		// 6 bytes per rune is the worst JSON escape (\u003c); dividing by it
		// makes the loop converge instead of shaving one character at a time.
		cut := len(r) - over/6 - 1
		if cut < 0 {
			cut = 0
		}
		m.Text = string(r[:cut])
		m.TextTruncated = true
		m.TextFullBytes = full
	}
	if m.TextTruncated && len(m.Text) < orig {
		st.notes = append(st.notes, "這一則超過 poll 的位元組預算，文字裁到 "+
			strconv.Itoa(len(m.Text))+" bytes（原本 "+strconv.Itoa(full)+
			" bytes）；要看完整內容等 discord_history")
	}

	// 2. Still too big: list fewer attachments and count what is left out.
	for len(m.Attachments) > 0 && messageCost(*m) > room {
		m.Attachments = m.Attachments[:len(m.Attachments)-1]
		m.AttachmentsOmit = m.AttachmentsTotal - len(m.Attachments)
	}
	if m.AttachmentsOmit > 0 {
		st.notes = append(st.notes, "附件只列到預算內，少了 "+strconv.Itoa(m.AttachmentsOmit)+
			" 個（附件本身仍在 Discord 上，url 沒被裁）")
	}

	// 3. Embeds are the least useful thing to keep when room is that tight.
	for len(m.Embeds) > 0 && messageCost(*m) > room {
		m.Embeds = m.Embeds[:len(m.Embeds)-1]
	}
}

// --- wire shapes -----------------------------------------------------------
//
// pollMessage above is the WORKING form: budget accounting, trimming and the
// media verdict all live on it, and it keeps fields the model never sees (its
// json tags are therefore dead weight kept only for readability). These types
// below are the only thing that gets marshalled, which buys three things: the
// budget cost of a message equals the bytes it actually ships (messageCost),
// bus.Author/bus.Embed shapes can no longer leak onto the wire, and adding a
// field to pollMessage costs the model nothing until it is deliberately added
// here too.

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
	Target       string      `json:"target,omitempty"`
	Author       string      `json:"author"`
	AuthorID     string      `json:"author_id,omitempty"`
	Bot          bool        `json:"bot,omitempty"`
	Self         bool        `json:"self,omitempty"`
	DM           bool        `json:"dm,omitempty"`
	Ts           string      `json:"ts,omitempty"`
	AgeS         float64     `json:"age_s,omitempty"`
	Text         string      `json:"text,omitempty"`
	TextCut      bool        `json:"text_truncated,omitempty"`
	MentionsSelf bool        `json:"mentions_self,omitempty"`
	Attachments  []attWire   `json:"attachments,omitempty"`
	Embeds       []embedWire `json:"embeds,omitempty"`
}

// wire projects the working form onto what the model receives.
func (m pollMessage) wire() msgWire {
	out := msgWire{
		Target:       m.Target,
		Author:       m.From.Name,
		AuthorID:     m.From.ID,
		Bot:          m.From.Bot,
		Self:         m.From.Self,
		DM:           m.DM,
		Ts:           m.Ts,
		AgeS:         m.AgeS,
		Text:         m.Text,
		TextCut:      m.TextTruncated,
		MentionsSelf: m.MentionsSelf,
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
}

// pollWire is the envelope, and it is the whole budget: a quiet poll costs two
// keys — the one thing a poll has to answer, plus whether it succeeded — and
// every other field only earns its place by being non-empty.
type pollWire struct {
	OK        bool      `json:"ok"`
	Reason    string    `json:"reason"`
	Messages  []msgWire `json:"messages,omitempty"`
	Remaining int       `json:"remaining,omitempty"` // absent means caught up
	Dropped   uint64    `json:"dropped,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
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
	w := pollWire{OK: true, Reason: reason}
	// Everything below rides with messages only: a queue that handed over
	// nothing has nothing left, nothing trimmed and nothing to explain.
	if m.count > 0 {
		w.Messages = wireMessages(m.messages)
		w.Remaining = m.remaining
		w.Dropped = m.dropped
		w.Notes = m.notes
	}
	d.logPoll(m)
	return w
}

// logPoll writes the one bookkeeping line a poll leaves behind: every number
// that used to ship to the model and is now only worth showing an operator.
func (d *Deps) logPoll(m pollMeta) {
	lg := d.debug()
	if !lg.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	fields := []any{"agent", m.agent, "reason", m.reason, "waited_ms", m.waitedMS,
		"max_wait", m.maxWait, "connected", m.connected, "self", m.self,
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
