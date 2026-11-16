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
// head and says how big it really was (text_full_bytes); if that is still not
// enough, trailing attachments are counted out (attachments_omitted). Metadata
// (ids, channel, author, timestamps, urls) is never trimmed — without it the
// agent could not answer what it just read.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
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

func pollTool() *mcp.Tool {
	return &mcp.Tool{
		Name:  "poll",
		Title: "Wait for new Discord messages",
		Description: "Wait until this agent's queue holds messages, return them, and remove them from the queue. " +
			"There is no cursor and no acknowledge step: what a poll hands you is yours, and what you do not take stays. " +
			"reason=messages means you got something; reason=timeout (ok:true) means nothing arrived within max_wait, call again; " +
			"reason=cancelled/shutdown means this answer was not delivered. " +
			"If partial is true, messages are still queued — poll again right away. " +
			"If dropped is present, messages were overwritten while nobody polled; poll more often. " +
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
					Description: "Only messages from these aliases (see discord_status routing.targets). " +
						"Omit to accept every target this agent may read.",
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

// pollMessage is the wire form of one queued event. It is a separate type from
// bus.Event on purpose: bus.Event is shared between agents and between polls
// (fan-out stores one pointer in several queues), so anything computed per call
// — trimmed text, media verdict — must live here and never back on the event.
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

// pollAttachment is the per-call view of an attachment: metadata plus how THIS
// call renders it. bus.Attachment stays shared and read-only.
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

	// Media is the verdict for this call. Until the fetcher lands it is always
	// url_only, with the configured policy reported next to it so nobody has to
	// guess: an agent must never believe it saw pixels it did not receive.
	Media  string `json:"media"`
	Policy string `json:"policy,omitempty"`

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
	connected := d.DC != nil && d.DC.Connected()

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
			return jsonResult(pollEnvelope(pollMeta{
				reason: "timeout", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
				maxWait: waitS, connected: connected,
			})), nil
		case <-ctx.Done():
			// Nothing deliverable was taken (msgs is empty), so there is nothing
			// to put back.
			return jsonResult(pollEnvelope(pollMeta{
				reason: "cancelled", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
				maxWait: waitS, connected: connected,
			})), nil
		case <-d.Stopping:
			return jsonResult(pollEnvelope(pollMeta{
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
		return jsonResult(pollEnvelope(pollMeta{
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

	var self string
	if d.DC != nil {
		snap := d.DC.Snapshot()
		self = snap.SelfName
		if self == "" {
			self = snap.SelfTag
		}
	}
	return jsonResult(pollEnvelope(pollMeta{
		reason: "messages", agent: p.AgentID, waitedMS: durMS(time.Since(start)),
		maxWait: waitS, connected: connected, self: self,
		count: len(msgs), remaining: remaining, partial: st.stopped,
		bytes: used, notes: notes, messages: msgs,
		dropped: queueDropped(q),
	})), nil
}

// --- filters ---------------------------------------------------------------

type pollFilter struct {
	targets     map[string]bool
	channels    map[string]bool
	from        map[string]bool
	mentions    bool
	ignoreBots  bool
	includeSelf bool
}

func (d *Deps) newFilter(p router.Principal, a *pollArgs) (*pollFilter, error) {
	f := &pollFilter{
		targets:    map[string]bool{},
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
		if !ru.Read {
			return nil, fmt.Errorf("target %q is read=false: this agent may not poll it", ref)
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
			out = append(out, "user:"+strings.ToLower(peer))
		}
	}
	if e.ChannelName != "" {
		out = append(out, strings.ToLower(e.ChannelName))
	}
	return out
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
	if len(f.targets) > 0 {
		hit := false
		for _, t := range eventTargets(e) {
			if f.targets[t] {
				hit = true
				break
			}
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

// --- rendering -------------------------------------------------------------

// buildMessage turns a queued event into its wire form for this call.
//
// The target is relabelled to the alias THIS principal knows, because that is
// the name it has to answer with, and a model should not have to remember
// snowflakes. bus.Event itself is never touched: the same pointer sits in other
// agents' queues, and their alias may be different (or none).
func (d *Deps) buildMessage(p router.Principal, e *bus.Event) pollMessage {
	target := e.Target
	if d.Router != nil {
		if alias := d.Router.AliasForTarget(p, eventTargets(e)...); alias != "" {
			target = alias
		}
	}
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
		policy := string(d.Cfg.MediaFor(a.Kind))
		// Hold back room for the payload the fetcher would attach to an inlinable
		// kind, so a 4-image message cannot sneak past the byte budget.
		est := estMetaAttachment
		if policy == string(config.MediaInline) && (a.Kind == bus.KindImage || a.Kind == bus.KindAudio) {
			est = estInlineAttachment
		}
		m.Attachments = append(m.Attachments, pollAttachment{
			ID: a.ID, Filename: a.Filename, MIME: a.ContentType, Kind: a.Kind,
			Size: a.Size, Width: a.Width, Height: a.Height, DurationSecs: a.DurationSecs,
			URL: a.URL, Media: "url_only", Policy: policy, est: est,
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
	b, err := json.Marshal(m)
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
			" 個（見 attachments_omitted；附件本身仍在 Discord 上，url 沒被裁）")
	}

	// 3. Embeds are the least useful thing to keep when room is that tight.
	for len(m.Embeds) > 0 && messageCost(*m) > room {
		m.Embeds = m.Embeds[:len(m.Embeds)-1]
	}
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

// pollEnvelope builds the reply. An empty answer stays tiny (six keys) because
// it is the common case on a quiet server, and every byte of it goes back into
// the model's context on every turn.
func pollEnvelope(m pollMeta) map[string]any {
	out := map[string]any{
		"ok":               true,
		"reason":           m.reason,
		"agent":            m.agent,
		"waited_ms":        m.waitedMS,
		"max_wait_applied": m.maxWait,
		"connected":        m.connected,
	}
	if m.count == 0 {
		return out
	}
	out["count"] = m.count
	out["remaining"] = m.remaining
	out["partial"] = m.partial
	out["bytes"] = m.bytes
	out["messages"] = m.messages
	if m.self != "" {
		out["self"] = m.self
	}
	if m.dropped > 0 {
		out["dropped"] = m.dropped
	}
	if len(m.notes) > 0 {
		out["notes"] = append([]string{}, m.notes...)
	}
	// result_bytes is the size of this exact JSON. Its own width depends on the
	// number it holds, so iterate to the fixed point (converges in ~3 passes).
	rb := 0
	for i := 0; i < 5; i++ {
		out["result_bytes"] = rb
		b, err := json.Marshal(out)
		if err != nil {
			delete(out, "result_bytes")
			return out
		}
		if len(b) == rb {
			break
		}
		rb = len(b)
	}
	return out
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
